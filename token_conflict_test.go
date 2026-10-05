package gosubscriptionchange

import (
	"errors"
	"testing"
)

// ---- 相同变更号：套餐 / 价格 / 生效时间变化报冲突 ----

func TestClientTokenConflictOnPriceAndTime(t *testing.T) {
	f := newFixture(t, "2026-01-10T00:00:00Z")
	f.createPlan("a", "A", 1000)
	f.createPlan("b", "B", 2000)
	f.createSub("s1", "a")
	effAt := mustDate(t, "2026-01-20T00:00:00Z")

	first, err := f.svc.ApplyChange(f.ctx, ChangeInput{
		SubscriptionID: "s1", ClientToken: "req-9",
		TargetPlanID: "b", EffectiveType: EffectiveScheduled, EffectiveAt: effAt,
	})
	if err != nil {
		t.Fatal(err)
	}

	// 完全相同的重试：返回原申请。
	retry, err := f.svc.ApplyChange(f.ctx, ChangeInput{
		SubscriptionID: "s1", ClientToken: "req-9",
		TargetPlanID: "b", EffectiveType: EffectiveScheduled, EffectiveAt: effAt,
	})
	if err != nil || retry.ID != first.ID {
		t.Fatalf("idempotent retry = %v/%v", retry, err)
	}
	// 同号但生效时间不同：冲突。
	_, err = f.svc.ApplyChange(f.ctx, ChangeInput{
		SubscriptionID: "s1", ClientToken: "req-9",
		TargetPlanID: "b", EffectiveType: EffectiveScheduled,
		EffectiveAt: mustDate(t, "2026-01-21T00:00:00Z"),
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("different time err = %v want ErrConflict", err)
	}
	// 同号但声明的期望金额不同：冲突。
	wrong := first.SettlementAmount + 1
	_, err = f.svc.ApplyChange(f.ctx, ChangeInput{
		SubscriptionID: "s1", ClientToken: "req-9",
		TargetPlanID: "b", EffectiveType: EffectiveScheduled, EffectiveAt: effAt,
		ExpectedAmount: &wrong,
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("different amount err = %v want ErrConflict", err)
	}
	// 同号带一致的期望金额：幂等返回。
	ok := first.SettlementAmount
	same, err := f.svc.ApplyChange(f.ctx, ChangeInput{
		SubscriptionID: "s1", ClientToken: "req-9",
		TargetPlanID: "b", EffectiveType: EffectiveScheduled, EffectiveAt: effAt,
		ExpectedAmount: &ok,
	})
	if err != nil || same.ID != first.ID {
		t.Fatalf("retry with same amount = %v/%v", same, err)
	}
}

func TestExpectedAmountGuardsPriceChange(t *testing.T) {
	f := newFixture(t, "2026-01-10T00:00:00Z")
	f.createPlan("a", "A", 1000)
	f.createPlan("b", "B", 2000)
	f.createSub("s1", "a")

	// 调用方按旧报价声明期望金额；价格被改写后受理必须冲突而不是静默按新价冻结。
	stale := int64(500)
	f.addPrice("b", 3000, "2026-01-11T00:00:00Z")
	f.clock.Set(mustDate(t, "2026-01-12T00:00:00Z"))
	_, err := f.svc.ApplyChange(f.ctx, ChangeInput{
		SubscriptionID: "s1", ClientToken: "q-1",
		TargetPlanID: "b", EffectiveType: EffectiveNextPeriod, ExpectedAmount: &stale,
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("stale quote err = %v want ErrConflict", err)
	}
	// 重新报价后（当前差额 3000）同一申请号可正常受理。
	cur := int64(3000)
	ch, err := f.svc.ApplyChange(f.ctx, ChangeInput{
		SubscriptionID: "s1", ClientToken: "q-1",
		TargetPlanID: "b", EffectiveType: EffectiveNextPeriod, ExpectedAmount: &cur,
	})
	if err != nil {
		t.Fatalf("apply with fresh quote: %v", err)
	}
	if ch.SettlementAmount != 3000 {
		t.Fatalf("settlement = %d want 3000", ch.SettlementAmount)
	}
}
