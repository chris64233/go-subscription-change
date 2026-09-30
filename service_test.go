package gosubscriptionchange

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

// ---- 测试夹具 ----

type fixture struct {
	t     *testing.T
	svc   *Service
	store *Store
	clock *FakeClock
	ctx   context.Context
}

func newFixture(t *testing.T, now string) *fixture {
	t.Helper()
	clk := NewFakeClock(mustDate(t, now))
	st := NewStore()
	return &fixture{
		t:     t,
		svc:   NewService(st),
		store: st,
		clock: clk,
		ctx:   withClock(context.Background(), clk),
	}
}

// createPlan 创建套餐，price 为首期价格。
func (f *fixture) createPlan(id, name string, price int64) *Plan {
	f.t.Helper()
	p, err := f.svc.CreatePlan(f.ctx, CreatePlanInput{ID: id, Name: name, Currency: "CNY", Price: price})
	if err != nil {
		f.t.Fatalf("create plan %s: %v", id, err)
	}
	return p
}

func (f *fixture) addPrice(planID string, price int64, eff string) {
	f.t.Helper()
	if _, err := f.svc.AddPlanPrice(f.ctx, planID, price, mustDate(f.t, eff)); err != nil {
		f.t.Fatalf("add price %s: %v", planID, err)
	}
}

func (f *fixture) createSub(id, planID string) *Subscription {
	f.t.Helper()
	s, err := f.svc.CreateSubscription(f.ctx, CreateSubscriptionInput{ID: id, PlanID: planID})
	if err != nil {
		f.t.Fatalf("create sub %s: %v", id, err)
	}
	return s
}

func (f *fixture) mustChange(sub, token, target, effType string) *ChangeApplication {
	f.t.Helper()
	ch, err := f.svc.ApplyChange(f.ctx, ChangeInput{SubscriptionID: sub, ClientToken: token, TargetPlanID: target, EffectiveType: effType})
	if err != nil {
		f.t.Fatalf("apply change %s->%s %s: %v", sub, target, effType, err)
	}
	return ch
}

func (f *fixture) bills(sub string) []*Bill {
	f.t.Helper()
	bs, err := f.svc.ListBills(f.ctx, sub)
	if err != nil {
		f.t.Fatalf("list bills: %v", err)
	}
	return bs
}

// ---- 1. 立即变更：按日精确补/退，且冻结价格不受后续调价影响 ----

