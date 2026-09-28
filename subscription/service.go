package subscription

import (
	"fmt"
	"sync/atomic"
	"time"
)

// Clock 返回当前时间；测试可注入固定时钟。
type Clock func() time.Time

// Service 在事件溯源存储之上提供订阅套餐变更的全部用例。
// 所有判定与提交都在 Store 的单次原子事务（Mutate）内完成。
type Service struct {
	store *Store
	now   Clock
	seq   atomic.Uint64
}

// NewService 创建服务。clock 为 nil 时使用 time.Now。
func NewService(store *Store, clock Clock) *Service {
	if clock == nil {
		clock = time.Now
	}
	return &Service{store: store, now: clock}
}

func (s *Service) id(prefix string) string {
	n := s.seq.Add(1)
	return fmt.Sprintf("%s_%d_%d", prefix, s.now().UnixNano(), n)
}

// ---------------- 套餐配置 ----------------

// PlanConfigInput 创建套餐。
type PlanConfigInput struct {
	ID         string
	Name       string
	Cycle      string
	PriceCents int64
}

// ConfigurePlan 创建套餐及其首个价格版本（v1，立即生效）。
func (s *Service) ConfigurePlan(in PlanConfigInput) (*Plan, error) {
	if in.ID == "" {
		return nil, fmt.Errorf("%w: plan id required", ErrInvalidArgument)
	}
	if !validCycle(in.Cycle) {
		return nil, fmt.Errorf("%w: cycle must be one of daily/weekly/monthly/yearly", ErrInvalidArgument)
	}
	if in.PriceCents < 0 {
		return nil, fmt.Errorf("%w: price must be non-negative", ErrInvalidArgument)
	}
	now := s.now().UTC()
	plan := &Plan{
		ID:        in.ID,
		Name:      in.Name,
		Cycle:     in.Cycle,
		CreatedAt: now,
		Versions: []PlanVersion{{
			Version:    1,
			PriceCents: in.PriceCents,
			Effective:  now,
			CreatedAt:  now,
		}},
	}
	err := s.store.Mutate(func(v View) ([]Event, error) {
		if _, ok := v.Plan(in.ID); ok {
			return nil, fmt.Errorf("%w: plan %s", ErrAlreadyExists, in.ID)
		}
		return []Event{{Type: EvPlanConfigured, At: now, Plan: plan}}, nil
	})
	if err != nil {
		return nil, err
	}
	return plan, nil
}

// AddPlanPrice 追加一个新价格版本（调价）。历史版本不可变，已受理申请
// 继续使用冻结版本。effectiveAt 为零值时取当前时间。
func (s *Service) AddPlanPrice(planID string, priceCents int64, effectiveAt time.Time) (*PlanVersion, error) {
	if priceCents < 0 {
		return nil, fmt.Errorf("%w: price must be non-negative", ErrInvalidArgument)
	}
	now := s.now().UTC()
	if effectiveAt.IsZero() {
		effectiveAt = now
	} else {
		effectiveAt = effectiveAt.UTC()
	}
	ver := &PlanVersion{PriceCents: priceCents, Effective: effectiveAt, CreatedAt: now}
	err := s.store.Mutate(func(v View) ([]Event, error) {
		p, ok := v.Plan(planID)
		if !ok {
			return nil, fmt.Errorf("%w: plan %s", ErrPlanMissing, planID)
		}
		last := p.Versions[len(p.Versions)-1]
		if !effectiveAt.After(last.Effective) {
			return nil, fmt.Errorf("%w: new price effective time must be after last version %s", ErrInvalidArgument, last.Effective.Format(time.RFC3339))
		}
		ver.Version = last.Version + 1
		return []Event{{Type: EvPlanPriceAdded, At: now, PlanID: planID, Version: ver}}, nil
	})
	if err != nil {
		return nil, err
	}
	return ver, nil
}

// ---------------- 订阅 ----------------

// CreateSubscriptionInput 创建订阅。
type CreateSubscriptionInput struct {
	ID     string // 可空，自动生成
	PlanID string
}

