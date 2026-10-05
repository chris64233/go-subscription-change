package gosubscriptionchange

import "time"

// 支付结果（由支付网关同步返回，本服务记录其终态）。
const (
	PaymentSucceeded = "SUCCEEDED"
	PaymentFailed    = "FAILED"
)

// Payment 是一次支付尝试的持久化记录。
// 预约类变更（差额为正）必须在生效时间前完成支付确认：
//   - 支付失败可换新支付号重试，同一变更至多一笔 SUCCEEDED；
//   - 支付号（clientToken）全局唯一：同号同内容重试原样返回，同号不同结果/金额报冲突；
//   - 变更已不在 PENDING（取消/替换/终止/生效/失败）时，迟到的支付被拒绝，
//     绝不可能把已关闭的变更重新激活。
type Payment struct {
	ID             string    `json:"id"`
	SubscriptionID string    `json:"subscriptionId"`
	ChangeID       string    `json:"changeId"`
	ClientToken    string    `json:"clientToken,omitempty"` // 支付号，全局唯一（幂等/冲突检测）
	Amount         int64     `json:"amount"`                // 必须等于变更冻结的差额
	Currency       string    `json:"currency"`
	Status         string    `json:"status"` // SUCCEEDED / FAILED
	FailureReason  string    `json:"failureReason,omitempty"`
	CreatedAt      time.Time `json:"createdAt"`
}