func TestImmediateChangeProrationAndFreeze(t *testing.T) {
	// 订阅 1/1 开通（周期 1/1~2/1，31天），1/11 受理变更（剩21天）。
	f := newFixture(t, "2026-01-01T00:00:00Z")
	f.createPlan("basic", "Basic", 1000)
	f.createPlan("pro", "Pro", 4100)
	sub := f.createSub("s1", "basic")
	f.clock.Set(mustDate(t, "2026-01-11T00:00:00Z"))

	ch := f.mustChange("s1", "", "pro", EffectiveImmediate)
	if ch.Status != ChangePending {
		t.Fatalf("status = %s", ch.Status)
	}
	// (4100-1000)*21/31 = 2100
	if ch.SettlementAmount != 2100 {
		t.Fatalf("settlement = %d want 2100", ch.SettlementAmount)
	}
	if ch.FromPlan.Version != 1 || ch.ToPlan.Version != 1 {
		t.Fatalf("frozen versions = %d/%d want 1/1", ch.FromPlan.Version, ch.ToPlan.Version)
	}

	// 受理后两个套餐都调价 —— 不得改写已受理申请。
	f.addPrice("basic", 9999, "2026-01-15T00:00:00Z")
	f.addPrice("pro", 8888, "2026-01-15T00:00:00Z")
	got, err := f.svc.GetChange(f.ctx, ch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.FromPlan.Amount != 1000 || got.ToPlan.Amount != 4100 || got.SettlementAmount != 2100 {
		t.Fatalf("accepted change was rewritten by new prices: %+v", got)
	}

	// 推进生效。
	res, err := f.svc.AdvanceSubscription(f.ctx, "s1")
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if res.Bill.Amount != 2100 || res.Bill.ChangeID != ch.ID {
		t.Fatalf("bill = %+v", res.Bill)
	}
	if res.Notification.ChangeID != ch.ID || res.Notification.Type != NotificationChangeEffective {
		t.Fatalf("notification = %+v", res.Notification)
	}
	sub2, _ := f.svc.GetSubscription(f.ctx, "s1")
	if sub2.CurrentPlanID != "pro" || sub2.PeriodStart != sub.PeriodStart || sub2.PeriodEnd != sub.PeriodEnd {
		t.Fatalf("immediate change must keep current period, sub=%+v", sub2)
	}
	if len(f.bills("s1")) != 1 {
		t.Fatalf("exactly one bill expected")
	}
}

func TestImmediateDowngradeIsRefund(t *testing.T) {
	f := newFixture(t, "2026-01-01T00:00:00Z")
	f.createPlan("pro", "Pro", 4100)
	f.createPlan("basic", "Basic", 1000)
	f.createSub("s1", "pro")
	f.clock.Set(mustDate(t, "2026-01-11T00:00:00Z"))
	ch := f.mustChange("s1", "", "basic", EffectiveImmediate)
	if ch.SettlementAmount != -2100 {
		t.Fatalf("refund amount = %d want -2100", ch.SettlementAmount)
	}
	res, err := f.svc.AdvanceSubscription(f.ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if res.Bill.Amount != -2100 {
		t.Fatalf("bill amount = %d want -2100", res.Bill.Amount)
	}
}

// ---- 2. 下一周期变更：边界才收费，按冻结新价且周期推进 ----

func TestNextPeriodChangeSettlesAtBoundary(t *testing.T) {
	// 1/15 受理，下一周期边界 2/15。
	f := newFixture(t, "2026-01-15T12:00:00Z")
	f.createPlan("basic", "Basic", 1000)
	f.createPlan("pro", "Pro", 3000)
	sub := f.createSub("s1", "basic")

	ch := f.mustChange("s1", "t-1", "pro", EffectiveNextPeriod)
	if !ch.EffectiveAt.Equal(sub.PeriodEnd) {
		t.Fatalf("effectiveAt=%s want %s", ch.EffectiveAt, sub.PeriodEnd)
	}
	if ch.SettlementAmount != 3000 {
		t.Fatalf("next-period settlement = %d want 3000", ch.SettlementAmount)
	}

	// 到期前推进：报错，不产生任何账单。
	if _, err := f.svc.AdvanceSubscription(f.ctx, "s1"); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("early advance err = %v want ErrInvalidArgument", err)
	}
	if len(f.bills("s1")) != 0 {
		t.Fatal("no bill before boundary")
	}

	// 受理后新套餐涨价到 5000（边界前生效）—— 已受理申请仍按冻结价 3000。
	f.addPrice("pro", 5000, "2026-01-20T00:00:00Z")

	f.clock.Set(mustDate(t, "2026-02-15T00:00:00Z"))
	res, err := f.svc.AdvanceSubscription(f.ctx, "s1")
	if err != nil {
		t.Fatalf("advance at boundary: %v", err)
	}
	if res.Bill.Amount != 3000 {
		t.Fatalf("bill = %d want frozen 3000", res.Bill.Amount)
	}
	sub2, _ := f.svc.GetSubscription(f.ctx, "s1")
	if sub2.CurrentPlanID != "pro" {
		t.Fatalf("plan = %s want pro", sub2.CurrentPlanID)
	}
	if !sub2.PeriodStart.Equal(mustDate(t, "2026-02-15T00:00:00Z")) ||
		!sub2.PeriodEnd.Equal(mustDate(t, "2026-03-15T00:00:00Z")) {
		t.Fatalf("period not advanced: %s~%s", sub2.PeriodStart, sub2.PeriodEnd)
	}
}

// ---- 3. 同一订阅只能保留一笔待生效变更 ----

func TestSinglePendingChange(t *testing.T) {
	f := newFixture(t, "2026-01-10T00:00:00Z")
	f.createPlan("a", "A", 1000)
	f.createPlan("b", "B", 2000)
	f.createPlan("c", "C", 3000)
	f.createSub("s1", "a")

	f.mustChange("s1", "", "b", EffectiveImmediate)
	_, err := f.svc.ApplyChange(f.ctx, ChangeInput{SubscriptionID: "s1", TargetPlanID: "c", EffectiveType: EffectiveImmediate})
	if !errors.Is(err, ErrPendingChangeExist) {
		t.Fatalf("second apply err = %v want ErrPendingChangeExist", err)
	}
}

func TestSamePlanRejected(t *testing.T) {
	f := newFixture(t, "2026-01-10T00:00:00Z")
	f.createPlan("a", "A", 1000)
	f.createSub("s1", "a")
	_, err := f.svc.ApplyChange(f.ctx, ChangeInput{SubscriptionID: "s1", TargetPlanID: "a", EffectiveType: EffectiveImmediate})
	if !errors.Is(err, ErrSamePlan) {
		t.Fatalf("err = %v want ErrSamePlan", err)
	}
}

// ---- 4. 外部申请号：幂等与冲突 ----

func TestClientTokenIdempotencyAndConflict(t *testing.T) {
	f := newFixture(t, "2026-01-10T00:00:00Z")
	f.createPlan("a", "A", 1000)
	f.createPlan("b", "B", 2000)
	f.createPlan("c", "C", 3000)
	f.createSub("s1", "a")
	f.createSub("s2", "a")

	first := f.mustChange("s1", "req-1", "b", EffectiveImmediate)

	// 完全相同的重试：返回同一申请，不报错，不新建。
	retry, err := f.svc.ApplyChange(f.ctx, ChangeInput{SubscriptionID: "s1", ClientToken: "req-1", TargetPlanID: "b", EffectiveType: EffectiveImmediate})
	if err != nil {
		t.Fatalf("idempotent retry: %v", err)
	}
	if retry.ID != first.ID {
		t.Fatalf("retry created %s, want %s", retry.ID, first.ID)
	}

	// 同号不同目标套餐：冲突。
	_, err = f.svc.ApplyChange(f.ctx, ChangeInput{SubscriptionID: "s1", ClientToken: "req-1", TargetPlanID: "c", EffectiveType: EffectiveImmediate})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("different target err = %v want ErrConflict", err)
	}
	// 同号不同生效方式：冲突。
	_, err = f.svc.ApplyChange(f.ctx, ChangeInput{SubscriptionID: "s1", ClientToken: "req-1", TargetPlanID: "b", EffectiveType: EffectiveNextPeriod})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("different type err = %v want ErrConflict", err)
	}
	// 同号绑定到别的订阅：冲突。
	_, err = f.svc.ApplyChange(f.ctx, ChangeInput{SubscriptionID: "s2", ClientToken: "req-1", TargetPlanID: "b", EffectiveType: EffectiveImmediate})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("cross subscription err = %v want ErrConflict", err)
	}
}

