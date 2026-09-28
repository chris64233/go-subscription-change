package gosubscriptionchange

import "math"

// 金额一律以整数“最小货币单位”（分）存储，全程不使用浮点，避免二进制误差。
// 账单差额 amount 为带符号整数：正数=应补收，负数=应退还。

// roundHalfUp 对绝对值做四舍五入并保留原符号，用于按比例拆分金额。
func roundHalfUp(x float64) int64 {
	if x >= 0 {
		return int64(math.Floor(x + 0.5))
	}
	return -int64(math.Floor(-x + 0.5))
}
