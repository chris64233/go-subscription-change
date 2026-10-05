package gosubscriptionchange

import "time"

// 生效方式。
const (
	// EffectiveImmediate 当前计费周期内立即变更：
	// 按日历日比例，退还原套餐剩余周期未用价值，补收新套餐剩余周期价值。
	EffectiveImmediate = "IMMEDIATE"
	// EffectiveNextPeriod 在下一计费周期边界变更：
	// 周期边界一次性切换，并按新套餐冻结价格收取下一周期费用。
	EffectiveNextPeriod = "NEXT_PERIOD"
	// EffectiveScheduled 在用户指定的未来生效时间变更（当前计费周期内）：
	// 受理时按生效时点的剩余日历日冻结差额，到期切换套餐，计费周期保持不变。
	EffectiveScheduled = "SCHEDULED"
)

// 变更申请状态机（状态只能向前流转，不可回退）：
//
//	PENDING ──(到期推进)──> EFFECTIVE
//	   │
//	   ├──(到期未支付)────> FAILED
//	   ├──(用户取消)──────> CANCELLED
//	   ├──(被新申请替换)──> SUPERSEDED
//	   └──(订阅终止)──────> TERMINATED
const (
	ChangePending    = "PENDING"
	ChangeEffective  = "EFFECTIVE"
	ChangeFailed     = "FAILED"
	ChangeCancelled  = "CANCELLED"
	ChangeSuperseded = "SUPERSEDED"
	ChangeTerminated = "TERMINATED"
)

// FrozenPrice 冻结受理时刻的价格版本快照。
type FrozenPrice struct {
	PlanID   string `json:"planId"`
	Version  int    `json:"version"`
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
}

// ChangeApplication 是一笔套餐变更申请。
// 同一订阅至多存在一笔 PENDING 申请；申请一旦受理，
// 原套餐、新套餐、计费周期与价格版本全部冻结，后续价格变化不影响本申请。
type ChangeApplication struct {
	ID             string      `json:"id"`
	SubscriptionID string      `json:"subscriptionId"`
	ClientToken    string      `json:"clientToken,omitempty"` // 外部申请号，全局唯一（幂等/冲突检测）
	FromPlan       FrozenPrice `json:"fromPlan"`
	ToPlan         FrozenPrice `json:"toPlan"`
	EffectiveType  string      `json:"effectiveType"`
	EffectiveAt    time.Time   `json:"effectiveAt"` // 计划生效时间
	// 冻结的计费周期（IMMEDIATE：当前周期；NEXT_PERIOD：下一周期）。
	FrozenPeriodStart time.Time `json:"frozenPeriodStart"`
	FrozenPeriodEnd   time.Time `json:"frozenPeriodEnd"`
	// 冻结的精确差额账单金额（正=补收，负=退还）。
	// IMMEDIATE：当前周期按日比例的净额；NEXT_PERIOD：下一周期整周期新价。
	SettlementAmount     int64     `json:"settlementAmount"`
	ProrationTotalDays   int       `json:"prorationTotalDays,omitempty"`
	ProrationRemainDays  int       `json:"prorationRemainDays,omitempty"`
	Status               string    `json:"status"`
	SupersedesChangeID   string    `json:"supersedesChangeId,omitempty"` // 本申请替换掉的旧申请
	FailureReason        string    `json:"failureReason,omitempty"`      // FAILED 时记录失败原因
	PaidAt               time.Time `json:"paidAt,omitempty"`             // 需支付申请的支付确认时间
	Version              int64     `json:"version"`                      // 乐观锁版本
	CreatedAt            time.Time `json:"createdAt"`
	DecidedAt            time.Time `json:"decidedAt,omitempty"`    // 终态时间
	ResultBillID         string    `json:"resultBillId,omitempty"` // 生效后产生的账单
	ResultNotificationID string    `json:"resultNotificationId,omitempty"`
}

// paymentRequired 报告本申请是否必须在生效前完成支付确认：
// 预约类变更（NEXT_PERIOD / SCHEDULED）差额为正时必须先收款；
// 立即变更在生效时一并结算，不要求预先支付；差额为零或为负（退还）同样无需收款。
func (ch *ChangeApplication) paymentRequired() bool {
	return ch.EffectiveType != EffectiveImmediate && ch.SettlementAmount > 0
}

// paid 报告本申请是否已确认支付。
func (ch *ChangeApplication) paid() bool {
	return !ch.PaidAt.IsZero()
}

// settlementSpec 是受理时计算并冻结的计费明细。
type settlementSpec struct {
	amount      int64
	totalDays   int
	remainDays  int
	periodStart time.Time
	periodEnd   time.Time
}

// computeImmediateSettlement 计算“当前周期内立即变更”的差额。
//
//	退原套餐剩余：-oldPrice * remaining / total
//	补新套餐剩余：+newPrice * remaining / total
//	净额 = roundHalfUp((newPrice - oldPrice) * remaining / total)
//
// 单独对净额做一次四舍五入（最小货币单位不可分），金额以整数分存储。
func computeImmediateSettlement(oldPrice, newPrice int64, periodStart, periodEnd, at time.Time) settlementSpec {
	total, _, remaining := periodDayInfo(periodStart, periodEnd, at)
	sp := settlementSpec{totalDays: total, remainDays: remaining,
		periodStart: periodStart, periodEnd: periodEnd}
	if total <= 0 {
		// 退化周期不做比例分摊。
		sp.amount = newPrice - oldPrice
		return sp
	}
	sp.amount = roundHalfUp(float64(newPrice-oldPrice) * float64(remaining) / float64(total))
	return sp
}

// computeNextPeriodSettlement 计算“下一周期边界变更”在边界应收取的金额：
// 下一整周期按冻结的新套餐价格收取。
func computeNextPeriodSettlement(newPrice int64, nextStart, nextEnd time.Time) settlementSpec {
	total, _, _ := periodDayInfo(nextStart, nextEnd, nextStart)
	return settlementSpec{amount: newPrice, totalDays: total, remainDays: total,
		periodStart: nextStart, periodEnd: nextEnd}
}