// ---- 5. 替换：旧申请 SUPERSEDED，新申请按当前版本重新冻结 ----

func TestReplaceChange(t *testing.T) {
	f := newFixture(t, "2026-01-10T00:00:00Z")
	f.createPlan("a", "A", 1000)
	f.createPlan("b", "B", 2000)
	f.createPlan("c", "C", 3000)
	f.createSub("s1", "a")

	old := f.mustChange("s1", "old-req", "b", EffectiveNextPeriod)

	// 旧申请受理后 c 涨价。
	f.addPrice("c", 3500, "2026-01-12T00:00:00Z")
	f.clock.Set(mustDate(t, "2026-01-13T00:00:00Z"))

	next, err := f.svc.ReplaceChange(f.ctx, ChangeInput{SubscriptionID: "s1", ClientToken: "new-req", TargetPlanID: "c", EffectiveType: EffectiveImmediate})
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	if next.SupersedesChangeID != old.ID {
		t.Fatalf("supersedes = %q want %q", next.SupersedesChangeID, old.ID)
	}
	if next.ToPlan.Amount != 3500 || next.ToPlan.Version != 2 {
		t.Fatalf("new change should freeze current price, got %+v", next.ToPlan)
	}
	// 立即变更差额按 1/13 时点（周期 1/10~2/10，已过3天剩28天）：(3500-1000)*28/31 ≈ 2258.06 -> 2258
	if next.SettlementAmount != 2258 {
		t.Fatalf("recomputed settlement = %d want 2258", next.SettlementAmount)
	}

	oldGot, _ := f.svc.GetChange(f.ctx, old.ID)
	if oldGot.Status != ChangeSuperseded {
		t.Fatalf("old status = %s want SUPERSEDED", oldGot.Status)
	}
	sub, _ := f.svc.GetSubscription(f.ctx, "s1")
	if sub.PendingChangeID != next.ID {
		t.Fatalf("pending pointer = %s want %s", sub.PendingChangeID, next.ID)
	}

	// 没有待生效变更时替换报错。
	f.clock.Set(mustDate(t, "2026-01-13T01:00:00Z"))
	if _, err := f.svc.AdvanceSubscription(f.ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.ReplaceChange(f.ctx, ChangeInput{SubscriptionID: "s1", ClientToken: "x", TargetPlanID: "b", EffectiveType: EffectiveImmediate})
	if !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrNotPending) {
		t.Fatalf("replace after effective err = %v", err)
	}
}

