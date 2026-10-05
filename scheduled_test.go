package gosubscriptionchange

import (
	"errors"
	"testing"
)

// ---- SCHEDULED：按指定生效时间切换 ----

func TestScheduledChangeFreezeAndBoundary(t *testing.T) {
	// 周期 1/1~2/1（31 天），1/10 受理，预约 1/20 生效（剩 12 天）。
	f := newFixture(t, "2026-01-01T00:00:00Z")
	f.createPlan("basic", "Basic", 1000)
	f.createPlan("pro", "Pro", 4100)
	sub := f.createSub("s1", "basic")
	f.clock.Set(mustDate(t, "2026-01-10T08:00:00Z"))

	effAt := mustDate(t, "2026-01-20T00:00:00Z")
	ch, err := f.svc.ApplyChange(f.ctx, ChangeInput{
		SubscriptionID: "s1", ClientToken: "sch-1",
		TargetPlanID: "pro", EffectiveType: EffectiveScheduled, EffectiveAt: effAt,
	})
	if err != nil {
		t.Fatalf("apply scheduled: %v", err)
	}
	// (4100-1000)*12/31 = 1200，按生效时点冻结。
	if ch.SettlementAmount != 1200 {
		t.Fatalf("settlement = %d want 1200", ch.SettlementAmount)
	}
	if !ch.EffectiveAt.Equal(effAt) || ch.Status != ChangePending {
		t.Fatalf("effectiveAt=%s status=%s", ch.EffectiveAt, ch.Status)
	}

	// 受理后调价不得改写待生效金额。
	f.addPrice("pro", 8888, "2026-01-12T00:00:00Z")
	got, _ := f.svc.GetChange(f.ctx, ch.ID)
	if got.SettlementAmount != 1200 || got.ToPlan.Amount != 4100 {
		t.Fatalf("scheduled change rewritten by new price: %+v", got)
	}

	// 时间边界：生效前 1 秒推进 -> 400；到达生效时刻整点 -> 可推进。
	f.clock.Set(effAt.Add(-1))
	if _, err := f.svc.AdvanceSubscription(f.ctx, "s1"); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("advance 1s early err = %v want ErrInvalidArgument", err)
	}
	f.mustPay(ch.ID, "pay-sch", 1200)
	f.clock.Set(effAt)
	res, err := f.svc.AdvanceSubscription(f.ctx, "s1")
	if err != nil {
		t.Fatalf("advance at effective time: %v", err)
	}
	if res.Bill == nil || res.Bill.Amount != 1200 {
		t.Fatalf("bill = %+v want 1200", res.Bill)
	}
	sub2, _ := f.svc.GetSubscription(f.ctx, "s1")
	if sub2.CurrentPlanID != "pro" {
		t.Fatalf("plan = %s want pro", sub2.CurrentPlanID)
	}
	// 周期内切换：计费周期保持不变。
	if !sub2.PeriodStart.Equal(sub.PeriodStart) || !sub2.PeriodEnd.Equal(sub.PeriodEnd) {
		t.Fatalf("scheduled switch must keep period: %s~%s", sub2.PeriodStart, sub2.PeriodEnd)
	}
}

func TestScheduledChangeValidation(t *testing.T) {
	f := newFixture(t, "2026-01-10T00:00:00Z")
	f.createPlan("a", "A", 1000)
	f.createPlan("b", "B", 2000)
	sub := f.createSub("s1", "a")

	cases := []struct {
		name string
		in   ChangeInput
	}{
		{"missing effectiveAt", ChangeInput{SubscriptionID: "s1", TargetPlanID: "b", EffectiveType: EffectiveScheduled}},
		{"past effectiveAt", ChangeInput{SubscriptionID: "s1", TargetPlanID: "b", EffectiveType: EffectiveScheduled,
			EffectiveAt: mustDate(t, "2026-01-09T00:00:00Z")}},
		{"effectiveAt == now", ChangeInput{SubscriptionID: "s1", TargetPlanID: "b", EffectiveType: EffectiveScheduled,
			EffectiveAt: mustDate(t, "2026-01-10T00:00:00Z")}},
		{"effectiveAt at period end", ChangeInput{SubscriptionID: "s1", TargetPlanID: "b", EffectiveType: EffectiveScheduled,
			EffectiveAt: sub.PeriodEnd}},
		{"effectiveAt beyond period", ChangeInput{SubscriptionID: "s1", TargetPlanID: "b", EffectiveType: EffectiveScheduled,
			EffectiveAt: sub.PeriodEnd.AddDate(0, 0, 1)}},
		{"effectiveAt on IMMEDIATE", ChangeInput{SubscriptionID: "s1", TargetPlanID: "b", EffectiveType: EffectiveImmediate,
			EffectiveAt: mustDate(t, "2026-01-15T00:00:00Z")}},
		{"effectiveAt on NEXT_PERIOD", ChangeInput{SubscriptionID: "s1", TargetPlanID: "b", EffectiveType: EffectiveNextPeriod,
			EffectiveAt: mustDate(t, "2026-01-15T00:00:00Z")}},
		{"unknown type", ChangeInput{SubscriptionID: "s1", TargetPlanID: "b", EffectiveType: "SOMEDAY"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := f.svc.ApplyChange(f.ctx, c.in); !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("err = %v want ErrInvalidArgument", err)
			}
		})
	}
	// 全部被拒绝：订阅上没有留下任何待生效变更。
	got, _ := f.svc.GetSubscription(f.ctx, "s1")
	if got.PendingChangeID != "" {
		t.Fatalf("rejected applies left pending change %s", got.PendingChangeID)
	}
}