// CreateSubscription 创建订阅、按套餐当前价格出首期账单。
func (s *Service) CreateSubscription(in CreateSubscriptionInput) (*Subscription, *Bill, error) {
	now := s.now().UTC()
	subID := in.ID
	if subID == "" {
		subID = s.id("sub")
	}
	var sub *Subscription
	var bill *Bill
	err := s.store.Mutate(func(v View) ([]Event, error) {
		if _, ok := v.Subscription(subID); ok {
			return nil, fmt.Errorf("%w: subscription %s", ErrAlreadyExists, subID)
		}
		plan, ok := v.Plan(in.PlanID)
		if !ok {
			return nil, fmt.Errorf("%w: plan %s", ErrPlanMissing, in.PlanID)
		}
		pv := plan.CurrentVersion(now)
		if pv == nil {
			return nil, fmt.Errorf("%w: plan %s has no effective price", ErrInvalidArgument, in.PlanID)
		}
		snap := snapshotOf(plan, pv)
		sub = &Subscription{
			ID:          subID,
			Status:      SubActive,
			Plan:        snap,
			PeriodStart: now,
			PeriodEnd:   AddBillingCycle(now, plan.Cycle),
			CreatedAt:   now,
		}
		bill = &Bill{
			ID:             s.id("bill"),
			SubscriptionID: subID,
			Kind:           BillKindInitial,
			Direction:      BillCharge,
			AmountCents:    pv.PriceCents,
			Description:    fmt.Sprintf("套餐 %s 首期费用（版本 v%d）", plan.Name, pv.Version),
			CreatedAt:      now,
		}
		return []Event{
			{Type: EvSubCreated, At: now, Sub: sub},
			{Type: EvBillWritten, At: now, Bill: bill, DedupKey: "initial:" + subID},
		}, nil
	})
	if err != nil {
		return nil, nil, err
	}
	return sub, bill, nil
}

// Terminate 终止订阅。若存在待生效变更，在同一原子事务内将其取消：
// 终止后任何到期推进都不能再切换套餐。
func (s *Service) Terminate(subscriptionID string) error {
	now := s.now().UTC()
	return s.store.Mutate(func(v View) ([]Event, error) {
		sub, ok := v.Subscription(subscriptionID)
		if !ok {
			return nil, fmt.Errorf("%w: subscription %s", ErrNotFound, subscriptionID)
		}
		if sub.Status == SubTerminated {
			return nil, fmt.Errorf("%w: subscription %s", ErrSubscriptionTerminated, subscriptionID)
		}
		events := []Event{{Type: EvSubTerminated, At: now, SubID: subscriptionID}}
		if pc, ok := v.PendingChange(subscriptionID); ok {
			events = append(events, Event{
				Type: EvChangeCancelled, At: now,
				Change: cancelledCopy(pc, now),
			})
		}
		return events, nil
	})
}

// GetSubscription 查询订阅。
func (s *Service) GetSubscription(id string) (*Subscription, error) {
	var out *Subscription
	err := s.store.View(func(v View) error {
		sub, ok := v.Subscription(id)
		if !ok {
			return fmt.Errorf("%w: subscription %s", ErrNotFound, id)
		}
		out = sub
		return nil
	})
	return out, err
}

// ListPlans 返回全部套餐。
func (s *Service) ListPlans() ([]*Plan, error) {
	var out []*Plan
	err := s.store.View(func(v View) error { out = v.Plans(); return nil })
	return out, err
}

// ---------------- 变更申请 ----------------

// ApplyChangeInput 变更申请。
type ApplyChangeInput struct {
	SubscriptionID string
	ExternalID     string // 外部申请号，用于幂等/冲突判定，可空
	ToPlanID       string
	EffectiveMode  string // immediate | next_cycle
}

