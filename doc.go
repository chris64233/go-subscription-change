// Package gosubscriptionchange 实现订阅套餐的预约变更与生效结算。
//
// 业务用例由 [Service] 提供，数据落在 [Store]（线程安全的内存快照事务仓储），
// 对外通过 [NewAPI] 暴露 JSON HTTP 接口。
//
// 关键不变量：
//   - 变更受理时冻结原/新套餐价格版本、计费周期与差额金额，之后调价不改写已受理申请；
//   - 同一订阅至多一笔 PENDING 变更，替换使用期望申请 ID 做乐观并发控制；
//   - 到期推进在单事务内切换套餐、写差额账单、生成唯一通知，账单与通知以 changeID 唯一；
//   - 订阅终止后套餐不再切换，已生效变更不会被迟到取消还原。
//
// 计费规则见 [computeImmediateSettlement] 与 [computeNextPeriodSettlement]。
package gosubscriptionchange
