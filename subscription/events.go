package subscription

import "time"

// 事件类型。所有状态变更都以事件追加到日志；变更历史天然持久化且不可改写。
const (
	EvPlanConfigured   = "plan_configured"
	EvPlanPriceAdded   = "plan_price_added"
	EvSubCreated       = "subscription_created"
	EvSubTerminated    = "subscription_terminated"
	EvChangeApplied    = "change_applied"
	EvChangeReplaced   = "change_replaced"
	EvChangeCancelled  = "change_cancelled"
	EvChangeEffective  = "change_effective"
	EvBillWritten      = "bill_written"
	EvNotificationSent = "notification_sent"
)

// Event 是状态变更的不可变记录。字段按事件类型各取所需，
// 与 Reducer 一一对应；JSON 持久化到 WAL。
type Event struct {
	Seq         int64          `json:"seq"`
	Type        string         `json:"type"`
	At          time.Time      `json:"at"`
	Plan        *Plan          `json:"plan,omitempty"`
	PlanID      string         `json:"plan_id,omitempty"`
	Version     *PlanVersion   `json:"version,omitempty"`
	Sub         *Subscription  `json:"sub,omitempty"`
	SubID       string         `json:"sub_id,omitempty"`
	Change      *ChangeRequest `json:"change,omitempty"`
	OldChangeID string         `json:"old_change_id,omitempty"`
	Bill        *Bill          `json:"bill,omitempty"`
	Notify      *Notification  `json:"notification,omitempty"`
	DedupKey    string         `json:"dedup_key,omitempty"`
}

// state 可变的内存投影。Store 在互斥锁内调用 reducer 顺序应用事件。
type state struct {
	plans       map[string]*Plan
	subs        map[string]*Subscription
	changes     map[string]*ChangeRequest
	subChanges  map[string][]string // subscriptionID -> change IDs（版本顺序）
	bills       map[string]*Bill
	changeBills map[string]string // changeID -> billID（幂等键）
	notifs      map[string]*Notification
	notifDedup  map[string]string // dedupKey -> notificationID
	externalIdx map[string]string // externalID -> changeID
}

func newState() *state {
	return &state{
		plans:       map[string]*Plan{},
		subs:        map[string]*Subscription{},
		changes:     map[string]*ChangeRequest{},
		subChanges:  map[string][]string{},
		bills:       map[string]*Bill{},
		changeBills: map[string]string{},
		notifs:      map[string]*Notification{},
		notifDedup:  map[string]string{},
		externalIdx: map[string]string{},
	}
}

// apply 把单个事件应用到状态。只做机械投影，业务校验在 service 的 mutate 闭包内完成。
func (s *state) apply(e Event) {
	switch e.Type {
	case EvPlanConfigured:
		if e.Plan != nil {
			s.plans[e.Plan.ID] = e.Plan
		}
	case EvPlanPriceAdded:
		if p := s.plans[e.PlanID]; p != nil && e.Version != nil {
			// 重放时按版本号去重，保证追加新版本不覆盖历史版本。
			found := false
			for _, v := range p.Versions {
				if v.Version == e.Version.Version {
					found = true
					break
				}
			}
			if !found {
				p.Versions = append(p.Versions, *e.Version)
			}
		}
	case EvSubCreated:
		if e.Sub != nil {
			s.subs[e.Sub.ID] = e.Sub
		}
	case EvSubTerminated:
		if sub := s.subs[e.SubID]; sub != nil {
			sub.Status = SubTerminated
			at := e.At
			sub.TerminatedAt = &at
			sub.PendingChangeID = ""
		}
	case EvChangeApplied:
		c := e.Change
		if c == nil {
			return
		}
		s.changes[c.ID] = c
		s.subChanges[c.SubscriptionID] = append(s.subChanges[c.SubscriptionID], c.ID)
		if sub := s.subs[c.SubscriptionID]; sub != nil && c.Status == ChangePending {
			sub.PendingChangeID = c.ID
		}
		if c.ExternalID != "" {
			if _, ok := s.externalIdx[c.ExternalID]; !ok {
				s.externalIdx[c.ExternalID] = c.ID
			}
		}
	case EvChangeReplaced:
		old := s.changes[e.OldChangeID]
		if old != nil {
			old.Status = ChangeSuperseded
		}
		c := e.Change
		if c != nil {
			s.changes[c.ID] = c
			s.subChanges[c.SubscriptionID] = append(s.subChanges[c.SubscriptionID], c.ID)
			if sub := s.subs[c.SubscriptionID]; sub != nil {
				sub.PendingChangeID = c.ID
			}
			if c.ExternalID != "" {
				// 新外部号指向最新待生效版本。
				s.externalIdx[c.ExternalID] = c.ID
			}
			if old != nil && old.ExternalID != "" && old.ExternalID != c.ExternalID {
				// 无号替换或换号替换：旧号继续指向（已 superseded 的）原始申请，
				// 同号重放由 service 层识别其非 pending 状态并判冲突。
				s.externalIdx[old.ExternalID] = old.ID
			}
		}
	case EvChangeCancelled:
		if c := s.changes[e.Change.ID]; c != nil {
			c.Status = ChangeCancelled
			at := e.At
			c.CancelledAt = &at
			if sub := s.subs[c.SubscriptionID]; sub != nil && sub.PendingChangeID == c.ID {
				sub.PendingChangeID = ""
			}
			// 取消后外部申请号可被同参数重试复用；不同参数仍按冲突处理（service 层判定）。
			delete(s.externalIdx, c.ExternalID)
		}
	case EvChangeEffective:
		c := e.Change
		if c == nil {
			return
		}
		if cur := s.changes[c.ID]; cur != nil {
			cur.Status = ChangeEffective
			cur.EffectiveAt = c.EffectiveAt
			cur.BillID = c.BillID
			cur.NotificationID = c.NotificationID
		}
		if sub := s.subs[c.SubscriptionID]; sub != nil {
			if sub.PendingChangeID == c.ID {
				sub.PendingChangeID = ""
			}
			// 切换套餐 / 周期。终止后的套餐不能再切换：service 层拦截终止订阅，
			// reducer 再做一次防御，terminated 状态不切换。
			if sub.Status == SubActive {
				sub.Plan = c.ToPlan
				if c.EffectiveMode == EffectiveNextCycle {
					// 新周期从冻结周期边界起，按新套餐周期推进。
					sub.PeriodStart = c.Period.End
					sub.PeriodEnd = AddBillingCycle(c.Period.End, c.ToPlan.Cycle)
				}
				// immediate 模式周期窗口不变。
			}
		}
	case EvBillWritten:
		b := e.Bill
		if b == nil {
			return
		}
		if _, exists := s.changeBills[b.ChangeID]; b.ChangeID != "" && exists {
			return
		}
		if _, exists := s.bills[b.ID]; exists {
			return
		}
		s.bills[b.ID] = b
		if b.ChangeID != "" {
			s.changeBills[b.ChangeID] = b.ID
		}
	case EvNotificationSent:
		n := e.Notify
		if n == nil {
			return
		}
		if e.DedupKey != "" {
			if _, exists := s.notifDedup[e.DedupKey]; exists {
				return
			}
			s.notifDedup[e.DedupKey] = n.ID
		}
		if _, exists := s.notifs[n.ID]; !exists {
			s.notifs[n.ID] = n
		}
	}
}
