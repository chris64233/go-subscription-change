package gosubscriptionchange

import "time"

// 账单类型。
const (
	// BillChangeSettlement 变更差额账单。
	// 立即变更：当前周期按日比例的补/退净额；下一周期变更：下一整周期新价。
	// 每笔变更至多产生一张差额账单（以 changeID 唯一约束保证只收一次费）。
	BillChangeSettlement = "CHANGE_SETTLEMENT"
)

// Bill 是一张账单。Amount 带符号：正数=应补收，负数=应退还。
type Bill struct {
	ID             string    `json:"id"`
	SubscriptionID string    `json:"subscriptionId"`
	ChangeID       string    `json:"changeId,omitempty"` // 来源变更；同 changeID 唯一
	Kind           string    `json:"kind"`
	Amount         int64     `json:"amount"`
	Currency       string    `json:"currency"`
	Description    string    `json:"description"`
	CreatedAt      time.Time `json:"createdAt"`
}

// Notification 是变更生效通知。通知以 changeID 为幂等键，
// 多个执行者同时推进同一申请时只生成一条。
type Notification struct {
	ID        string    `json:"id"`
	ChangeID  string    `json:"changeId"` // 唯一
	Type      string    `json:"type"`
	Payload   string    `json:"payload"`
	CreatedAt time.Time `json:"createdAt"`
}

// 通知类型。
const (
	NotificationChangeEffective = "CHANGE_EFFECTIVE"
)

// 变更历史事件类型（持久化的审计流水，状态变化均追加，不可修改）。
const (
	EventChangeApplied    = "CHANGE_APPLIED"    // 申请受理
	EventChangeReplaced   = "CHANGE_REPLACED"   // 旧申请被替换
	EventChangeCancelled  = "CHANGE_CANCELLED"  // 用户取消
	EventChangeEffective  = "CHANGE_EFFECTIVE"  // 到期生效
	EventChangeFailed     = "CHANGE_FAILED"     // 到期未支付，切换失败
	EventChangeTerminated = "CHANGE_TERMINATED" // 随订阅终止而关闭
	EventPaymentSucceeded = "PAYMENT_SUCCEEDED" // 支付确认
	EventPaymentFailed    = "PAYMENT_FAILED"    // 支付失败（可换新支付号重试）
	EventSubCreated       = "SUBSCRIPTION_CREATED"
	EventSubTerminated    = "SUBSCRIPTION_TERMINATED"
)

// Event 是一条持久化的变更历史。
type Event struct {
	Seq            int64     `json:"seq"` // 自增序号
	SubscriptionID string    `json:"subscriptionId"`
	ChangeID       string    `json:"changeId,omitempty"`
	Type           string    `json:"type"`
	Detail         string    `json:"detail,omitempty"`
	CreatedAt      time.Time `json:"createdAt"`
}
