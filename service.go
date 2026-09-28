package gosubscriptionchange

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Service 在原子仓储之上实现订阅套餐的预约变更与生效结算。
// 所有用例都在单个 Update 事务内完成：任何一步校验失败，整笔回滚。
type Service struct {
	store *Store
}

// NewService 创建服务。
func NewService(store *Store) *Service {
	return &Service{store: store}
}

// ---------------- 套餐配置 ----------------

// CreatePlanInput 创建套餐入参。
type CreatePlanInput struct {
	ID          string    // 可空，空则自动生成
	Name        string    // 必填
	Currency    string    // 必填，ISO-4217，如 CNY
	Price       int64     // 必填，首期价格（>=0，最小货币单位）
	EffectiveAt time.Time // 可空，空则取当前时间
}

// CreatePlan 创建套餐并发布价格版本 v1。
func (s *Service) CreatePlan(ctx context.Context, in CreatePlanInput) (*Plan, error) {
	if in.Name == "" || in.Currency == "" {
		return nil, invalidArgf("plan name and currency are required")
	}
	if in.Price < 0 {
		return nil, invalidArgf("price must be non-negative")
	}
	now := clockFrom(ctx).Now()
	eff := in.EffectiveAt
	if eff.IsZero() {
		eff = now
	}
	var out *Plan
	err := s.store.Update(func(tx *Tx) error {
		if in.ID != "" {
			if _, ok := tx.getPlan(in.ID); ok {
				return fmt.Errorf("%w: plan %s", ErrAlreadyExists, in.ID)
			}
		}
		id := in.ID
		if id == "" {
			id = tx.newID("plan")
		}
		p := &Plan{
			ID: id, Name: in.Name, Currency: in.Currency,
			Prices:    []PriceVersion{{Version: 1, Amount: in.Price, EffectiveAt: eff.UTC()}},
			CreatedAt: now,
		}
		tx.putPlan(p)
		out, _ = tx.getPlan(id)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// AddPlanPrice 为套餐追加一个不可变的新价格版本。
// 生效时间必须严格晚于上一版本；新版本不影响已受理的申请。
func (s *Service) AddPlanPrice(ctx context.Context, planID string, amount int64, effectiveAt time.Time) (*PriceVersion, error) {
	if amount < 0 {
		return nil, invalidArgf("price must be non-negative")
	}
	if effectiveAt.IsZero() {
		effectiveAt = clockFrom(ctx).Now()
	}
	effectiveAt = effectiveAt.UTC()
	var out *PriceVersion
	err := s.store.Update(func(tx *Tx) error {
		p, ok := tx.getPlan(planID)
		if !ok {
			return notFoundf("plan %s", planID)
		}
		last, _ := p.CurrentPrice()
		if !effectiveAt.After(last.EffectiveAt) {
			return invalidArgf("new price effectiveAt must be after %s", last.EffectiveAt)
		}
		nv := PriceVersion{Version: last.Version + 1, Amount: amount, EffectiveAt: effectiveAt}
		p.Prices = append(p.Prices, nv)
		tx.putPlan(p)
		v := nv
		out = &v
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetPlan 查询套餐配置（含全部价格版本）。
func (s *Service) GetPlan(ctx context.Context, planID string) (*Plan, error) {
	var out *Plan
	err := s.store.View(func(tx *Tx) error {
		p, ok := tx.getPlan(planID)
		if !ok {
			return notFoundf("plan %s", planID)
		}
		out = p
		return nil
	})
	return out, err
}

// ListPlans 列出全部套餐。
func (s *Service) ListPlans(ctx context.Context) ([]*Plan, error) {
	var out []*Plan
	err := s.store.View(func(tx *Tx) error {
		out = tx.listPlans()
		return nil
	})
	return out, err
}

// ---------------- 订阅 ----------------

// CreateSubscriptionInput 创建订阅入参。
type CreateSubscriptionInput struct {
	ID     string // 可空
	PlanID string // 必填，首期套餐
}

// CreateSubscription 以当前价格开通订阅，计费周期自开通时刻起一个自然月。
func (s *Service) CreateSubscription(ctx context.Context, in CreateSubscriptionInput) (*Subscription, error) {
	if in.PlanID == "" {
		return nil, invalidArgf("planId is required")
	}
	now := clockFrom(ctx).Now()
	var out *Subscription
	err := s.store.Update(func(tx *Tx) error {
		p, ok := tx.getPlan(in.PlanID)
		if !ok {
			return notFoundf("plan %s", in.PlanID)
		}
		if _, ok := p.priceAt(now); !ok {
			return invalidArgf("plan %s has no price effective at %s", in.PlanID, now)
		}
		if in.ID != "" {
			if _, ok := tx.getSubscription(in.ID); ok {
				return fmt.Errorf("%w: subscription %s", ErrAlreadyExists, in.ID)
			}
		}
		id := in.ID
		if id == "" {
			id = tx.newID("sub")
		}
		start := dayStart(now)
		sub := &Subscription{
			ID:            id,
			CurrentPlanID: p.ID,
			Currency:      p.Currency,
			PeriodStart:   start,
			PeriodEnd:     nextMonthlyBoundary(start),
			Status:        SubActive,
			CreatedAt:     now,
		}
		if err := tx.saveSubscription(sub, 0); err != nil {
			return err
		}
		tx.appendEvent(&Event{SubscriptionID: id, Type: EventSubCreated,
			Detail: fmt.Sprintf(`{"planId":%q}`, p.ID), CreatedAt: now})
		out, _ = tx.getSubscription(id)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetSubscription 查询订阅。
func (s *Service) GetSubscription(ctx context.Context, id string) (*Subscription, error) {
	var out *Subscription
	err := s.store.View(func(tx *Tx) error {
		sub, ok := tx.getSubscription(id)
		if !ok {
			return notFoundf("subscription %s", id)
		}
		out = sub
		return nil
	})
	return out, err
}

// TerminateSubscription 终止订阅。终止是终态：
//   - 仍处于 PENDING 的变更随订阅关闭（TERMINATED），不再可能生效；
//   - 终止后到达的推进被拒绝，套餐不会再切换；
//   - 与生效/取消并发时，以先提交的事务为准，结果确定。
//
// 重复终止幂等返回当前状态。
func (s *Service) TerminateSubscription(ctx context.Context, id string) (*Subscription, error) {
	now := clockFrom(ctx).Now()
	var out *Subscription
	err := s.store.Update(func(tx *Tx) error {
		sub, ok := tx.getSubscription(id)
		if !ok {
			return notFoundf("subscription %s", id)
		}
		if sub.Status == SubTerminated {
			out = sub // 幂等
			return nil
		}
		subV := sub.Version
		if sub.PendingChangeID != "" {
			ch, ok := tx.getChange(sub.PendingChangeID)
			if !ok {
				return notFoundf("pending change %s", sub.PendingChangeID)
			}
			if ch.Status == ChangePending {
				ch.Status = ChangeTerminated
				ch.DecidedAt = now
				if err := tx.saveChange(ch, ch.Version); err != nil {
					return err
				}
				tx.appendEvent(&Event{SubscriptionID: id, ChangeID: ch.ID,
					Type: EventChangeTerminated, CreatedAt: now})
			}
			sub.PendingChangeID = ""
		}
		sub.Status = SubTerminated
		sub.TerminatedAt = now
		if err := tx.saveSubscription(sub, subV); err != nil {
			return err
		}
		tx.appendEvent(&Event{SubscriptionID: id, Type: EventSubTerminated, CreatedAt: now})
		out, _ = tx.getSubscription(id)
		return nil
	})
	return out, err
}

// ---------------- 变更申请：申请 / 替换 / 取消 ----------------

// ChangeInput 是申请与替换的共同入参。
type ChangeInput struct {
	SubscriptionID string `json:"subscriptionId"`
	ClientToken    string `json:"clientToken,omitempty"` // 外部申请号（幂等/冲突检测）
	TargetPlanID   string `json:"targetPlanId"`
	EffectiveType  string `json:"effectiveType"` // IMMEDIATE / NEXT_PERIOD
	// ExpectedChangeID 仅替换时使用：调用方所看到的当前待生效申请 ID。
	// 非空时启用乐观并发控制：当前待生效申请已被别人替换/取消/推进（ID 不匹配）
	// 则返回 ErrConcurrentUpdate，保证并发修改不会静默互相覆盖；
	// 为空表示不做前置身份校验（盲替换，仍有完整审计链）。
	ExpectedChangeID string `json:"expectedChangeId,omitempty"`
}

// ApplyChange 受理一笔套餐变更申请。
//
//   - IMMEDIATE：当前周期内立即切换，冻结受理时刻原/新套餐价格与当前周期，
//     按剩余日历日精确计算补/退差额；
//   - NEXT_PERIOD：在下一周期边界切换，冻结原/新套餐价格与下一周期，
//     边界收取新套餐整周期费用。
//
// 同一订阅只能存在一笔待生效变更；相同外部申请号重复请求按幂等处理，
// 但目标套餐或生效方式不同则报冲突。
func (s *Service) ApplyChange(ctx context.Context, in ChangeInput) (*ChangeApplication, error) {
	now := clockFrom(ctx).Now()
	var out *ChangeApplication
	err := s.store.Update(func(tx *Tx) error {
		ch, err := s.applyChangeTx(tx, in, now)
		if err != nil {
			return err
		}
		out = ch
		return nil
	})
	return out, err
}

// ReplaceChange 用当前最新价格版本替换尚未生效的变更申请。
// 旧申请置为 SUPERSEDED，新申请重新冻结价格与计费周期；
// 旧申请的外部申请号保留其历史归属，不能被新内容复用。
func (s *Service) ReplaceChange(ctx context.Context, in ChangeInput) (*ChangeApplication, error) {
	now := clockFrom(ctx).Now()
	var out *ChangeApplication
	err := s.store.Update(func(tx *Tx) error {
		sub, err := loadActiveSub(tx, in.SubscriptionID)
		if err != nil {
			return err
		}
		if sub.PendingChangeID == "" {
			return notFoundf("no pending change on subscription %s", in.SubscriptionID)
		}
		old, ok := tx.getChange(sub.PendingChangeID)
		if !ok {
			return notFoundf("pending change %s", sub.PendingChangeID)
		}
		if old.Status != ChangePending {
			return fmt.Errorf("%w: change %s is %s", ErrNotPending, old.ID, old.Status)
		}
		// 幂等重放先于乐观锁检查：成功替换的重试返回其创建的申请。
		if dup, err := checkClientToken(tx, in); err != nil {
			return err
		} else if dup != nil {
			out = dup // 幂等重放已存在的申请
			return nil
		}
		// 乐观并发控制：只能替换调用方所看到的那一笔待生效申请。
		if in.ExpectedChangeID != "" && in.ExpectedChangeID != old.ID {
			return fmt.Errorf("%w: expected pending change %s but current is %s",
				ErrConcurrentUpdate, in.ExpectedChangeID, old.ID)
		}

		newCh, sub2, err := buildPendingChange(tx, sub, in, now)
		if err != nil {
			return err
		}
		// 旧申请终态化。
		old.Status = ChangeSuperseded
		old.DecidedAt = now
		if err := tx.saveChange(old, old.Version); err != nil {
			return err
		}
		// 新申请落库并接管 pending 指针。
		newCh.ID = tx.newID("change")
		newCh.SupersedesChangeID = old.ID
		if err := tx.saveChange(newCh, 0); err != nil {
			return err
		}
		sub2.PendingChangeID = newCh.ID
		if err := tx.saveSubscription(sub2, sub.Version); err != nil {
			return err
		}
		tx.appendEvent(&Event{SubscriptionID: sub.ID, ChangeID: old.ID,
			Type:      EventChangeReplaced,
			Detail:    fmt.Sprintf(`{"replacedBy":%q}`, newCh.ID),
			CreatedAt: now})
		tx.appendEvent(&Event{SubscriptionID: sub.ID, ChangeID: newCh.ID,
			Type: EventChangeApplied,
			Detail: fmt.Sprintf(`{"toPlanId":%q,"effectiveType":%q,"supersedes":%q}`,
				in.TargetPlanID, in.EffectiveType, old.ID),
			CreatedAt: now})
		out, _ = tx.getChange(newCh.ID)
		return nil
	})
	return out, err
}

// CancelChange 取消订阅当前待生效的变更申请。
// 申请必须仍为 PENDING；已经生效（或已终止/被替换）的申请不能被取消，
// 迟到取消绝不会把已生效的套餐切换还原。
func (s *Service) CancelChange(ctx context.Context, subscriptionID string) (*ChangeApplication, error) {
	now := clockFrom(ctx).Now()
	var out *ChangeApplication
	err := s.store.Update(func(tx *Tx) error {
		sub, ok := tx.getSubscription(subscriptionID)
		if !ok {
			return notFoundf("subscription %s", subscriptionID)
		}
		if sub.PendingChangeID == "" {
			return fmt.Errorf("%w: no pending change on subscription %s", ErrNotPending, subscriptionID)
		}
		ch, ok := tx.getChange(sub.PendingChangeID)
		if !ok {
			return notFoundf("pending change %s", sub.PendingChangeID)
		}
		if ch.Status != ChangePending {
			return fmt.Errorf("%w: change %s is %s", ErrNotPending, ch.ID, ch.Status)
		}
		subV := sub.Version
		chV := ch.Version
		ch.Status = ChangeCancelled
		ch.DecidedAt = now
		if err := tx.saveChange(ch, chV); err != nil {
			return err
		}
		sub.PendingChangeID = ""
		if err := tx.saveSubscription(sub, subV); err != nil {
			return err
		}
		tx.appendEvent(&Event{SubscriptionID: subscriptionID, ChangeID: ch.ID,
			Type: EventChangeCancelled, CreatedAt: now})
		out, _ = tx.getChange(ch.ID)
		return nil
	})
	return out, err
}

// applyChangeTx 是 ApplyChange 的事务体；supersedes 非空时表示内部替换场景。
func (s *Service) applyChangeTx(tx *Tx, in ChangeInput, now time.Time) (*ChangeApplication, error) {
	// 幂等/冲突检查优先：即使订阅随后终止，同一外部申请号的重试仍稳定返回原申请。
	if dup, err := checkClientToken(tx, in); err != nil {
		return nil, err
	} else if dup != nil {
		return dup, nil // 幂等重放：原样返回已受理申请
	}
	sub, err := loadActiveSub(tx, in.SubscriptionID)
	if err != nil {
		return nil, err
	}
	if sub.PendingChangeID != "" {
		return nil, fmt.Errorf("%w: subscription %s has pending change %s",
			ErrPendingChangeExist, sub.ID, sub.PendingChangeID)
	}
	ch, sub2, err := buildPendingChange(tx, sub, in, now)
	if err != nil {
		return nil, err
	}
	ch.ID = tx.newID("change")
	if err := tx.saveChange(ch, 0); err != nil {
		return nil, err
	}
	sub2.PendingChangeID = ch.ID
	if err := tx.saveSubscription(sub2, sub.Version); err != nil {
		return nil, err
	}
	tx.appendEvent(&Event{SubscriptionID: sub.ID, ChangeID: ch.ID,
		Type:      EventChangeApplied,
		Detail:    fmt.Sprintf(`{"toPlanId":%q,"effectiveType":%q}`, in.TargetPlanID, in.EffectiveType),
		CreatedAt: now})
	got, _ := tx.getChange(ch.ID)
	return got, nil
}

// ---------------- 到期推进（生效结算） ----------------

// EffectiveResult 是一次生效推进的全部产物，三者同事务产生。
type EffectiveResult struct {
	Change       *ChangeApplication `json:"change"`
	Bill         *Bill              `json:"bill"`
	Notification *Notification      `json:"notification"`
}

// AdvanceSubscription 推进订阅当前待生效变更：到期则在一次原子操作中
// 切换套餐、写入差额账单并生成唯一通知。
// 多个执行者并发推进同一申请时，只有一个能提交，其余收到 ErrConcurrentUpdate；
// 账单以 changeID 为唯一键，旧版本任务不可能再次收费。
func (s *Service) AdvanceSubscription(ctx context.Context, subscriptionID string) (*EffectiveResult, error) {
	now := clockFrom(ctx).Now()
	var out *EffectiveResult
	err := s.store.Update(func(tx *Tx) error {
		sub, ok := tx.getSubscription(subscriptionID)
		if !ok {
			return notFoundf("subscription %s", subscriptionID)
		}
		if sub.Status == SubTerminated {
			return ErrSubscriptionEnded
		}
		if sub.PendingChangeID == "" {
			return fmt.Errorf("%w: no pending change on subscription %s", ErrNotPending, subscriptionID)
		}
		r, err := advanceChange(tx, sub, sub.PendingChangeID, now)
		if err != nil {
			return err
		}
		out = r
		return nil
	})
	return out, err
}

// AdvanceChange 按变更申请 ID 推进（调度器/外部执行者按申请号处理时使用）。
func (s *Service) AdvanceChange(ctx context.Context, changeID string) (*EffectiveResult, error) {
	now := clockFrom(ctx).Now()
	var out *EffectiveResult
	err := s.store.Update(func(tx *Tx) error {
		ch, ok := tx.getChange(changeID)
		if !ok {
			return notFoundf("change %s", changeID)
		}
		sub, ok := tx.getSubscription(ch.SubscriptionID)
		if !ok {
			return notFoundf("subscription %s", ch.SubscriptionID)
		}
		if sub.Status == SubTerminated {
			return ErrSubscriptionEnded
		}
		if sub.PendingChangeID != changeID {
			return fmt.Errorf("%w: change %s is not the current pending change", ErrNotPending, changeID)
		}
		r, err := advanceChange(tx, sub, changeID, now)
		if err != nil {
			return err
		}
		out = r
		return nil
	})
	return out, err
}

// advanceChange 是推进的共享事务体：到期校验 + CAS 状态流转 + 三件写入。
func advanceChange(tx *Tx, sub *Subscription, changeID string, now time.Time) (*EffectiveResult, error) {
	ch, ok := tx.getChange(changeID)
	if !ok {
		return nil, notFoundf("change %s", changeID)
	}
	if ch.Status != ChangePending {
		return nil, fmt.Errorf("%w: change %s is %s", ErrNotPending, ch.ID, ch.Status)
	}
	if now.Before(ch.EffectiveAt) {
		return nil, invalidArgf("change %s is not due until %s (now %s)", ch.ID, ch.EffectiveAt, now)
	}

	subV := sub.Version
	chV := ch.Version

	// 1) 切换套餐。立即变更保留当前周期（按日比例已结算）；
	//    下一周期变更把周期推进到冻结的下一周期。
	sub.CurrentPlanID = ch.ToPlan.PlanID
	if ch.EffectiveType == EffectiveNextPeriod {
		sub.PeriodStart = ch.FrozenPeriodStart
		sub.PeriodEnd = ch.FrozenPeriodEnd
	}
	sub.PendingChangeID = ""

	// 2) 差额账单（changeID 唯一，重复收费会被索引拒绝）。
	bill := &Bill{
		ID:             tx.newID("bill"),
		SubscriptionID: sub.ID,
		ChangeID:       ch.ID,
		Kind:           BillChangeSettlement,
		Amount:         ch.SettlementAmount,
		Currency:       ch.ToPlan.Currency,
		Description:    settlementDescription(ch),
		CreatedAt:      now,
	}
	if err := tx.insertBill(bill); err != nil {
		return nil, err
	}

	// 3) 唯一生效通知。
	payload, _ := json.Marshal(map[string]any{
		"subscriptionId":   sub.ID,
		"changeId":         ch.ID,
		"fromPlanId":       ch.FromPlan.PlanID,
		"toPlanId":         ch.ToPlan.PlanID,
		"effectiveType":    ch.EffectiveType,
		"settlementAmount": ch.SettlementAmount,
		"currency":         ch.ToPlan.Currency,
	})
	notif := &Notification{
		ID:        tx.newID("ntf"),
		ChangeID:  ch.ID,
		Type:      NotificationChangeEffective,
		Payload:   string(payload),
		CreatedAt: now,
	}
	if err := tx.insertNotification(notif); err != nil {
		return nil, err
	}

	// 4) 申请终态化（CAS：并发执行者中只有一个能通过版本校验）。
	ch.Status = ChangeEffective
	ch.DecidedAt = now
	ch.ResultBillID = bill.ID
	ch.ResultNotificationID = notif.ID
	if err := tx.saveChange(ch, chV); err != nil {
		return nil, err
	}
	if err := tx.saveSubscription(sub, subV); err != nil {
		return nil, err
	}
	tx.appendEvent(&Event{SubscriptionID: sub.ID, ChangeID: ch.ID,
		Type:      EventChangeEffective,
		Detail:    fmt.Sprintf(`{"billId":%q,"amount":%d}`, bill.ID, bill.Amount),
		CreatedAt: now})

	gotCh, _ := tx.getChange(ch.ID)
	gotBill := *bill
	gotNotif := *notif
	return &EffectiveResult{Change: gotCh, Bill: &gotBill, Notification: &gotNotif}, nil
}

// SweepDue 推进所有已到期但仍 PENDING 的变更（供周期边界调度器调用）。
// 已经被并发处理/取消/终止的申请跳过，不报错。
func (s *Service) SweepDue(ctx context.Context) []*EffectiveResult {
	now := clockFrom(ctx).Now()
	// 先在只读事务里收集到期订阅，释放锁后再逐笔推进
	// （Update 与 View 使用同一把互斥锁，不能嵌套）。
	var dueSubIDs []string
	_ = s.store.View(func(tx *Tx) error {
		for _, sub := range tx.listSubscriptions() {
			if sub.Status != SubActive || sub.PendingChangeID == "" {
				continue
			}
			ch, ok := tx.getChange(sub.PendingChangeID)
			if !ok || ch.Status != ChangePending || now.Before(ch.EffectiveAt) {
				continue
			}
			dueSubIDs = append(dueSubIDs, sub.ID)
		}
		return nil
	})
	var results []*EffectiveResult
	for _, id := range dueSubIDs {
		r, err := s.AdvanceSubscription(ctx, id)
		if err == nil && r != nil {
			results = append(results, r)
		}
	}
	return results
}

// ---------------- 查询 ----------------

// GetChange 查询变更申请。
func (s *Service) GetChange(ctx context.Context, id string) (*ChangeApplication, error) {
	var out *ChangeApplication
	err := s.store.View(func(tx *Tx) error {
		ch, ok := tx.getChange(id)
		if !ok {
			return notFoundf("change %s", id)
		}
		out = ch
		return nil
	})
	return out, err
}

// ListBills 查询订阅的全部账单（按创建顺序）。
func (s *Service) ListBills(ctx context.Context, subscriptionID string) ([]*Bill, error) {
	var out []*Bill
	err := s.store.View(func(tx *Tx) error {
		if _, ok := tx.getSubscription(subscriptionID); !ok {
			return notFoundf("subscription %s", subscriptionID)
		}
		out = tx.listBills(subscriptionID)
		return nil
	})
	return out, err
}

// ListHistory 查询订阅的持久化变更历史（按序号升序）。
func (s *Service) ListHistory(ctx context.Context, subscriptionID string) ([]*Event, error) {
	var out []*Event
	err := s.store.View(func(tx *Tx) error {
		if _, ok := tx.getSubscription(subscriptionID); !ok {
			return notFoundf("subscription %s", subscriptionID)
		}
		out = tx.listEvents(subscriptionID)
		return nil
	})
	return out, err
}

// ---------------- 内部辅助 ----------------

// loadActiveSub 取出订阅并校验存在且未终止。
func loadActiveSub(tx *Tx, id string) (*Subscription, error) {
	sub, ok := tx.getSubscription(id)
	if !ok {
		return nil, notFoundf("subscription %s", id)
	}
	if sub.Status == SubTerminated {
		return nil, ErrSubscriptionEnded
	}
	return sub, nil
}

// checkClientToken 处理外部申请号：
//   - 号不存在：返回 (nil, nil)；
//   - 同订阅、同目标套餐、同生效方式：返回已存在申请（调用方按幂等重放）；
//   - 其余情况（不同订阅，或目标套餐/生效方式不同）：返回冲突。
func checkClientToken(tx *Tx, in ChangeInput) (*ChangeApplication, error) {
	if in.ClientToken == "" {
		return nil, nil
	}
	exist, ok := tx.changeByClientToken(in.ClientToken)
	if !ok {
		return nil, nil
	}
	if exist.SubscriptionID != in.SubscriptionID {
		return nil, conflictf("client token %s already bound to another subscription", in.ClientToken)
	}
	if exist.ToPlan.PlanID == in.TargetPlanID && exist.EffectiveType == in.EffectiveType {
		return exist, nil
	}
	return nil, conflictf("client token %s reused with different target plan or effective type", in.ClientToken)
}

// buildPendingChange 校验套餐与入参，按当前价格版本冻结计费要素。
// 返回未持久化的 change（ID 由调用方分配）以及传入订阅的拷贝。
func buildPendingChange(tx *Tx, sub *Subscription, in ChangeInput, now time.Time) (*ChangeApplication, *Subscription, error) {
	if in.TargetPlanID == "" || in.EffectiveType == "" {
		return nil, nil, invalidArgf("targetPlanId and effectiveType are required")
	}
	if in.EffectiveType != EffectiveImmediate && in.EffectiveType != EffectiveNextPeriod {
		return nil, nil, invalidArgf("effectiveType must be %s or %s", EffectiveImmediate, EffectiveNextPeriod)
	}
	from, ok := tx.getPlan(sub.CurrentPlanID)
	if !ok {
		return nil, nil, notFoundf("current plan %s", sub.CurrentPlanID)
	}
	to, ok := tx.getPlan(in.TargetPlanID)
	if !ok {
		return nil, nil, notFoundf("target plan %s", in.TargetPlanID)
	}
	if from.Currency != to.Currency {
		return nil, nil, invalidArgf("currency mismatch: %s vs %s", from.Currency, to.Currency)
	}
	if to.ID == from.ID {
		return nil, nil, ErrSamePlan
	}
	fromPV, ok := from.priceAt(now)
	if !ok {
		return nil, nil, invalidArgf("plan %s has no price effective at %s", from.ID, now)
	}
	toPV, ok := to.priceAt(now)
	if !ok {
		return nil, nil, invalidArgf("plan %s has no price effective at %s", to.ID, now)
	}

	ch := &ChangeApplication{
		SubscriptionID: sub.ID,
		ClientToken:    in.ClientToken,
		FromPlan:       FrozenPrice{PlanID: from.ID, Version: fromPV.Version, Amount: fromPV.Amount, Currency: from.Currency},
		ToPlan:         FrozenPrice{PlanID: to.ID, Version: toPV.Version, Amount: toPV.Amount, Currency: to.Currency},
		EffectiveType:  in.EffectiveType,
		Status:         ChangePending,
		CreatedAt:      now,
	}

	sub2 := *sub
	switch in.EffectiveType {
	case EffectiveImmediate:
		sp := computeImmediateSettlement(fromPV.Amount, toPV.Amount, sub.PeriodStart, sub.PeriodEnd, now)
		ch.EffectiveAt = now
		ch.FrozenPeriodStart = sp.periodStart
		ch.FrozenPeriodEnd = sp.periodEnd
		ch.SettlementAmount = sp.amount
		ch.ProrationTotalDays = sp.totalDays
		ch.ProrationRemainDays = sp.remainDays
	case EffectiveNextPeriod:
		nextStart := sub.PeriodEnd
		nextEnd := nextMonthlyBoundary(nextStart)
		sp := computeNextPeriodSettlement(toPV.Amount, nextStart, nextEnd)
		ch.EffectiveAt = nextStart
		ch.FrozenPeriodStart = sp.periodStart
		ch.FrozenPeriodEnd = sp.periodEnd
		ch.SettlementAmount = sp.amount
		ch.ProrationTotalDays = sp.totalDays
		ch.ProrationRemainDays = sp.remainDays
	}
	return ch, &sub2, nil
}

// settlementDescription 生成账单可读描述。
func settlementDescription(ch *ChangeApplication) string {
	switch ch.EffectiveType {
	case EffectiveImmediate:
		if ch.SettlementAmount >= 0 {
			return fmt.Sprintf("immediate upgrade %s->%s, %d/%d days remaining",
				ch.FromPlan.PlanID, ch.ToPlan.PlanID, ch.ProrationRemainDays, ch.ProrationTotalDays)
		}
		return fmt.Sprintf("immediate downgrade %s->%s, %d/%d days remaining",
			ch.FromPlan.PlanID, ch.ToPlan.PlanID, ch.ProrationRemainDays, ch.ProrationTotalDays)
	default:
		return fmt.Sprintf("next-period change %s->%s, plan price v%d",
			ch.FromPlan.PlanID, ch.ToPlan.PlanID, ch.ToPlan.Version)
	}
}
