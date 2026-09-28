package subscription

import (
	"math/big"
	"testing"
	"time"
)

func TestRatHalfUpCents(t *testing.T) {
	cases := []struct {
		num, den int64
		want     int64
	}{
		{1, 2, 1},   // 0.5 → 1（半额向上）
		{3, 2, 2},   // 1.5 → 2
		{1, 3, 0},   // 0.333 → 0
		{2, 3, 1},   // 0.667 → 1
		{-1, 2, -1}, // -0.5 → -1（对称，远离零）
		{-2, 3, -1}, // -0.667 → -1
		{-1, 3, 0},
		{10, 2, 5},
	}
	for _, c := range cases {
		got := ratHalfUpCents(new(big.Rat).SetFrac(big.NewInt(c.num), big.NewInt(c.den)))
		if got != c.want {
			t.Errorf("ratHalfUpCents(%d/%d) = %d, want %d", c.num, c.den, got, c.want)
		}
	}
}

func TestProrationExact(t *testing.T) {
	// 月周期 31 天，剩余 16 天；新 6000 / 旧 3000（分）
	// 差额 = 3000*16/31 = 1548.387… → 1548
	if got := proration(3000, 6000, 15, 16, 31); got != 1548 {
		t.Fatalf("charge proration = %d, want 1548", got)
	}
	// 反向降级：应为退款 -1548（与升级绝对值一致，仅符号不同）
	if got := proration(6000, 3000, 15, 16, 31); got != -1548 {
		t.Fatalf("refund proration = %d, want -1548", got)
	}
	// 同价：差额恒为 0
	if got := proration(3000, 3000, 1, 30, 31); got != 0 {
		t.Fatalf("same-price proration = %d, want 0", got)
	}
	// 剩余 0：不退不补
	if got := proration(1000, 2000, 31, 0, 31); got != 0 {
		t.Fatalf("zero-remain proration = %d, want 0", got)
	}
	// 精确半分向上：(1)*1/2 = 0.5 → 1
	if got := proration(0, 1, 1, 1, 2); got != 1 {
		t.Fatalf("half-cent proration = %d, want 1", got)
	}
}

func TestAddBillingCycleCalendarClamp(t *testing.T) {
	loc := time.UTC
	jan31 := time.Date(2026, 1, 31, 0, 0, 0, 0, loc)
	if got := AddBillingCycle(jan31, CycleMonthly); !got.Equal(time.Date(2026, 2, 28, 0, 0, 0, 0, loc)) {
		t.Fatalf("jan31 + month = %v, want 2026-02-28", got)
	}
	if got := AddBillingCycle(jan31, CycleYearly); !got.Equal(time.Date(2027, 1, 31, 0, 0, 0, 0, loc)) {
		t.Fatalf("jan31 + year = %v", got)
	}
	if got := AddBillingCycle(jan31, CycleWeekly); !got.Equal(time.Date(2026, 2, 7, 0, 0, 0, 0, loc)) {
		t.Fatalf("jan31 + week = %v", got)
	}
}

func TestPlanVersionAtFreeze(t *testing.T) {
	now := time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)
	p := &Plan{ID: "p", Cycle: CycleMonthly, Versions: []PlanVersion{
		{Version: 1, PriceCents: 1000, Effective: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		{Version: 2, PriceCents: 2000, Effective: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)},
	}}
	if v := p.VersionAt(now); v.Version != 1 || v.PriceCents != 1000 {
		t.Fatalf("VersionAt(Jan10) = v%d/%d, want v1/1000", v.Version, v.PriceCents)
	}
	if v := p.VersionAt(time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)); v.Version != 2 {
		t.Fatalf("VersionAt(Feb1) = v%d, want v2", v.Version)
	}
}
