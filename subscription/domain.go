// Package subscription implements scheduled subscription plan changes and
// effective settlement: applications freeze plan/price/period snapshots,
// prorated differences are computed with exact rational arithmetic, and the
// effective transition (plan switch + bill + notification) is committed as a
// single atomic, idempotent operation backed by an event-sourced store.
package subscription

import (
	"errors"
	"math/big"
	"time"
)

// 生效方式：当前计费周期内立即变更，或在下一周期边界变更。
const (
	EffectiveImmediate = "immediate" // 当前周期内立即变更，按剩余天数补/退差额
	EffectiveNextCycle = "next_cycle"
)

// 变更申请状态。
const (
	ChangePending    = "pending"    // 待生效
	ChangeEffective  = "effective"  // 已生效
	ChangeCancelled  = "cancelled"  // 已取消（可被同外部号再次申请）
	ChangeSuperseded = "superseded" // 被同订阅更新版本替换
)

// 订阅状态。
const (
	SubActive     = "active"
	SubTerminated = "terminated"
)

// 账单方向。
const (
	BillCharge = "charge" // 应收（补差价 / 新周期费用）
	BillRefund = "refund" // 应退
)

// 账单来源。
const (
	BillKindInitial   = "initial"   // 订阅创建首期
	BillKindProration = "proration" // 立即变更差额
	BillKindRenewal   = "renewal"   // 下一周期边界生效的新周期费用
)

// 标准周期时长（月/年用相对日期推进，日/周用固定时长）。
const (
	CycleDaily   = "daily"
	CycleWeekly  = "weekly"
	CycleMonthly = "monthly"
	CycleYearly  = "yearly"
)

// Sentinel errors. 用 errors.Is 判断，HTTP 层据此映射状态码。
var (
	ErrNotFound               = errors.New("subscription: not found")
	ErrAlreadyExists          = errors.New("subscription: already exists")
	ErrConflict               = errors.New("subscription: conflict")
	ErrSubscriptionTerminated = errors.New("subscription: subscription terminated")
	ErrPlanMissing            = errors.New("subscription: plan not configured")
	ErrInvalidArgument        = errors.New("subscription: invalid argument")
	ErrPendingChangeExists    = errors.New("subscription: pending change already exists")
	ErrVersionMismatch        = errors.New("subscription: version mismatch") // 乐观并发：期望版本不是当前待生效版本
	ErrChangeNotDue           = errors.New("subscription: change not due")
)

// PlanVersion 是套餐的一个不可变价格版本。调价即追加新版本，
// 已经受理的申请继续使用受理时冻结的版本，历史价格变化不能改写申请。
type PlanVersion struct {
	Version    int       `json:"version"`
	PriceCents int64     `json:"price_cents"` // 一个标准计费周期的价格（整数分）
	Effective  time.Time `json:"effective"`   // 价格版本生效时间
	CreatedAt  time.Time `json:"created_at"`
}

// Plan 套餐配置。
type Plan struct {
	ID        string        `json:"id"`
	Name      string        `json:"name"`
	Cycle     string        `json:"cycle"`
	Versions  []PlanVersion `json:"versions"`
	CreatedAt time.Time     `json:"created_at"`
}

// CurrentVersion 返回当前价格版本（now 之前最近一个生效版本）。
func (p *Plan) CurrentVersion(now time.Time) *PlanVersion {
	var cur *PlanVersion
	for i := range p.Versions {
		v := &p.Versions[i]
		if !v.Effective.After(now) && (cur == nil || v.Version > cur.Version) {
			cur = v
		}
	}
	return cur
}

// VersionAt 返回指定时间点生效的价格版本（受理申请时冻结用）。
func (p *Plan) VersionAt(at time.Time) *PlanVersion {
	return p.CurrentVersion(at)
}

// AddBillingCycle 在 t 基础上推进一个标准计费周期，返回 [t, end)。
// 月/年按日历推进并钳制到月末（如 1月31日 + 1月 = 2月28/29日）。
func AddBillingCycle(t time.Time, cycle string) time.Time {
	switch cycle {
	case CycleDaily:
		return t.AddDate(0, 0, 1)
	case CycleWeekly:
		return t.AddDate(0, 0, 7)
	case CycleMonthly:
		return addCalendarClamped(t, 0, 1)
	case CycleYearly:
		return addCalendarClamped(t, 1, 0)
	default:
		// 未知周期：按 30 天兜底，避免 panic。
		return t.AddDate(0, 0, 30)
	}
}

// addCalendarClamped 按日历推进年/月，并把日钳制到目标月末
// （1月31日 + 1月 = 2月28/29日；2月29日 + 1年 = 次年2月28日），
// 避免 time.AddDate 的跨月溢出规范化（1月31日 → 3月3日）。
func addCalendarClamped(t time.Time, years, months int) time.Time {
	day := t.Day()
	first := time.Date(t.Year(), t.Month(), 1, t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), t.Location())
	targetFirst := first.AddDate(years, months, 0)
	lastDay := targetFirst.AddDate(0, 1, -1).Day()
	if day > lastDay {
		day = lastDay
	}
	return time.Date(targetFirst.Year(), targetFirst.Month(), day,
		t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), t.Location())
}