// ---- 到期未支付：切换失败、保留原套餐、记录原因 ----

func TestScheduledUnpaidAtEffectiveTimeFails(t *testing.T) {
	f := newFixture(t, "2026-01-10T00:00:00Z")
	f.createPlan("a", "A", 1000)
	f.createPlan("b", "B", 2000)
	f.createSub("s1", "a")
	ch, err := f.svc.ApplyChange(f.ctx, ChangeInput{
		SubscriptionID: "s1", TargetPlanID: "b", EffectiveType: EffectiveScheduled,
		EffectiveAt: mustDate(t, "2026-01-20T00:00:00Z"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if ch.SettlementAmount <= 0 {
		t.Fatalf("test requires positive settlement, got %d", ch.SettlementAmount)
	}

	// 到期未支付推进：切换失败，保留原套餐。
	f.clock.Set(mustDate(t, "2026-01-20T00:00:00Z"))
	res, err := f.svc.AdvanceSubscription(f.ctx, "s1")
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if res.Change.Status != ChangeFailed || res.Change.FailureReason == "" {
		t.Fatalf("change = %+v want FAILED with reason", res.Change)
	}
	if res.Bill != nil || res.Notification != nil {
		t.Fatal("failed change must not bill or notify")
	}
	sub, _ := f.svc.GetSubscription(f.ctx, "s1")
	if sub.CurrentPlanID != "a" || sub.PendingChangeID != "" {
		t.Fatalf("original plan must be kept, sub=%+v", sub)
	}
	if len(f.bills("s1")) != 0 {
		t.Fatal("failed change must not bill")
	}
	// 失败原因写入历史。
	events, _ := f.svc.ListHistory(f.ctx, "s1")
	last := events[len(events)-1]
	if last.Type != EventChangeFailed {
		t.Fatalf("last event = %s want %s", last.Type, EventChangeFailed)
	}

	// FAILED 是终态：迟到的支付/取消/推进都不能改写，已生效结果不会被旧任务回滚。
	if _, err := f.svc.SubmitPayment(f.ctx, ch.ID, PaymentInput{Amount: ch.SettlementAmount, Result: PaymentSucceeded}); !errors.Is(err, ErrNotPending) {
		t.Fatalf("late pay err = %v want ErrNotPending", err)
	}
	if _, err := f.svc.CancelChange(f.ctx, "s1"); !errors.Is(err, ErrNotPending) {
		t.Fatalf("cancel after fail err = %v want ErrNotPending", err)
	}
	if _, err := f.svc.AdvanceChange(f.ctx, ch.ID); !errors.Is(err, ErrNotPending) {
		t.Fatalf("re-advance err = %v want ErrNotPending", err)
	}
	// SweepDue 不会反复处理已失败申请。
	if got := f.svc.SweepDue(f.ctx); len(got) != 0 {
		t.Fatalf("sweep picked failed change: %d results", len(got))
	}
	// 失败后可以申请新变更（原套餐仍是 a）。
	ch2 := f.mustChange("s1", "retry-1", "b", EffectiveImmediate)
	if ch2.FromPlan.PlanID != "a" {
		t.Fatalf("from plan = %s want a", ch2.FromPlan.PlanID)
	}
}