// ---- 6. 取消：生效前可取消；迟到取消不能还原已生效变更 ----

func TestCancelAndLateCancel(t *testing.T) {
	f := newFixture(t, "2026-01-10T00:00:00Z")
	f.createPlan("a", "A", 1000)
	f.createPlan("b", "B", 2000)
	f.createSub("s1", "a")
	ch := f.mustChange("s1", "", "b", EffectiveNextPeriod)

	cancelled, err := f.svc.CancelChange(f.ctx, "s1")
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if cancelled.Status != ChangeCancelled {
		t.Fatalf("status = %s", cancelled.Status)
	}
	sub, _ := f.svc.GetSubscription(f.ctx, "s1")
	if sub.PendingChangeID != "" {
		t.Fatal("pending pointer should be cleared")
	}
	// 再次取消 / 到边界推进：都不得生效。
	if _, err := f.svc.CancelChange(f.ctx, "s1"); !errors.Is(err, ErrNotPending) {
		t.Fatalf("second cancel err = %v", err)
	}
	f.clock.Set(mustDate(t, "2026-02-01T00:00:00Z"))
	if _, err := f.svc.AdvanceChange(f.ctx, ch.ID); !errors.Is(err, ErrNotPending) {
		t.Fatalf("advance cancelled err = %v want ErrNotPending", err)
	}
	sub2, _ := f.svc.GetSubscription(f.ctx, "s1")
	if sub2.CurrentPlanID != "a" {
		t.Fatal("cancelled change must not switch plan")
	}
	if len(f.bills("s1")) != 0 {
		t.Fatal("cancelled change must not bill")
	}
}

// ---- 7. 并发推进：只生效一次，只有一张账单一条通知 ----

func TestConcurrentAdvanceEffectiveOnce(t *testing.T) {
	f := newFixture(t, "2026-01-10T00:00:00Z")
	f.createPlan("a", "A", 1000)
	f.createPlan("b", "B", 2000)
	f.createSub("s1", "a")
	ch := f.mustChange("s1", "tok", "b", EffectiveImmediate)

	const n = 32
	var wg sync.WaitGroup
	var ok, fail int64
	var mu sync.Mutex
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := f.svc.AdvanceSubscription(f.ctx, "s1")
			mu.Lock()
			if err == nil {
				ok++
			} else {
				fail++
			}
			mu.Unlock()
		}()
	}
	close(start)
	wg.Wait()
	if ok != 1 || fail != n-1 {
		t.Fatalf("success=%d fail=%d, want exactly 1 success", ok, fail)
	}

	got, _ := f.svc.GetChange(f.ctx, ch.ID)
	if got.Status != ChangeEffective {
		t.Fatalf("status = %s want EFFECTIVE", got.Status)
	}
	if bs := f.bills("s1"); len(bs) != 1 {
		t.Fatalf("bills = %d want 1", len(bs))
	}
	var notifCount int
	_ = f.store.View(func(tx *Tx) error {
		notifCount = len(tx.d.notifByChange)
		return nil
	})
	if notifCount != 1 {
		t.Fatalf("notifications = %d want 1", notifCount)
	}
	// 旧版本任务再次执行：不能二次收费。
	if _, err := f.svc.AdvanceChange(f.ctx, ch.ID); !errors.Is(err, ErrNotPending) {
		t.Fatalf("stale re-advance err = %v want ErrNotPending", err)
	}
	if bs := f.bills("s1"); len(bs) != 1 {
		t.Fatalf("bills after stale re-advance = %d want 1", len(bs))
	}
}

// ---- 8. 取消 / 生效 / 终止并发到达：结果确定 ----

