// Package gosubscriptionchange 实现订阅套餐的预约变更与生效结算。
//
// 业务用例由 [Service] 提供，数据落在 [Store]（线程安全的内存快照事务仓储），
// 对外通过 [NewAPI] 暴露 JSON HTTP 接口。
//
// 关键不变量：
//   - 变更受理时冻结原/新套餐价格版本、计费周期与差额金额，之后调价不改写已受理申请；
//   - 同一订阅至多一笔 PENDING 变更，替换使用期望申请 ID 做乐观并发控制；
//   - SCHEDULED 在指定的未来生效时间（当前周期内）切换，生效时间同样在受理时冻结；
//   - 预约类变更（NEXT_PERIOD / SCHEDULED）差额为正时须先生效前支付确认，
//     到期未支付置 FAILED（终态）并保留原套餐；迟到的支付不能激活已关闭的变更；
//   - 到期推进在单事务内切换套餐、写差额账单、生成唯一通知，账单与通知以 changeID 唯一；
//   - 订阅终止后套餐不再切换，已生效变更不会被迟到取消还原。
//
// 计费规则见 [computeImmediateSettlement] 与 [computeNextPeriodSettlement]
// （SCHEDULED 复用按日比例分摊，仅把基准时点从受理时刻换成冻结的生效时刻）。
package gosubscriptionchange
