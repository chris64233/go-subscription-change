package gosubscriptionchange

import (
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"sync"
	"testing"
)

// 本文件补充“预约变更与生效结算”关键不变量的自动化测试：
//   - 外部申请号在申请终态后重放仍稳定返回原申请；
//   - 替换复用旧申请号的冲突/幂等规则与乐观锁失配；
//   - 生效后允许受理下一笔变更，连续两次生效各有独立账单与通知；
//   - 替换×推进、替换×终止、推进×取消×终止三方并发的终局不变量；
//   - HTTP 层 expectedChangeId 乐观锁冲突。

// ---- 外部申请号：终态后重放 ----

func TestClientTokenReplayAfterDecision(t *testing.T) {
	f := newFixture(t, "2026-01-10T00:00:00Z")
	f.createPlan("a", "A", 1000)
	f.createPlan("b", "B", 2000)
	f.createSub("s1", "a")
	f.createSub("s2", "a")

	// 生效后重放：返回同一笔（EFFECTIVE），不重复收费。
	first := f.mustChange("s1", "tok-eff", "b", EffectiveImmediate)
	if _, err := f.svc.AdvanceSubscription(f.ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	replay, err := f.svc.ApplyChange(f.ctx, ChangeInput{
		SubscriptionID: "s1", ClientToken: "tok-eff",
		TargetPlanID: "b", EffectiveType: EffectiveImmediate,
	})
	if err != nil {
		t.Fatalf("replay after effective: %v", err)
	}
	if replay.ID != first.ID || replay.Status != ChangeEffective {
		t.Fatalf("replay = %s/%s want %s/EFFECTIVE", replay.ID, replay.Status, first.ID)
	}
	if bs := f.bills("s1"); len(bs) != 1 {
		t.Fatalf("replay after effective produced %d bills", len(bs))
	}

	// 取消后重放：返回同一笔（CANCELLED），不产生账单。
	cancelled := f.mustChange("s2", "tok-cxl", "b", EffectiveNextPeriod)
	if _, err := f.svc.CancelChange(f.ctx, "s2"); err != nil {
		t.Fatal(err)
	}
	replay2, err := f.svc.ApplyChange(f.ctx, ChangeInput{
		SubscriptionID: "s2", ClientToken: "tok-cxl",
		TargetPlanID: "b", EffectiveType: EffectiveNextPeriod,
	})
	if err != nil {
		t.Fatalf("replay after cancel: %v", err)
	}
	if replay2.ID != cancelled.ID || replay2.Status != ChangeCancelled {
		t.Fatalf("replay = %s/%s want %s/CANCELLED", replay2.ID, replay2.Status, cancelled.ID)
	}
	if bs := f.bills("s2"); len(bs) != 0 {
		t.Fatalf("replay after cancel produced %d bills", len(bs))
	}
}

// ---- 替换时的外部申请号规则与乐观锁 ----

func TestReplaceTokenRulesAndOptimisticLock(t *testing.T) {
	f := newFixture(t, "2026-01-10T00:00:00Z")
	f.createPlan("a", "A", 1000)
	f.createPlan("b", "B", 2000)
	f.createPlan("c", "C", 3000)
	f.createSub("s1", "a")

	pending := f.mustChange("s1", "req-1", "b", EffectiveNextPeriod)

	// 同号但目标/方式不同：冲突。
	_, err := f.svc.ReplaceChange(f.ctx, ChangeInput{
		SubscriptionID: "s1", ClientToken: "req-1",
		TargetPlanID: "c", EffectiveType: EffectiveNextPeriod,
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("replace reused token w/ different target err = %v want ErrConflict", err)
	}

	// 同号同内容：幂等返回原申请，不发生替换。
	same, err := f.svc.ReplaceChange(f.ctx, ChangeInput{
		SubscriptionID: "s1", ClientToken: "req-1",
		TargetPlanID: "b", EffectiveType: EffectiveNextPeriod,
	})
	if err != nil {
		t.Fatalf("identical replace replay: %v", err)
	}
	if same.ID != pending.ID || same.Status != ChangePending || same.SupersedesChangeID != "" {
		t.Fatalf("identical replay should be no-op: %+v", same)
	}

	// 错误的 expectedChangeId：乐观锁失配，不能静默覆盖。
	_, err = f.svc.ReplaceChange(f.ctx, ChangeInput{
		SubscriptionID: "s1", ClientToken: "req-2",
		TargetPlanID: "c", EffectiveType: EffectiveImmediate,
		ExpectedChangeID: "change-000999",
	})
	if !errors.Is(err, ErrConcurrentUpdate) {
		t.Fatalf("stale expectedChangeId err = %v want ErrConcurrentUpdate", err)
	}
	got, _ := f.svc.GetChange(f.ctx, pending.ID)
	if got.Status != ChangePending {
		t.Fatalf("failed replace must not modify pending: %s", got.Status)
	}

	// 正确的 expectedChangeId：替换成功。
	next, err := f.svc.ReplaceChange(f.ctx, ChangeInput{
		SubscriptionID: "s1", ClientToken: "req-3",
		TargetPlanID: "c", EffectiveType: EffectiveImmediate,
		ExpectedChangeID: pending.ID,
	})
	if err != nil {
		t.Fatalf("replace with matching expected id: %v", err)
	}
	if next.SupersedesChangeID != pending.ID {
		t.Fatalf("supersedes = %q want %q", next.SupersedesChangeID, pending.ID)
	}
	old, _ := f.svc.GetChange(f.ctx, pending.ID)
	if old.Status != ChangeSuperseded {
		t.Fatalf("old status = %s want SUPERSEDED", old.Status)
	}
}

// ---- 终止后拒绝新的申请/替换 ----

func TestApplyAndReplaceRejectedAfterTerminate(t *testing.T) {
	f := newFixture(t, "2026-01-10T00:00:00Z")
	f.createPlan("a", "A", 1000)
	f.createPlan("b", "B", 2000)
	f.createSub("s1", "a")
	f.mustChange("s1", "t1", "b", EffectiveNextPeriod)
	if _, err := f.svc.TerminateSubscription(f.ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.ApplyChange(f.ctx, ChangeInput{
		SubscriptionID: "s1", ClientToken: "t2",
		TargetPlanID: "b", EffectiveType: EffectiveImmediate,
	}); !errors.Is(err, ErrSubscriptionEnded) {
		t.Fatalf("apply after terminate err = %v want ErrSubscriptionEnded", err)
	}
	if _, err := f.svc.ReplaceChange(f.ctx, ChangeInput{
		SubscriptionID: "s1", ClientToken: "t3",
		TargetPlanID: "b", EffectiveType: EffectiveImmediate,
	}); !errors.Is(err, ErrSubscriptionEnded) {
		t.Fatalf("replace after terminate err = %v want ErrSubscriptionEnded", err)
	}
}

// ---- 生效后可连续受理并生效下一笔变更，账单/通知各自独立 ----

func TestSequentialChangesAfterEffective(t *testing.T) {
	f := newFixture(t, "2026-01-10T00:00:00Z")
	f.createPlan("a", "A", 1000)
	f.createPlan("b", "B", 2000)
	f.createPlan("c", "C", 5000)
	f.createSub("s1", "a")

	first := f.mustChange("s1", "t1", "b", EffectiveImmediate)
	r1, err := f.svc.AdvanceSubscription(f.ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	// 第一笔生效后没有待生效申请，允许再申请一笔。
	second, err := f.svc.ApplyChange(f.ctx, ChangeInput{
		SubscriptionID: "s1", ClientToken: "t2",
		TargetPlanID: "c", EffectiveType: EffectiveImmediate,
	})
	if err != nil {
		t.Fatalf("apply after effective: %v", err)
	}
	if second.FromPlan.PlanID != "b" {
		t.Fatalf("second change from-plan = %s want b", second.FromPlan.PlanID)
	}
	r2, err := f.svc.AdvanceSubscription(f.ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}

	if r1.Bill.ID == r2.Bill.ID || r1.Notification.ID == r2.Notification.ID {
		t.Fatal("bill/notification ids must be unique across changes")
	}
	if bs := f.bills("s1"); len(bs) != 2 || bs[0].ChangeID != first.ID || bs[1].ChangeID != second.ID {
		t.Fatalf("bills = %+v", bs)
	}
	var notifCount int
	_ = f.store.View(func(tx *Tx) error { notifCount = len(tx.d.notifByChange); return nil })
	if notifCount != 2 {
		t.Fatalf("notifications = %d want 2", notifCount)
	}
	sub, _ := f.svc.GetSubscription(f.ctx, "s1")
	if sub.CurrentPlanID != "c" {
		t.Fatalf("plan = %s want c", sub.CurrentPlanID)
	}
	events, _ := f.svc.ListHistory(f.ctx, "s1")
	want := []string{
		EventSubCreated, EventChangeApplied, EventChangeEffective,
		EventChangeApplied, EventChangeEffective,
	}
	if len(events) != len(want) {
		t.Fatalf("history len = %d want %d (%v)", len(events), len(want), events)
	}
	for i, ty := range want {
		if events[i].Type != ty {
			t.Fatalf("history[%d] = %s want %s", i, events[i].Type, ty)
		}
	}
}

// ---- 下一周期生效：账单/通知与申请的关联全部落定 ----

func TestNextPeriodEffectiveArtifacts(t *testing.T) {
	f := newFixture(t, "2026-01-10T00:00:00Z")
	f.createPlan("a", "A", 1000)
	f.createPlan("b", "B", 3000)
	f.createSub("s1", "a")
	ch := f.mustChange("s1", "t1", "b", EffectiveNextPeriod)

	f.clock.Set(mustDate(t, "2026-02-10T00:00:00Z"))
	res, err := f.svc.AdvanceSubscription(f.ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if res.Change.ID != ch.ID || res.Change.Status != ChangeEffective {
		t.Fatalf("result change = %+v", res.Change)
	}
	if res.Change.ResultBillID != res.Bill.ID || res.Change.ResultNotificationID != res.Notification.ID {
		t.Fatalf("result linkage broken: %+v", res.Change)
	}
	if res.Bill.ChangeID != ch.ID || res.Bill.Amount != 3000 {
		t.Fatalf("bill = %+v", res.Bill)
	}
	if res.Notification.ChangeID != ch.ID || res.Notification.Type != NotificationChangeEffective {
		t.Fatalf("notification = %+v", res.Notification)
	}
}

// ---- 并发：替换 × 推进 ----
//
// 两种序列化结局，且不变量恒成立：
//   - 推进先提交：旧申请 EFFECTIVE、切到 b、一张归属于旧申请的账单；
//   - 替换先提交：旧申请 SUPERSEDED，推进随即落到新申请并生效——切到 c、
//     一张归属于新申请的账单；订阅上不残留 PENDING。
func TestReplaceVsAdvanceRace(t *testing.T) {
	for iter := 0; iter < 40; iter++ {
		t.Run(fmt.Sprintf("iter%d", iter), func(t *testing.T) {
			f := newFixture(t, "2026-01-10T00:00:00Z")
			f.createPlan("a", "A", 1000)
			f.createPlan("b", "B", 2000)
			f.createPlan("c", "C", 3000)
			f.createSub("s1", "a")
			old := f.mustChange("s1", "t-old", "b", EffectiveImmediate)

			var wg sync.WaitGroup
			start := make(chan struct{})
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				_, _ = f.svc.AdvanceSubscription(f.ctx, "s1")
			}()
			go func() {
				defer wg.Done()
				<-start
				_, _ = f.svc.ReplaceChange(f.ctx, ChangeInput{
					SubscriptionID: "s1", ClientToken: "t-new",
					TargetPlanID: "c", EffectiveType: EffectiveImmediate,
				})
			}()
			close(start)
			wg.Wait()

			sub, _ := f.svc.GetSubscription(f.ctx, "s1")
			bills := f.bills("s1")
			var notifCount int
			_ = f.store.View(func(tx *Tx) error { notifCount = len(tx.d.notifByChange); return nil })
			oldGot, _ := f.svc.GetChange(f.ctx, old.ID)

			if len(bills) != 1 || notifCount != 1 {
				t.Fatalf("bills=%d notif=%d, want exactly one settlement", len(bills), notifCount)
			}
			if sub.PendingChangeID != "" {
				t.Fatalf("pending change left dangling: %s", sub.PendingChangeID)
			}
			switch oldGot.Status {
			case ChangeEffective:
				if sub.CurrentPlanID != "b" || bills[0].ChangeID != old.ID {
					t.Fatalf("advance-first branch broken: plan=%s bill=%+v", sub.CurrentPlanID, bills[0])
				}
			case ChangeSuperseded:
				eff, _ := f.svc.GetChange(f.ctx, bills[0].ChangeID)
				if eff == nil || eff.Status != ChangeEffective || eff.ToPlan.PlanID != "c" ||
					eff.SupersedesChangeID != old.ID {
					t.Fatalf("replace-first branch broken: effective=%+v", eff)
				}
				if sub.CurrentPlanID != "c" {
					t.Fatalf("replace-first plan = %s want c", sub.CurrentPlanID)
				}
			default:
				t.Fatalf("old change nondeterministic status: %s", oldGot.Status)
			}
		})
	}
}

// ---- 并发：替换 × 终止 ----
//
// 终止先提交：旧申请 TERMINATED，替换被拒绝；替换先提交：旧申请 SUPERSEDED、
// 新申请被随订阅关闭为 TERMINATED。任何结局都不账单、不通知、不残留 PENDING。
func TestReplaceVsTerminateRace(t *testing.T) {
	for iter := 0; iter < 40; iter++ {
		t.Run(fmt.Sprintf("iter%d", iter), func(t *testing.T) {
			f := newFixture(t, "2026-01-10T00:00:00Z")
			f.createPlan("a", "A", 1000)
			f.createPlan("b", "B", 2000)
			f.createPlan("c", "C", 3000)
			f.createSub("s1", "a")
			old := f.mustChange("s1", "t-old", "b", EffectiveNextPeriod)

			var wg sync.WaitGroup
			start := make(chan struct{})
			var replacement *ChangeApplication
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				ch, _ := f.svc.ReplaceChange(f.ctx, ChangeInput{
					SubscriptionID: "s1", ClientToken: "t-new",
					TargetPlanID: "c", EffectiveType: EffectiveNextPeriod,
				})
				if ch != nil {
					replacement = ch
				}
			}()
			go func() {
				defer wg.Done()
				<-start
				_, _ = f.svc.TerminateSubscription(f.ctx, "s1")
			}()
			close(start)
			wg.Wait()

			sub, _ := f.svc.GetSubscription(f.ctx, "s1")
			if sub.Status != SubTerminated || sub.PendingChangeID != "" {
				t.Fatalf("sub = %s pending=%q, want TERMINATED with no pending",
					sub.Status, sub.PendingChangeID)
			}
			if bs := f.bills("s1"); len(bs) != 0 {
				t.Fatalf("terminate races must never bill, got %d", len(bs))
			}
			var notifCount int
			_ = f.store.View(func(tx *Tx) error { notifCount = len(tx.d.notifByChange); return nil })
			if notifCount != 0 {
				t.Fatalf("terminate races must never notify, got %d", notifCount)
			}
			oldGot, _ := f.svc.GetChange(f.ctx, old.ID)
			switch oldGot.Status {
			case ChangeTerminated:
				// 终止先提交：替换必然失败，不存在新申请。
				if replacement != nil {
					t.Fatalf("terminate-first must reject replace, got %+v", replacement)
				}
			case ChangeSuperseded:
				// 替换先提交：新申请必须存在且随订阅 TERMINATED。
				if replacement == nil {
					t.Fatal("replace-first must return the new change")
				}
				newGot, _ := f.svc.GetChange(f.ctx, replacement.ID)
				if newGot == nil || newGot.Status != ChangeTerminated {
					t.Fatalf("new change = %+v want TERMINATED with subscription", newGot)
				}
			default:
				t.Fatalf("old change nondeterministic status: %s", oldGot.Status)
			}
		})
	}
}

// ---- 并发：推进 × 取消 × 终止（三方同时到达） ----
//
// 事务被仓储互斥锁完全串行化，终局只能是：
//   - 变更 EFFECTIVE：恰好一张账单/一条通知、套餐已切换（终止随后幂等到达）；
//   - 变更 CANCELLED：无账单，订阅最终 TERMINATED；
//   - 变更 TERMINATED：无账单，订阅 TERMINATED。
//
// 已生效的切换绝不会被任何迟到的取消/终止还原。
func TestAdvanceCancelTerminateThreeWayRace(t *testing.T) {
	statusCounts := map[string]int{}
	for iter := 0; iter < 60; iter++ {
		t.Run(fmt.Sprintf("iter%d", iter), func(t *testing.T) {
			f := newFixture(t, "2026-01-10T00:00:00Z")
			f.createPlan("a", "A", 1000)
			f.createPlan("b", "B", 2000)
			f.createSub("s1", "a")
			ch := f.mustChange("s1", "t1", "b", EffectiveImmediate)

			var wg sync.WaitGroup
			start := make(chan struct{})
			run := func(fn func()) {
				defer wg.Done()
				<-start
				fn()
			}
			ops := []func(){
				func() { _, _ = f.svc.AdvanceSubscription(f.ctx, "s1") },
				func() { _, _ = f.svc.CancelChange(f.ctx, "s1") },
				func() { _, _ = f.svc.TerminateSubscription(f.ctx, "s1") },
			}
			// 每轮打乱启动顺序，避免固定调度顺序使某个分支长期饥饿。
			r := rand.New(rand.NewSource(int64(iter)))
			r.Shuffle(len(ops), func(i, j int) { ops[i], ops[j] = ops[j], ops[i] })
			wg.Add(len(ops))
			for _, op := range ops {
				go run(op)
			}
			close(start)
			wg.Wait()

			got, _ := f.svc.GetChange(f.ctx, ch.ID)
			sub, _ := f.svc.GetSubscription(f.ctx, "s1")
			bills := f.bills("s1")
			var notifCount int
			_ = f.store.View(func(tx *Tx) error { notifCount = len(tx.d.notifByChange); return nil })

			if sub.Status != SubTerminated || sub.PendingChangeID != "" {
				t.Fatalf("sub = %s pending=%q, after all three arrive want TERMINATED/no pending",
					sub.Status, sub.PendingChangeID)
			}
			statusCounts[got.Status]++
			switch got.Status {
			case ChangeEffective:
				if sub.CurrentPlanID != "b" || len(bills) != 1 || notifCount != 1 {
					t.Fatalf("effective branch: plan=%s bills=%d notif=%d",
						sub.CurrentPlanID, len(bills), notifCount)
				}
			case ChangeCancelled, ChangeTerminated:
				if sub.CurrentPlanID != "a" || len(bills) != 0 || notifCount != 0 {
					t.Fatalf("%s branch: plan=%s bills=%d notif=%d",
						got.Status, sub.CurrentPlanID, len(bills), notifCount)
				}
			default:
				t.Fatalf("nondeterministic change status: %s", got.Status)
			}
		})
	}
	t.Logf("three-way outcome distribution over 60 iterations: %v", statusCounts)
}

// ---- HTTP 层：expectedChangeId 乐观锁 ----

func TestAPIReplaceOptimisticLock(t *testing.T) {
	h, _, _ := newTestServer(t, "2026-01-10T00:00:00Z")
	mustOK := func(st int, where string) {
		t.Helper()
		if st != http.StatusCreated {
			t.Fatalf("%s status=%d want 201", where, st)
		}
	}
	st, _ := doJSON(t, h, "POST", "/plans", map[string]any{"id": "a", "name": "A", "currency": "CNY", "price": 1000})
	mustOK(st, "plan a")
	st, _ = doJSON(t, h, "POST", "/plans", map[string]any{"id": "b", "name": "B", "currency": "CNY", "price": 2000})
	mustOK(st, "plan b")
	st, _ = doJSON(t, h, "POST", "/plans", map[string]any{"id": "c", "name": "C", "currency": "CNY", "price": 3000})
	mustOK(st, "plan c")
	st, _ = doJSON(t, h, "POST", "/subscriptions", map[string]any{"id": "s1", "planId": "a"})
	mustOK(st, "sub s1")
	st, chAny := doJSON(t, h, "POST", "/subscriptions/s1/changes",
		map[string]any{"clientToken": "r1", "targetPlanId": "b", "effectiveType": "NEXT_PERIOD"})
	if st != http.StatusCreated {
		t.Fatalf("apply st=%d", st)
	}
	currentID := asMap(t, chAny)["id"].(string)

	// 携带错误的 expectedChangeId：409 CONCURRENT_UPDATE，不发生替换。
	st, body := doJSON(t, h, "PUT", "/subscriptions/s1/changes", map[string]any{
		"clientToken":      "r2",
		"targetPlanId":     "c",
		"effectiveType":    "IMMEDIATE",
		"expectedChangeId": "change-000999",
	})
	if st != http.StatusConflict {
		t.Fatalf("stale optimistic lock st=%d body=%v want 409", st, body)
	}
	if code := asMap(t, body)["error"].(map[string]any)["code"]; code != "CONCURRENT_UPDATE" {
		t.Fatalf("error code = %v want CONCURRENT_UPDATE", code)
	}
	st, still := doJSON(t, h, "GET", "/changes/"+currentID, nil)
	if st != http.StatusOK || asMap(t, still)["status"] != ChangePending {
		t.Fatalf("pending change must survive failed replace: %v", still)
	}

	// 携带正确的 expectedChangeId：200 替换成功。
	st, replaced := doJSON(t, h, "PUT", "/subscriptions/s1/changes", map[string]any{
		"clientToken":      "r3",
		"targetPlanId":     "c",
		"effectiveType":    "IMMEDIATE",
		"expectedChangeId": currentID,
	})
	if st != http.StatusOK {
		t.Fatalf("matched optimistic lock st=%d body=%v", st, replaced)
	}
	if asMap(t, replaced)["supersedesChangeId"] != currentID {
		t.Fatalf("supersedes = %v want %s", asMap(t, replaced)["supersedesChangeId"], currentID)
	}
}