func TestTerminateClosesPendingAndBlocksSwitch(t *testing.T) {
	f := newFixture(t, "2026-01-10T00:00:00Z")
	f.createPlan("a", "A", 1000)
	f.createPlan("b", "B", 2000)
	f.createSub("s1", "a")
	ch := f.mustChange("s1", "", "b", EffectiveNextPeriod)

	sub, err := f.svc.TerminateSubscription(f.ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if sub.Status != SubTerminated {
		t.Fatal("subscription not terminated")
	}
	got, _ := f.svc.GetChange(f.ctx, ch.ID)
	if got.Status != ChangeTerminated {
		t.Fatalf("change status = %s want TERMINATED", got.Status)
	}
	// 终止后推进：拒绝，套餐不变。
	f.clock.Set(mustDate(t, "2026-02-01T00:00:00Z"))
	if _, err := f.svc.AdvanceSubscription(f.ctx, "s1"); !errors.Is(err, ErrSubscriptionEnded) {
		t.Fatalf("advance after terminate err = %v want ErrSubscriptionEnded", err)
	}
	s2, _ := f.svc.GetSubscription(f.ctx, "s1")
	if s2.CurrentPlanID != "a" {
		t.Fatal("plan switched after termination")
	}
	if len(f.bills("s1")) != 0 {
		t.Fatal("terminated change must not bill")
	}
	// 终止幂等。
	if _, err := f.svc.TerminateSubscription(f.ctx, "s1"); err != nil {
		t.Fatalf("idempotent terminate: %v", err)
	}
}

func TestEffectiveThenTerminateAndLateCancel(t *testing.T) {
	f := newFixture(t, "2026-01-10T00:00:00Z")
	f.createPlan("a", "A", 1000)
	f.createPlan("b", "B", 2000)
	f.createSub("s1", "a")
	ch := f.mustChange("s1", "", "b", EffectiveImmediate)

	if _, err := f.svc.AdvanceSubscription(f.ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.TerminateSubscription(f.ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	// 迟到取消不能还原已生效切换。
	if _, err := f.svc.CancelChange(f.ctx, "s1"); !errors.Is(err, ErrNotPending) {
		t.Fatalf("late cancel err = %v want ErrNotPending", err)
	}
	s, _ := f.svc.GetSubscription(f.ctx, "s1")
	if s.CurrentPlanID != "b" {
		t.Fatal("effective switch was rolled back by late cancel")
	}
	if bs := f.bills("s1"); len(bs) != 1 || bs[0].Amount <= 0 {
		t.Fatalf("effective bill missing: %+v", bs)
	}
	got, _ := f.svc.GetChange(f.ctx, ch.ID)
	if got.Status != ChangeEffective {
		t.Fatalf("status = %s want EFFECTIVE", got.Status)
	}
}

// TestAdvanceVsTerminateRace 反复并发推进与终止，结果必须二选一且不变量成立。
func TestAdvanceVsTerminateRace(t *testing.T) {
	for iter := 0; iter < 40; iter++ {
		t.Run(fmt.Sprintf("iter%d", iter), func(t *testing.T) {
			f := newFixture(t, "2026-01-10T00:00:00Z")
			f.createPlan("a", "A", 1000)
			f.createPlan("b", "B", 2000)
			f.createSub("s1", "a")
			ch := f.mustChange("s1", "", "b", EffectiveImmediate)

			var wg sync.WaitGroup
			start := make(chan struct{})
			wg.Add(2)
			go func() { defer wg.Done(); <-start; _, _ = f.svc.AdvanceSubscription(f.ctx, "s1") }()
			go func() { defer wg.Done(); <-start; _, _ = f.svc.TerminateSubscription(f.ctx, "s1") }()
			close(start)
			wg.Wait()

			sub, _ := f.svc.GetSubscription(f.ctx, "s1")
			got, _ := f.svc.GetChange(f.ctx, ch.ID)
			bills := f.bills("s1")
			var notifCount int
			_ = f.store.View(func(tx *Tx) error { notifCount = len(tx.d.notifByChange); return nil })

			switch {
			case got.Status == ChangeEffective:
				// 生效胜出（终止可能在其后到达）：恰好一张账单+一条通知，已切到 b。
				if sub.CurrentPlanID != "b" || len(bills) != 1 || notifCount != 1 {
					t.Fatalf("effective branch invariant broken: sub=%s/%s bills=%d notif=%d",
						sub.CurrentPlanID, sub.Status, len(bills), notifCount)
				}
			case got.Status == ChangeTerminated && sub.Status == SubTerminated:
				// 终止胜出：没有账单/通知，套餐仍是 a。
				if sub.CurrentPlanID != "a" || len(bills) != 0 || notifCount != 0 {
					t.Fatalf("terminated branch invariant broken: sub=%s bills=%d notif=%d",
						sub.CurrentPlanID, len(bills), notifCount)
				}
			default:
				t.Fatalf("nondeterministic state: change=%s sub=%s", got.Status, sub.Status)
			}
		})
	}
}

// TestCancelVsAdvanceRace 取消与推进并发：要么取消要么生效，绝不二者皆有。
func TestCancelVsAdvanceRace(t *testing.T) {
	for iter := 0; iter < 40; iter++ {
		t.Run(fmt.Sprintf("iter%d", iter), func(t *testing.T) {
			f := newFixture(t, "2026-01-10T00:00:00Z")
			f.createPlan("a", "A", 1000)
			f.createPlan("b", "B", 2000)
			f.createSub("s1", "a")
			ch := f.mustChange("s1", "", "b", EffectiveImmediate)

			var wg sync.WaitGroup
			start := make(chan struct{})
			wg.Add(2)
			go func() { defer wg.Done(); <-start; _, _ = f.svc.AdvanceSubscription(f.ctx, "s1") }()
			go func() { defer wg.Done(); <-start; _, _ = f.svc.CancelChange(f.ctx, "s1") }()
			close(start)
			wg.Wait()

			got, _ := f.svc.GetChange(f.ctx, ch.ID)
			bills := f.bills("s1")
			sub, _ := f.svc.GetSubscription(f.ctx, "s1")
			switch got.Status {
			case ChangeEffective:
				if len(bills) != 1 || sub.CurrentPlanID != "b" {
					t.Fatal("effective branch invariant broken")
				}
			case ChangeCancelled:
				if len(bills) != 0 || sub.CurrentPlanID != "a" || sub.PendingChangeID != "" {
					t.Fatal("cancel branch invariant broken")
				}
			default:
				t.Fatalf("unexpected status %s", got.Status)
			}
		})
	}
}

// ---- 9. 并发替换不互相静默覆盖 ----

func TestConcurrentReplaceNoSilentOverwrite(t *testing.T) {
	f := newFixture(t, "2026-01-10T00:00:00Z")
	f.createPlan("a", "A", 1000)
	f.createPlan("b", "B", 2000)
	f.createPlan("c", "C", 3000)
	f.createSub("s1", "a")
	first := f.mustChange("s1", "req-0", "b", EffectiveNextPeriod)

	const n = 16
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			// 所有调用方都基于同一版本 first.Version 做替换（乐观锁）。
			in := ChangeInput{
				SubscriptionID:   "s1",
				ClientToken:      fmt.Sprintf("rep-%d", i),
				TargetPlanID:     "c",
				EffectiveType:    EffectiveNextPeriod,
				ExpectedChangeID: first.ID,
			}
			_, errs[i] = f.svc.ReplaceChange(f.ctx, in)
		}(i)
	}
	close(start)
	wg.Wait()

	success := 0
	for _, e := range errs {
		if e == nil {
			success++
		} else if !errors.Is(e, ErrConcurrentUpdate) {
			t.Fatalf("unexpected replace err: %v", e)
		}
	}
	if success != 1 {
		t.Fatalf("successful replaces = %d want 1 (rest must be ErrConcurrentUpdate)", success)
	}
	// 链条完整：首个申请被恰好一个新申请替换。
	firstGot, _ := f.svc.GetChange(f.ctx, first.ID)
	if firstGot.Status != ChangeSuperseded {
		t.Fatalf("first change status = %s want SUPERSEDED", firstGot.Status)
	}
	sub, _ := f.svc.GetSubscription(f.ctx, "s1")
	cur, _ := f.svc.GetChange(f.ctx, sub.PendingChangeID)
	if cur.Status != ChangePending || cur.ToPlan.PlanID != "c" || cur.SupersedesChangeID != first.ID {
		t.Fatalf("unexpected current pending: %+v", cur)
	}
}

// ---- 10. 历史持久化 ----

func TestHistoryPersisted(t *testing.T) {
	f := newFixture(t, "2026-01-10T00:00:00Z")
	f.createPlan("a", "A", 1000)
	f.createPlan("b", "B", 2000)
	f.createSub("s1", "a")
	f.mustChange("s1", "tok1", "b", EffectiveImmediate)
	if _, err := f.svc.AdvanceSubscription(f.ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	events, err := f.svc.ListHistory(f.ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, e := range events {
		types = append(types, e.Type)
	}
	want := []string{EventSubCreated, EventChangeApplied, EventChangeEffective}
	if len(types) != len(want) {
		t.Fatalf("events = %v want %v", types, want)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("event[%d] = %s want %s (full=%v)", i, types[i], want[i], types)
		}
	}
}

// ---- 11. SweepDue 批量推进到期申请 ----

func TestSweepDue(t *testing.T) {
	f := newFixture(t, "2026-01-10T00:00:00Z")
	f.createPlan("a", "A", 1000)
	f.createPlan("b", "B", 2000)
	f.createSub("s1", "a")
	f.createSub("s2", "a")
	f.mustChange("s1", "t1", "b", EffectiveNextPeriod)
	f.mustChange("s2", "t2", "b", EffectiveImmediate) // 立即到期

	res := f.svc.SweepDue(f.ctx)
	if len(res) != 1 || res[0].Change.SubscriptionID != "s2" {
		t.Fatalf("sweep = %d results, want only s2", len(res))
	}
	f.clock.Set(mustDate(t, "2026-02-10T00:00:00Z"))
	res = f.svc.SweepDue(f.ctx)
	if len(res) != 1 || res[0].Change.SubscriptionID != "s1" {
		t.Fatalf("sweep at boundary = %d results, want only s1", len(res))
	}
	if res = f.svc.SweepDue(f.ctx); len(res) != 0 {
		t.Fatalf("repeat sweep = %d want 0", len(res))
	}
}

func TestTimeHelpers(t *testing.T) {
	// 月末开通：Go AddDate 归一化（1/31 -> 2/28）。
	d := dayStart(mustDate(t, "2026-01-31T10:00:00Z"))
	nb := nextMonthlyBoundary(d)
	if !nb.Equal(mustDate(t, "2026-02-28T00:00:00Z")) {
		t.Fatalf("boundary = %s want 2026-02-28", nb)
	}
}

// ---- 12. 终止后的写操作守卫与申请号重试 ----

func TestTerminatedSubscriptionRejectsWrites(t *testing.T) {
	f := newFixture(t, "2026-01-10T00:00:00Z")
	f.createPlan("a", "A", 1000)
	f.createPlan("b", "B", 2000)
	f.createSub("s1", "a")
	ch := f.mustChange("s1", "t1", "b", EffectiveNextPeriod)

	if _, err := f.svc.TerminateSubscription(f.ctx, "s1"); err != nil {
		t.Fatal(err)
	}

	// 同一外部申请号的重试仍稳定返回原申请（幂等优先于终止校验）。
	retry, err := f.svc.ApplyChange(f.ctx, ChangeInput{SubscriptionID: "s1", ClientToken: "t1", TargetPlanID: "b", EffectiveType: EffectiveNextPeriod})
	if err != nil {
		t.Fatalf("token retry after terminate: %v", err)
	}
	if retry.ID != ch.ID || retry.Status != ChangeTerminated {
		t.Fatalf("retry = %+v, want original terminated change", retry)
	}

	// 新申请 / 替换 / 推进均被拒绝。
	if _, err := f.svc.ApplyChange(f.ctx, ChangeInput{SubscriptionID: "s1", ClientToken: "t2", TargetPlanID: "b", EffectiveType: EffectiveImmediate}); !errors.Is(err, ErrSubscriptionEnded) {
		t.Fatalf("apply after terminate err = %v want ErrSubscriptionEnded", err)
	}
	if _, err := f.svc.ReplaceChange(f.ctx, ChangeInput{SubscriptionID: "s1", ClientToken: "t3", TargetPlanID: "b", EffectiveType: EffectiveImmediate}); !errors.Is(err, ErrSubscriptionEnded) {
		t.Fatalf("replace after terminate err = %v want ErrSubscriptionEnded", err)
	}
	if _, err := f.svc.CancelChange(f.ctx, "s1"); !errors.Is(err, ErrNotPending) {
		t.Fatalf("cancel after terminate err = %v want ErrNotPending", err)
	}
}

// ---- 13. 替换的幂等重放与旧申请号归属 ----

func TestReplaceIdempotentReplay(t *testing.T) {
	f := newFixture(t, "2026-01-10T00:00:00Z")
	f.createPlan("a", "A", 1000)
	f.createPlan("b", "B", 2000)
	f.createPlan("c", "C", 3000)
	f.createSub("s1", "a")
	old := f.mustChange("s1", "r1", "b", EffectiveNextPeriod)

	in := ChangeInput{SubscriptionID: "s1", ClientToken: "r2", TargetPlanID: "c", EffectiveType: EffectiveImmediate}
	first, err := f.svc.ReplaceChange(f.ctx, in)
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	// 同一 clientToken 重试替换：返回第一次创建的申请，不再产生新申请。
	replay, err := f.svc.ReplaceChange(f.ctx, in)
	if err != nil {
		t.Fatalf("replace replay: %v", err)
	}
	if replay.ID != first.ID {
		t.Fatalf("replay created %s, want %s", replay.ID, first.ID)
	}
	var changeCount int
	_ = f.store.View(func(tx *Tx) error { changeCount = len(tx.d.changes); return nil })
	if changeCount != 2 {
		t.Fatalf("changes = %d want 2 (old + replacement)", changeCount)
	}

	// 旧申请的外部申请号保留历史归属：换内容复用 -> 冲突。
	_, err = f.svc.ReplaceChange(f.ctx, ChangeInput{SubscriptionID: "s1", ClientToken: "r1", TargetPlanID: "c", EffectiveType: EffectiveImmediate})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("reuse superseded token err = %v want ErrConflict", err)
	}
	// 旧申请号 + 原内容重放：返回旧申请本身（幂等），不报错。
	dup, err := f.svc.ReplaceChange(f.ctx, ChangeInput{SubscriptionID: "s1", ClientToken: "r1", TargetPlanID: "b", EffectiveType: EffectiveNextPeriod})
	if err != nil {
		t.Fatalf("superseded token replay: %v", err)
	}
	if dup.ID != old.ID {
		t.Fatalf("replay = %s want old %s", dup.ID, old.ID)
	}
}

// ---- 14. 按申请号推进（外部执行者路径）----

func TestAdvanceChangeByID(t *testing.T) {
	f := newFixture(t, "2026-01-10T00:00:00Z")
	f.createPlan("a", "A", 1000)
	f.createPlan("b", "B", 2000)
	f.createSub("s1", "a")
	ch := f.mustChange("s1", "tok", "b", EffectiveImmediate)

	res, err := f.svc.AdvanceChange(f.ctx, ch.ID)
	if err != nil {
		t.Fatalf("advance by id: %v", err)
	}
	if res.Change.Status != ChangeEffective || res.Bill.ChangeID != ch.ID || res.Notification.ChangeID != ch.ID {
		t.Fatalf("result = %+v", res)
	}
	sub, _ := f.svc.GetSubscription(f.ctx, "s1")
	if sub.CurrentPlanID != "b" {
		t.Fatalf("plan = %s want b", sub.CurrentPlanID)
	}
	// 不存在的申请号 -> 404 族错误。
	if _, err := f.svc.AdvanceChange(f.ctx, "change-999999"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing change err = %v want ErrNotFound", err)
	}
}

// ---- 15. 月末开通的周期钳制与跨周期推进 ----

func TestMonthEndSubscriptionPeriods(t *testing.T) {
	// 1/31 开通：首周期 1/31 ~ 2/28（钳制到 2 月最后一天）。
	f := newFixture(t, "2026-01-31T10:00:00Z")
	f.createPlan("a", "A", 1000)
	f.createPlan("b", "B", 2000)
	sub := f.createSub("s1", "a")
	if !sub.PeriodStart.Equal(mustDate(t, "2026-01-31T00:00:00Z")) ||
		!sub.PeriodEnd.Equal(mustDate(t, "2026-02-28T00:00:00Z")) {
		t.Fatalf("first period = %s~%s", sub.PeriodStart, sub.PeriodEnd)
	}

	// 下一周期变更冻结 2/28 ~ 3/28。
	ch := f.mustChange("s1", "", "b", EffectiveNextPeriod)
	if !ch.EffectiveAt.Equal(mustDate(t, "2026-02-28T00:00:00Z")) ||
		!ch.FrozenPeriodEnd.Equal(mustDate(t, "2026-03-28T00:00:00Z")) {
		t.Fatalf("frozen next period = %s~%s", ch.FrozenPeriodStart, ch.FrozenPeriodEnd)
	}

	// 边界推进后订阅周期切换到冻结区间。
	f.clock.Set(mustDate(t, "2026-02-28T00:00:00Z"))
	if _, err := f.svc.AdvanceSubscription(f.ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	sub2, _ := f.svc.GetSubscription(f.ctx, "s1")
	if !sub2.PeriodStart.Equal(mustDate(t, "2026-02-28T00:00:00Z")) ||
		!sub2.PeriodEnd.Equal(mustDate(t, "2026-03-28T00:00:00Z")) {
		t.Fatalf("period after advance = %s~%s", sub2.PeriodStart, sub2.PeriodEnd)
	}
}