// ApplyChange 受理变更申请：冻结原套餐/新套餐/计费周期/价格版本，并用精确
// 金额计算应补或应退差额。immediate 在同一原子事务内立即结算生效；
// next_cycle 等待到期推进。同一订阅只允许一笔待生效变更。
func (s *Service) ApplyChange(in ApplyChangeInput) (*ChangeRequest, *AdvanceResult, error) {
	now := s.now().UTC()
	var out *ChangeRequest
	var adv *AdvanceResult
	err := s.store.Mutate(func(v View) ([]Event, error) {
		ch, planTo, events, err := s.buildChange(v, buildArgs{
			subID:      in.SubscriptionID,
			externalID: in.ExternalID,
			toPlanID:   in.ToPlanID,
			mode:       in.EffectiveMode,
			replacing:  nil,
			now:        now,
		})
		if err == errIdempotentReplay {
			// 相同外部号 + 相同参数：原样返回既有申请（与既有账单/通知），不重复出账。
			out = ch
			adv = existingResult(v, ch)
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		// 受理事件持有独立的 pending 副本；buildSettle 在另一份副本上标记
		// effective，保证 WAL 中“受理”与“生效”事件各自表达真实状态。
		pending := *ch
		events = append(events, Event{Type: EvChangeApplied, At: now, Change: &pending})
		// immediate：受理即原子结算（切换+账单+通知），杜绝半生效状态。
		if ch.EffectiveMode == EffectiveImmediate {
			settleEvents, result, err := s.buildSettle(v, ch, planTo, now)
			if err != nil {
				return nil, err
			}
			events = append(events, settleEvents...)
			adv = result
		}
		out = ch
		return events, nil
	})
	if err != nil {
		return nil, nil, err
	}
	return out, adv, nil
}

// ReplaceChangeInput 用当前价格/周期版本替换待生效变更。
type ReplaceChangeInput struct {
	SubscriptionID  string
	ChangeID        string // 待替换的变更 ID（必须是当前待生效版本，否则冲突）
	ToPlanID        string
	EffectiveMode   string
	ExpectedVersion *int   // 乐观锁：期望替换的待生效版本，nil 不检查
	ExternalID      string // 新外部申请号，空则沿用原号
}

// ReplaceChange 替换待生效变更：新版本号递增，旧版本置为 superseded
// （旧版本任务永远不能再次收费）。变更 ID 不是当前待生效版本、或期望版本
// 不匹配都返回 ErrVersionMismatch/ErrConflict，并发修改不会静默覆盖。
func (s *Service) ReplaceChange(in ReplaceChangeInput) (*ChangeRequest, *AdvanceResult, error) {
	now := s.now().UTC()
	var out *ChangeRequest
	var adv *AdvanceResult
	err := s.store.Mutate(func(v View) ([]Event, error) {
		pending, ok := v.PendingChange(in.SubscriptionID)
		if !ok {
			return nil, fmt.Errorf("%w: no pending change for subscription %s", ErrNotFound, in.SubscriptionID)
		}
		if in.ChangeID != "" && in.ChangeID != pending.ID {
			if target, exists := v.Change(in.ChangeID); exists {
				return nil, fmt.Errorf("%w: change %s is %s, current pending is v%d", ErrVersionMismatch, in.ChangeID, target.Status, pending.Version)
			}
			return nil, fmt.Errorf("%w: change %s", ErrNotFound, in.ChangeID)
		}
		if in.ExpectedVersion != nil && *in.ExpectedVersion != pending.Version {
			return nil, fmt.Errorf("%w: expected v%d but current is v%d", ErrVersionMismatch, *in.ExpectedVersion, pending.Version)
		}
		if in.ExternalID != "" {
			if in.ExternalID == pending.ExternalID {
				// 同号同参：幂等返回既有待生效版本；同号但目标/方式不同：冲突。
				if in.ToPlanID != pending.ToPlan.PlanID || in.EffectiveMode != pending.EffectiveMode {
					return nil, fmt.Errorf("%w: external id %s already accepted with different target/mode", ErrConflict, in.ExternalID)
				}
				out = pending
				return nil, nil
			}
			if other, ok := v.ChangeByExternal(in.ExternalID); ok {
				return nil, fmt.Errorf("%w: external id %s already used by change %s", ErrConflict, in.ExternalID, other.ID)
			}
		}
		ch, planTo, buildEvents, err := s.buildChange(v, buildArgs{
			subID:      in.SubscriptionID,
			externalID: in.ExternalID,
			toPlanID:   in.ToPlanID,
			mode:       in.EffectiveMode,
			replacing:  pending,
			now:        now,
		})
		if err != nil {
			return nil, err
		}
		if len(buildEvents) != 0 {
			return nil, fmt.Errorf("%w: internal: replacement must not emit events", ErrConflict)
		}
		pendingNew := *ch
		events := []Event{{
			Type:        EvChangeReplaced,
			At:          now,
			OldChangeID: pending.ID,
			Change:      &pendingNew,
		}}
		if ch.EffectiveMode == EffectiveImmediate {
			settleEvents, result, err := s.buildSettle(v, ch, planTo, now)
			if err != nil {
				return nil, err
			}
			events = append(events, settleEvents...)
			adv = result
		}
		out = ch
		return events, nil
	})
	if err != nil {
		return nil, nil, err
	}
	return out, adv, nil
}

// CancelChangeInput 取消待生效变更。
type CancelChangeInput struct {
	SubscriptionID  string
	ChangeID        string // 可选：指定取消的变更 ID；必须是当前待生效版本
	ExpectedVersion *int
}

// CancelChange 取消待生效变更。已经生效的变更不能被迟到取消还原
// （返回 ErrConflict）；变更 ID 不是当前待生效版本或期望版本不匹配
// 返回 ErrVersionMismatch/ErrConflict。
func (s *Service) CancelChange(in CancelChangeInput) error {
	now := s.now().UTC()
	return s.store.Mutate(func(v View) ([]Event, error) {
		sub, ok := v.Subscription(in.SubscriptionID)
		if !ok {
			return nil, fmt.Errorf("%w: subscription %s", ErrNotFound, in.SubscriptionID)
		}
		if sub.Status == SubTerminated {
			return nil, fmt.Errorf("%w: subscription %s", ErrSubscriptionTerminated, in.SubscriptionID)
		}
		pending, ok := v.PendingChange(in.SubscriptionID)
		if !ok {
			// 待生效不存在：若指定的变更刚被推进生效，明确拒绝“迟到取消”。
			if in.ChangeID != "" {
				if c, exists := v.Change(in.ChangeID); exists && c.Status == ChangeEffective {
					return nil, fmt.Errorf("%w: change %s already effective, late cancel rejected", ErrConflict, in.ChangeID)
				}
			}
			if last := lastChange(v, in.SubscriptionID); last != nil && last.Status == ChangeEffective {
				return nil, fmt.Errorf("%w: change %s already effective, late cancel rejected", ErrConflict, last.ID)
			}
			return nil, fmt.Errorf("%w: no pending change for subscription %s", ErrNotFound, in.SubscriptionID)
		}
		if in.ChangeID != "" && in.ChangeID != pending.ID {
			if target, exists := v.Change(in.ChangeID); exists {
				return nil, fmt.Errorf("%w: change %s is %s, current pending is v%d", ErrVersionMismatch, in.ChangeID, target.Status, pending.Version)
			}
			return nil, fmt.Errorf("%w: change %s", ErrNotFound, in.ChangeID)
		}
		if in.ExpectedVersion != nil && *in.ExpectedVersion != pending.Version {
			return nil, fmt.Errorf("%w: expected v%d but current is v%d", ErrVersionMismatch, *in.ExpectedVersion, pending.Version)
		}
		return []Event{{Type: EvChangeCancelled, At: now, Change: cancelledCopy(pending, now)}}, nil
	})
}

// ---------------- 到期推进 ----------------

// AdvanceResult 推进结果；Advanced=false 表示申请此前已生效（幂等重放）。
type AdvanceResult struct {
	Change       *ChangeRequest
	Bill         *Bill // 零差额时可能为 nil
	Notification *Notification
	Advanced     bool
}

// AdvanceChange 原子推进指定变更：切换套餐、写入差额/续费账单、生成唯一通知。
// 多个执行者同时处理同一申请只生效一次（重复调用返回原结果，不再次收费）；
// superseded 的旧版本任务调用返回 ErrConflict，不会再次收费；
// 未到 ScheduledAt 返回 ErrChangeNotDue；终止后的订阅不能切换。
func (s *Service) AdvanceChange(changeID string) (*AdvanceResult, error) {
	now := s.now().UTC()
	var result *AdvanceResult
	err := s.store.Mutate(func(v View) ([]Event, error) {
		ch, ok := v.Change(changeID)
		if !ok {
			return nil, fmt.Errorf("%w: change %s", ErrNotFound, changeID)
		}
		sub, ok := v.Subscription(ch.SubscriptionID)
		if !ok {
			return nil, fmt.Errorf("%w: subscription %s", ErrNotFound, ch.SubscriptionID)
		}
		// 已生效：幂等返回既有账单/通知，绝不二次收费。
		if ch.Status == ChangeEffective {
			result = existingResult(v, ch)
			return nil, nil
		}
		if sub.Status == SubTerminated {
			return nil, fmt.Errorf("%w: subscription %s terminated, change %s will not take effect", ErrSubscriptionTerminated, sub.ID, changeID)
		}
		if ch.Status != ChangePending {
			return nil, fmt.Errorf("%w: change %s is %s", ErrConflict, changeID, ch.Status)
		}
		if sub.PendingChangeID != ch.ID {
			// 该版本已被替换但状态投影异常时的防御路径。
			return nil, fmt.Errorf("%w: change %s superseded, only the current pending version may take effect", ErrConflict, changeID)
		}
		if ch.ScheduledAt.After(now) {
			return nil, fmt.Errorf("%w: change %s scheduled at %s", ErrChangeNotDue, changeID, ch.ScheduledAt.Format(time.RFC3339))
		}
		toPlan, _ := v.Plan(ch.ToPlan.PlanID)
		events, r, err := s.buildSettle(v, ch, toPlan, now)
		if err != nil {
			return nil, err
		}
		result = r
		return events, nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// AdvanceDue 推进订阅已到期的待生效变更；没有待生效变更返回 (nil, nil)，
// 未到期返回 ErrChangeNotDue。
func (s *Service) AdvanceDue(subscriptionID string) (*AdvanceResult, error) {
	var pendingID string
	err := s.store.View(func(v View) error {
		if pc, ok := v.PendingChange(subscriptionID); ok {
			pendingID = pc.ID
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if pendingID == "" {
		return nil, nil
	}
	return s.AdvanceChange(pendingID)
}

// buildSettle 构造推进事件组（账单 + 唯一通知 + 生效），并同步给出返回值。
// 调用方必须保证 ch 为当前待生效版本且已到期。
func (s *Service) buildSettle(v View, ch *ChangeRequest, toPlan *Plan, now time.Time) ([]Event, *AdvanceResult, error) {
	var bill *Bill
	events := make([]Event, 0, 3)

	switch ch.EffectiveMode {
	case EffectiveImmediate:
		if ch.DiffCents != 0 {
			dir := BillCharge
			amount := ch.DiffCents
			if amount < 0 {
				dir = BillRefund
				amount = -amount
			}
			bill = &Bill{
				ID:             s.id("bill"),
				SubscriptionID: ch.SubscriptionID,
				ChangeID:       ch.ID,
				Kind:           BillKindProration,
				Direction:      dir,
				AmountCents:    amount,
				Description: fmt.Sprintf(
					"套餐 %s(v%d) → %s(v%d) 立即变更差额（周期 %s ~ %s，冻结版本）",
					ch.FromPlan.PlanName, ch.FromPlan.Version,
					ch.ToPlan.PlanName, ch.ToPlan.Version,
					ch.Period.Start.Format("2006-01-02"), ch.Period.End.Format("2006-01-02")),
				CreatedAt: now,
			}
			events = append(events, Event{Type: EvBillWritten, At: now, Bill: bill, DedupKey: "change-bill:" + ch.ID})
		}
	case EffectiveNextCycle:
		// 下一周期边界生效：按受理时冻结的新套餐价格出整周期续费账单。
		bill = &Bill{
			ID:             s.id("bill"),
			SubscriptionID: ch.SubscriptionID,
			ChangeID:       ch.ID,
			Kind:           BillKindRenewal,
			Direction:      BillCharge,
			AmountCents:    ch.ToPlan.PriceCents,
			Description: fmt.Sprintf("套餐变更为 %s(v%d) 后首个周期费用（申请时冻结价格）",
				ch.ToPlan.PlanName, ch.ToPlan.Version),
			CreatedAt: now,
		}
		events = append(events, Event{Type: EvBillWritten, At: now, Bill: bill, DedupKey: "change-bill:" + ch.ID})
	default:
		return nil, nil, fmt.Errorf("%w: unknown effective mode %s", ErrInvalidArgument, ch.EffectiveMode)
	}

	if bill != nil {
		ch.BillID = bill.ID
	}
	n := &Notification{
		ID:             s.id("ntf"),
		SubscriptionID: ch.SubscriptionID,
		ChangeID:       ch.ID,
		Type:           "change_effective",
		Message:        fmt.Sprintf("变更 %s 已生效：套餐切换为 %s（v%d）", ch.ID, ch.ToPlan.PlanName, ch.ToPlan.Version),
		CreatedAt:      now,
	}
	ch.NotificationID = n.ID
	effAt := now
	ch.Status = ChangeEffective
	ch.EffectiveAt = &effAt

	dedup := "change-effective:" + ch.ID
	events = append(events,
		Event{Type: EvNotificationSent, At: now, Notify: n, DedupKey: dedup},
		Event{Type: EvChangeEffective, At: now, Change: ch},
	)
	return events, &AdvanceResult{Change: ch, Bill: bill, Notification: n, Advanced: true}, nil
}

type buildArgs struct {
	subID      string
	externalID string
	toPlanID   string
	mode       string
	replacing  *ChangeRequest // 非 nil 表示替换
	now        time.Time
}

// buildChange 执行申请/替换的校验并构造冻结的 ChangeRequest（不含 applied/replaced 事件）。
func (s *Service) buildChange(v View, a buildArgs) (*ChangeRequest, *Plan, []Event, error) {
	if a.mode != EffectiveImmediate && a.mode != EffectiveNextCycle {
		return nil, nil, nil, fmt.Errorf("%w: effective_mode must be immediate or next_cycle", ErrInvalidArgument)
	}
	sub, ok := v.Subscription(a.subID)
	if !ok {
		return nil, nil, nil, fmt.Errorf("%w: subscription %s", ErrNotFound, a.subID)
	}
	if sub.Status == SubTerminated {
		return nil, nil, nil, fmt.Errorf("%w: subscription %s", ErrSubscriptionTerminated, a.subID)
	}
	toPlan, ok := v.Plan(a.toPlanID)
	if !ok {
		return nil, nil, nil, fmt.Errorf("%w: plan %s", ErrPlanMissing, a.toPlanID)
	}
	fromPlan, ok := v.Plan(sub.Plan.PlanID)
	if !ok {
		return nil, nil, nil, fmt.Errorf("%w: current plan %s", ErrPlanMissing, sub.Plan.PlanID)
	}
	fromPV := fromPlan.VersionAt(a.now)
	toPV := toPlan.VersionAt(a.now)
	if fromPV == nil || toPV == nil {
		return nil, nil, nil, fmt.Errorf("%w: plan has no effective price at apply time", ErrInvalidArgument)
	}

	if a.replacing == nil {
		// 外部申请号优先判定：相同号 + 相同目标/生效方式 = 幂等重放；
		// 相同号 + 不同参数 = 冲突。必须先于“仅一笔待生效”约束，
		// 否则同号重试会被误判为第二笔待生效。
		if a.externalID != "" {
			if ex, ok := v.ChangeByExternal(a.externalID); ok {
				sameIntent := ex.SubscriptionID == a.subID && ex.ToPlan.PlanID == a.toPlanID && ex.EffectiveMode == a.mode
				if sameIntent && ex.Status == ChangePending {
					return ex, toPlan, nil, errIdempotentReplay
				}
				if sameIntent {
					return nil, nil, nil, fmt.Errorf("%w: external id %s already %s as change %s",
						ErrConflict, a.externalID, ex.Status, ex.ID)
				}
				return nil, nil, nil, fmt.Errorf("%w: external id %s already used by change %s with different target/mode",
					ErrConflict, a.externalID, ex.ID)
			}
		}
		// 同一订阅只能保留一笔待生效变更。
		if pc, ok := v.PendingChange(a.subID); ok {
			return nil, nil, nil, fmt.Errorf("%w: subscription %s has pending change %s (v%d); replace or cancel it first",
				ErrPendingChangeExists, a.subID, pc.ID, pc.Version)
		}
	}

	// 冻结计费周期（使用订阅当前周期窗口）。
	period := CycleSnapshot{Start: sub.PeriodStart, End: sub.PeriodEnd}

	ch := &ChangeRequest{
		ID:             s.id("chg"),
		SubscriptionID: a.subID,
		ExternalID:     a.externalID,
		EffectiveMode:  a.mode,
		FromPlan:       snapshotOf(fromPlan, fromPV),
		ToPlan:         snapshotOf(toPlan, toPV),
		Period:         period,
		AppliedAt:      a.now,
		Status:         ChangePending,
	}
	if a.replacing != nil {
		ch.Version = a.replacing.Version + 1
	} else if history := v.ChangesBySubscription(a.subID); len(history) > 0 {
		ch.Version = history[len(history)-1].Version + 1
	} else {
		ch.Version = 1
	}

	if a.mode == EffectiveImmediate {
		total := wholeDays(period.Start, period.End)
		remain := wholeDays(a.now, period.End)
		if total <= 0 {
			return nil, nil, nil, fmt.Errorf("%w: frozen billing cycle has non-positive length", ErrInvalidArgument)
		}
		if remain <= 0 {
			return nil, nil, nil, fmt.Errorf("%w: current billing cycle ended at %s; use next_cycle", ErrInvalidArgument, period.End.Format(time.RFC3339))
		}
		used := total - remain
		diff := proration(fromPV.PriceCents, toPV.PriceCents, used, remain, total)
		ch.DiffCents = diff
		if diff > 0 {
			ch.DiffDirection = BillCharge
		} else if diff < 0 {
			ch.DiffDirection = BillRefund
		}
		ch.ScheduledAt = a.now
	} else {
		// next_cycle：当前周期费用不重算，新周期整期费用在推进时按冻结价格出账。
		ch.ScheduledAt = period.End
	}
	return ch, toPlan, nil, nil
}

// errIdempotentReplay 是事务内部信号：相同外部申请号原样重放，直接返回原申请。
var errIdempotentReplay = fmt.Errorf("idempotent replay")

func existingResult(v View, ch *ChangeRequest) *AdvanceResult {
	r := &AdvanceResult{Change: ch, Advanced: false}
	if b, ok := v.BillByChange(ch.ID); ok {
		r.Bill = b
	}
	if n, ok := v.NotificationByDedup("change-effective:" + ch.ID); ok {
		r.Notification = n
	}
	return r
}

func lastChange(v View, subID string) *ChangeRequest {
	h := v.ChangesBySubscription(subID)
	if len(h) == 0 {
		return nil
	}
	return h[len(h)-1]
}

func cancelledCopy(c *ChangeRequest, at time.Time) *ChangeRequest {
	cp := *c
	cp.Status = ChangeCancelled
	cp.CancelledAt = &at
	return &cp
}

func validCycle(c string) bool {
	switch c {
	case CycleDaily, CycleWeekly, CycleMonthly, CycleYearly:
		return true
	}
	return false
}

// ---------------- 查询 ----------------

// GetChange 查询变更申请。
func (s *Service) GetChange(id string) (*ChangeRequest, error) {
	var out *ChangeRequest
	err := s.store.View(func(v View) error {
		ch, ok := v.Change(id)
		if !ok {
			return fmt.Errorf("%w: change %s", ErrNotFound, id)
		}
		out = ch
		return nil
	})
	return out, err
}

// ListChanges 返回订阅的全部变更历史（含已生效/取消/替换版本，按版本顺序）。
func (s *Service) ListChanges(subscriptionID string) ([]*ChangeRequest, error) {
	var out []*ChangeRequest
	err := s.store.View(func(v View) error {
		if _, ok := v.Subscription(subscriptionID); !ok {
			return fmt.Errorf("%w: subscription %s", ErrNotFound, subscriptionID)
		}
		out = v.ChangesBySubscription(subscriptionID)
		return nil
	})
	return out, err
}

// GetBill 查询账单。
func (s *Service) GetBill(id string) (*Bill, error) {
	var out *Bill
	err := s.store.View(func(v View) error {
		b, ok := v.Bill(id)
		if !ok {
			return fmt.Errorf("%w: bill %s", ErrNotFound, id)
		}
		out = b
		return nil
	})
	return out, err
}

// ListBills 返回订阅账单（按时间顺序）。
func (s *Service) ListBills(subscriptionID string) ([]*Bill, error) {
	var out []*Bill
	err := s.store.View(func(v View) error {
		if _, ok := v.Subscription(subscriptionID); !ok {
			return fmt.Errorf("%w: subscription %s", ErrNotFound, subscriptionID)
		}
		out = v.BillsBySubscription(subscriptionID)
		return nil
	})
	return out, err
}
