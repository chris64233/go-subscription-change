package gosubscriptionchange

// 金额一律以整数“最小货币单位”（分）存储，全程不使用浮点，避免二进制误差。
// 账单差额 amount 为带符号整数：正数=应补收，负数=应退还。

// prorateAmount 以纯整数运算计算 roundHalfUp(diff × part / whole)，
// 对绝对值四舍五入并保留 diff 的符号：
//
//	roundHalfUp(n/d) = floor((2n + d) / 2d)   （n >= 0, d > 0）
//
// 不经过浮点，任何金额与天数组合都不会引入二进制误差。
// whole <= 0 时退化为整体差额（调用方保证只在周期退化时出现）。
func prorateAmount(diff, part, whole int64) int64 {
	if whole <= 0 {
		return diff
	}
	n := diff
	neg := n < 0
	if neg {
		n = -n
	}
	v := (2*n*part + whole) / (2 * whole)
	if neg {
		return -v
	}
	return v
}
