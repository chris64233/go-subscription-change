package gosubscriptionchange

import "time"

// 订阅状态。
const (
	SubActive     = "ACTIVE"     // 正常
	SubTerminated = "TERMINATED" // 已终止：套餐不能再切换
)

// Subscription 是一条订阅。Version 单调递增，用于乐观并发控制：
// 任何更新都必须带期望值，防止并发修改静默互相覆盖。
type Subscription struct {
	ID              string    `json:"id"`
	CurrentPlanID   string    `json:"currentPlanId"`
	Currency        string    `json:"currency"`
	PeriodStart     time.Time `json:"periodStart"` // 当前计费周期起点（UTC）
	PeriodEnd       time.Time `json:"periodEnd"`   // 当前计费周期边界（UTC，开区间）
	Status          string    `json:"status"`
	PendingChangeID string    `json:"pendingChangeId,omitempty"`
	Version         int64     `json:"version"`
	CreatedAt       time.Time `json:"createdAt"`
	TerminatedAt    time.Time `json:"terminatedAt,omitempty"`
}

// dayStart 将时间归一化到 UTC 当日零点（按日比例摊分时使用日历日）。
func dayStart(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

// nextMonthlyBoundary 返回 from 之后一个计费周期的边界：次月同一日历日；
// 当次月没有该日（如 1 月 31 日的次月）钳制到次月最后一天（2 月 28 日）。
func nextMonthlyBoundary(from time.Time) time.Time {
	d := dayStart(from)
	// 次月一号；再取下下月一号回退一天，得到“次月”的天数。
	firstOfNext := time.Date(d.Year(), d.Month()+1, 1, 0, 0, 0, 0, time.UTC)
	firstAfterNext := time.Date(d.Year(), d.Month()+2, 1, 0, 0, 0, 0, time.UTC)
	daysInNext := firstAfterNext.AddDate(0, 0, -1).Day()
	day := d.Day()
	if day > daysInNext {
		day = daysInNext
	}
	return time.Date(firstOfNext.Year(), firstOfNext.Month(), day, 0, 0, 0, 0, time.UTC)
}

// periodDayInfo 返回计费周期内：
//
//	total     —— 周期日历日总数
//	elapsed   —— 到 at 为止已经过的日历日（不含 at 当天，当天按“未使用”退给用户）
//	remaining —— at 当天及之后的剩余日历日
//
// 按日历日分摊：elapsed + remaining == total。
func periodDayInfo(start, end, at time.Time) (total, elapsed, remaining int) {
	s := dayStart(start)
	e := dayStart(end)
	total = int(e.Sub(s).Hours() / 24)

	cur := dayStart(at)
	if !cur.After(s) {
		return total, 0, total
	}
	if !cur.Before(e) {
		return total, total, 0
	}
	elapsed = int(cur.Sub(s).Hours() / 24)
	remaining = total - elapsed
	return
}
