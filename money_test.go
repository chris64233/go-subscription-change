package gosubscriptionchange

import (
	"testing"
	"time"
)

func mustDate(t *testing.T, s string) time.Time {
	t.Helper()
	tt, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return tt
}

func TestPeriodDayInfo(t *testing.T) {
	start := mustDate(t, "2026-01-01T00:00:00Z")
	end := mustDate(t, "2026-02-01T00:00:00Z")

	cases := []struct {
		name                      string
		at                        string
		total, elapsed, remaining int
	}{
		{"at start", "2026-01-01T00:00:00Z", 31, 0, 31},
		{"midday start day counts as unused", "2026-01-01T12:00:00Z", 31, 0, 31},
		{"day 10 start", "2026-01-11T00:00:00Z", 31, 10, 21},
		{"day 10 noon", "2026-01-11T09:30:00Z", 31, 10, 21},
		{"last day", "2026-01-31T23:59:00Z", 31, 30, 1},
		{"at boundary", "2026-02-01T00:00:00Z", 31, 31, 0},
		{"after boundary", "2026-02-05T00:00:00Z", 31, 31, 0},
		{"before start", "2025-12-31T00:00:00Z", 31, 0, 31},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tot, el, rem := periodDayInfo(start, end, mustDate(t, c.at))
			if tot != c.total || el != c.elapsed || rem != c.remaining {
				t.Fatalf("got total=%d elapsed=%d remaining=%d, want %d/%d/%d",
					tot, el, rem, c.total, c.elapsed, c.remaining)
			}
			if el+rem != tot {
				t.Fatalf("elapsed+remaining = %d, want total %d", el+rem, tot)
			}
		})
	}
}

func TestComputeImmediateSettlement(t *testing.T) {
	start := mustDate(t, "2026-01-01T00:00:00Z")
	end := mustDate(t, "2026-02-01T00:00:00Z")
	at := mustDate(t, "2026-01-11T00:00:00Z") // 已过10天，剩21天

	// 升级 1000 -> 4100：差额3100 * 21/31 = 2100
	sp := computeImmediateSettlement(1000, 4100, start, end, at)
	if sp.amount != 2100 || sp.totalDays != 31 || sp.remainDays != 21 {
		t.Fatalf("upgrade: got amount=%d total=%d rem=%d", sp.amount, sp.totalDays, sp.remainDays)
	}

	// 降级 4100 -> 1000：净额 -2100
	sp = computeImmediateSettlement(4100, 1000, start, end, at)
	if sp.amount != -2100 {
		t.Fatalf("downgrade: got %d want -2100", sp.amount)
	}

	// 同价：0
	sp = computeImmediateSettlement(1000, 1000, start, end, at)
	if sp.amount != 0 {
		t.Fatalf("same price: got %d want 0", sp.amount)
	}

	// 边界当天：剩余0天 -> 0
	sp = computeImmediateSettlement(1000, 4100, start, end, end)
	if sp.amount != 0 {
		t.Fatalf("at boundary: got %d want 0", sp.amount)
	}

	// 周期起点：整月差额
	sp = computeImmediateSettlement(1000, 4100, start, end, start)
	if sp.amount != 3100 {
		t.Fatalf("at period start: got %d want 3100", sp.amount)
	}
}

func TestRoundHalfUp(t *testing.T) {
	cases := []struct {
		in   float64
		want int64
	}{
		{0.5, 1}, {1.5, 2}, {2.5, 3}, {-0.5, -1}, {-1.5, -2},
		{1.49, 1}, {-1.49, -1}, {0, 0},
	}
	for _, c := range cases {
		if got := roundHalfUp(c.in); got != c.want {
			t.Errorf("roundHalfUp(%v) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestPriceAtUsesEffectiveBoundaries(t *testing.T) {
	p := &Plan{Prices: []PriceVersion{
		{Version: 1, Amount: 100, EffectiveAt: mustDate(t, "2026-01-01T00:00:00Z")},
		{Version: 2, Amount: 200, EffectiveAt: mustDate(t, "2026-02-01T00:00:00Z")},
		{Version: 3, Amount: 300, EffectiveAt: mustDate(t, "2026-03-01T00:00:00Z")},
	}}
	cases := []struct {
		at     string
		ver    int
		amount int64
	}{
		{"2026-01-15T00:00:00Z", 1, 100},
		{"2026-01-31T23:59:59Z", 1, 100},
		{"2026-02-01T00:00:00Z", 2, 200},
		{"2026-02-20T00:00:00Z", 2, 200},
		{"2026-03-01T00:00:00Z", 3, 300},
		{"2026-12-01T00:00:00Z", 3, 300},
	}
	for _, c := range cases {
		pv, ok := p.priceAt(mustDate(t, c.at))
		if !ok || pv.Version != c.ver || pv.Amount != c.amount {
			t.Errorf("at %s got v%d=%d (ok=%v), want v%d=%d", c.at, pv.Version, pv.Amount, ok, c.ver, c.amount)
		}
	}
	if _, ok := (&Plan{}).priceAt(mustDate(t, "2026-01-01T00:00:00Z")); ok {
		t.Fatal("empty plan should have no effective price")
	}
}