// PlanSnapshot 申请受理时冻结的套餐价格版本。
type PlanSnapshot struct {
	PlanID     string `json:"plan_id"`
	PlanName   string `json:"plan_name"`
	Cycle      string `json:"cycle"`
	Version    int    `json:"version"`
	PriceCents int64  `json:"price_cents"`
}

func snapshotOf(p *Plan, v *PlanVersion) PlanSnapshot {
	return PlanSnapshot{
		PlanID:     p.ID,
		PlanName:   p.Name,
		Cycle:      p.Cycle,
		Version:    v.Version,
		PriceCents: v.PriceCents,
	}
}

// CycleSnapshot 申请受理时冻结的计费周期。
type CycleSnapshot struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// ChangeRequest 变更申请（持久化的变更历史条目）。
type ChangeRequest struct {
	ID             string        `json:"id"`
	SubscriptionID string        `json:"subscription_id"`
	ExternalID     string        `json:"external_id"`
	Version        int           `json:"version"` // 该订阅待生效变更的版本号，替换递增
	EffectiveMode  string        `json:"effective_mode"`
	FromPlan       PlanSnapshot  `json:"from_plan"`
	ToPlan         PlanSnapshot  `json:"to_plan"`
	Period         CycleSnapshot `json:"period"`         // 冻结的当前计费周期
	DiffCents      int64         `json:"diff_cents"`     // 正数补、负数退；next_cycle 受理时为 0
	DiffDirection  string        `json:"diff_direction"` // charge / refund；next_cycle 为 renewal 账单在生效时出
	ScheduledAt    time.Time     `json:"scheduled_at"`   // immediate: 受理时间；next_cycle: 周期边界
	AppliedAt      time.Time     `json:"applied_at"`     // 受理时间
	Status         string        `json:"status"`
	EffectiveAt    *time.Time    `json:"effective_at,omitempty"`
	CancelledAt    *time.Time    `json:"cancelled_at,omitempty"`
	BillID         string        `json:"bill_id,omitempty"`
	NotificationID string        `json:"notification_id,omitempty"`
}

// Bill 差额/费用账单。同一申请最多一张（advance 幂等）。
type Bill struct {
	ID             string    `json:"id"`
	SubscriptionID string    `json:"subscription_id"`
	ChangeID       string    `json:"change_id,omitempty"` // 首期账单为空
	Kind           string    `json:"kind"`                // initial / proration / renewal
	Direction      string    `json:"direction"`           // charge / refund
	AmountCents    int64     `json:"amount_cents"`
	Description    string    `json:"description"`
	CreatedAt      time.Time `json:"created_at"`
}

// Notification 生效通知。dedupKey 保证同一申请唯一通知。
type Notification struct {
	ID             string    `json:"id"`
	SubscriptionID string    `json:"subscription_id"`
	ChangeID       string    `json:"change_id"`
	Type           string    `json:"type"` // change_effective
	Message        string    `json:"message"`
	CreatedAt      time.Time `json:"created_at"`
}

// Subscription 订阅聚合。
type Subscription struct {
	ID              string       `json:"id"`
	Status          string       `json:"status"`
	Plan            PlanSnapshot `json:"current_plan"`
	PeriodStart     time.Time    `json:"period_start"`
	PeriodEnd       time.Time    `json:"period_end"`
	PendingChangeID string       `json:"pending_change_id,omitempty"`
	CreatedAt       time.Time    `json:"created_at"`
	TerminatedAt    *time.Time   `json:"terminated_at,omitempty"`
}

// Days 返回 [start,end) 的整天数（按 24h 计）。
func wholeDays(start, end time.Time) int64 {
	d := end.Sub(start)
	return int64(d / (24 * time.Hour))
}

// proration 以精确有理数计算立即变更的应补/应退差额（整数分，半分向上取整到分）。
//
//	新套餐剩余天数费用 = newPrice * remain/total
//	旧套餐已消耗天数费用 = oldPrice * used/total （理论值；实际退补按下式）
//	差额        = newPrice*remain/total - oldPrice*remain/total
//	            = (newPrice-oldPrice) * remain/total
//
// 正数=应补，负数=应退。remain/total 在同一时区下按整天比计算。
func proration(oldPriceCents, newPriceCents int64, usedDays, remainDays, totalDays int64) int64 {
	if totalDays <= 0 || remainDays <= 0 {
		return 0
	}
	num := big.NewRat(newPriceCents-oldPriceCents, 1)
	num.Mul(num, big.NewRat(remainDays, totalDays))
	return ratHalfUpCents(num)
}

// ratHalfUpCents 将有理金额（单位：分）半额向上取整为整数分：
// 正数 >= x.5 向上，负数按绝对值半额向上（即对称，远离零），结果确定无浮点误差。
func ratHalfUpCents(r *big.Rat) int64 {
	sign := int64(1)
	if r.Sign() < 0 {
		sign = -1
		r = new(big.Rat).Abs(r)
	}
	num := new(big.Int).Set(r.Num())
	den := new(big.Int).Set(r.Denom())
	q, rem := new(big.Int).QuoRem(num, den, new(big.Int))
	two := big.NewInt(2)
	// rem*2 >= den → 进位
	if new(big.Int).Mul(rem, two).Cmp(den) >= 0 {
		q.Add(q, big.NewInt(1))
	}
	v := q.Int64()
	return sign * v
}
