package subscription

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// testEnv 提供内存存储与可拨动的时钟。
type testEnv struct {
	t     *testing.T
	store *Store
	svc   *Service
	now   time.Time
	mu    sync.Mutex
}

func newTestEnv(t *testing.T, start time.Time) *testEnv {
	t.Helper()
	store, err := Open("")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	env := &testEnv{t: t, store: store, now: start}
	env.svc = NewService(store, env.clock)
	t.Cleanup(func() { _ = store.Close() })
	return env
}

func (e *testEnv) clock() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.now
}

func (e *testEnv) advance(d time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.now = e.now.Add(d)
}

func (e *testEnv) setNow(tm time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.now = tm
}

// mustPlan 配置套餐。
func (e *testEnv) mustPlan(id, name, cycle string, price int64) *Plan {
	e.t.Helper()
	p, err := e.svc.ConfigurePlan(PlanConfigInput{ID: id, Name: name, Cycle: cycle, PriceCents: price})
	if err != nil {
		e.t.Fatalf("ConfigurePlan(%s): %v", id, err)
	}
	return p
}

func (e *testEnv) mustSub(planID string) *Subscription {
	e.t.Helper()
	sub, _, err := e.svc.CreateSubscription(CreateSubscriptionInput{PlanID: planID})
	if err != nil {
		e.t.Fatalf("CreateSubscription: %v", err)
	}
	return sub
}

func wantErrIs(t *testing.T, err, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("err = %v, want errors.Is %v", err, target)
	}
}

// ---------------- 场景 1：申请冻结 + 精确差额 + 价格变化不改写 ----------------

func TestImmediateChangeProrationAndFreeze(t *testing.T) {
	// 30 天周期（4月），起始时刻 2026-04-01。
	start := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	env := newTestEnv(t, start)
	basic := env.mustPlan("basic", "基础版", CycleMonthly, 3000)
	pro := env.mustPlan("pro", "专业版", CycleMonthly, 6000)
	sub := env.mustSub(basic.ID)
	_ = pro

	// 第 10 天结束时（已用 10 天、剩余 20 天）立即升级。
	env.setNow(time.Date(2026, 4, 11, 0, 0, 0, 0, time.UTC))
	ch, adv, err := env.svc.ApplyChange(ApplyChangeInput{
		SubscriptionID: sub.ID, ToPlanID: "pro", EffectiveMode: EffectiveImmediate,
	})
	if err != nil {
		t.Fatalf("ApplyChange: %v", err)
	}
	// 差额 = (6000-3000)*20/30 = 2000 整
	if ch.DiffCents != 2000 || ch.DiffDirection != BillCharge {
		t.Fatalf("diff = %d/%s, want 2000/charge", ch.DiffCents, ch.DiffDirection)
	}
	// 冻结校验
	if ch.FromPlan.PriceCents != 3000 || ch.ToPlan.PriceCents != 6000 {
		t.Fatalf("snapshot prices = %d→%d, want 3000→6000", ch.FromPlan.PriceCents, ch.ToPlan.PriceCents)
	}
	if !ch.Period.Start.Equal(start) {
		t.Fatalf("frozen period start = %v, want %v", ch.Period.Start, start)
	}
	if ch.Status != ChangeEffective || adv == nil || !adv.Advanced {
		t.Fatalf("immediate change should settle atomically, status=%s adv=%+v", ch.Status, adv)
	}
	if adv.Bill == nil || adv.Bill.Kind != BillKindProration || adv.Bill.AmountCents != 2000 || adv.Bill.Direction != BillCharge {
		t.Fatalf("bill = %+v, want proration charge 2000", adv.Bill)
	}
	if adv.Notification == nil || adv.Notification.ChangeID != ch.ID {
		t.Fatalf("notification missing: %+v", adv.Notification)
	}
	// 订阅已切换到 pro，周期窗口不变。
	got, _ := env.svc.GetSubscription(sub.ID)
	if got.Plan.PlanID != "pro" {
		t.Fatalf("current plan = %s, want pro", got.Plan.PlanID)
	}
	if !got.PeriodEnd.Equal(ch.Period.End) {
		t.Fatalf("period window must not change on immediate switch")
	}
}

func TestPriceChangesDoNotRewriteAcceptedChange(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	env := newTestEnv(t, start)
	env.mustPlan("basic", "基础版", CycleMonthly, 3000)
	env.mustPlan("pro", "专业版", CycleMonthly, 6000)
	sub := env.mustSub("basic")

	// 第 5 天预约下一周期边界升级，冻结 pro v1=6000。
	env.setNow(time.Date(2026, 1, 6, 0, 0, 0, 0, time.UTC))
	ch, _, err := env.svc.ApplyChange(ApplyChangeInput{
		SubscriptionID: sub.ID, ToPlanID: "pro", EffectiveMode: EffectiveNextCycle,
	})
	if err != nil {
		t.Fatalf("ApplyChange: %v", err)
	}
	if ch.ToPlan.PriceCents != 6000 {
		t.Fatalf("frozen pro price = %d, want 6000", ch.ToPlan.PriceCents)
	}

	// 受理后 pro 涨价到 9000；basic 也调价。
	if _, err := env.svc.AddPlanPrice("pro", 9000, time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("AddPlanPrice: %v", err)
	}
	if _, err := env.svc.AddPlanPrice("basic", 3300, time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("AddPlanPrice: %v", err)
	}

	// 推进到周期边界（2026-02-01）后 advance，续费账单必须按冻结价 6000。
	env.setNow(time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC))
	adv, err := env.svc.AdvanceChange(ch.ID)
	if err != nil {
		t.Fatalf("AdvanceChange: %v", err)
	}
	if adv.Bill == nil || adv.Bill.AmountCents != 6000 || adv.Bill.Kind != BillKindRenewal {
		t.Fatalf("renewal bill = %+v, want renewal 6000 (frozen)", adv.Bill)
	}
	got, _ := env.svc.GetSubscription(sub.ID)
	if got.Plan.Version != 1 || got.Plan.PriceCents != 6000 {
		t.Fatalf("switched plan snapshot = v%d/%d, want frozen v1/6000", got.Plan.Version, got.Plan.PriceCents)
	}
	// 新周期窗口按新套餐（同为 monthly）推进。
	if !got.PeriodStart.Equal(ch.Period.End) {
		t.Fatalf("new period start = %v, want %v", got.PeriodStart, ch.Period.End)
	}
}

// ---------------- 场景 2：单待生效、版本乐观并发、外部号冲突 ----------------

func TestSinglePendingConstraint(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	env := newTestEnv(t, start)
	env.mustPlan("basic", "基础版", CycleMonthly, 3000)
	env.mustPlan("pro", "专业版", CycleMonthly, 6000)
	env.mustPlan("ultra", "旗舰版", CycleMonthly, 9000)
	sub := env.mustSub("basic")
	env.advance(24 * time.Hour)

	ch, _, err := env.svc.ApplyChange(ApplyChangeInput{SubscriptionID: sub.ID, ToPlanID: "pro", EffectiveMode: EffectiveNextCycle})
	if err != nil {
		t.Fatalf("ApplyChange: %v", err)
	}
	// 第二笔 next_cycle 必须被拒。
	_, _, err = env.svc.ApplyChange(ApplyChangeInput{SubscriptionID: sub.ID, ToPlanID: "ultra", EffectiveMode: EffectiveNextCycle})
	wantErrIs(t, err, ErrPendingChangeExists)
	if ch.Version != 1 {
		t.Fatalf("first change version = %d, want 1", ch.Version)
	}
}

func TestReplaceVersionCASAndOldVersionCannotCharge(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	env := newTestEnv(t, start)
	env.mustPlan("basic", "基础版", CycleMonthly, 3000)
	env.mustPlan("pro", "专业版", CycleMonthly, 6000)
	env.mustPlan("ultra", "旗舰版", CycleMonthly, 9000)
	sub := env.mustSub("basic")
	env.advance(24 * time.Hour)

	v1, _, err := env.svc.ApplyChange(ApplyChangeInput{SubscriptionID: sub.ID, ToPlanID: "pro", EffectiveMode: EffectiveNextCycle})
	if err != nil {
		t.Fatalf("ApplyChange v1: %v", err)
	}
	// 错误的期望版本：拒绝，避免静默覆盖。
	wrong := 9
	_, _, err = env.svc.ReplaceChange(ReplaceChangeInput{
		SubscriptionID: sub.ID, ChangeID: v1.ID, ToPlanID: "ultra", EffectiveMode: EffectiveNextCycle, ExpectedVersion: &wrong,
	})
	wantErrIs(t, err, ErrVersionMismatch)

	// 拿旧版本 ID 再替换（并发场景：另一方已先替换为 v2），旧 ID 必须失败。
	v2, _, err := env.svc.ReplaceChange(ReplaceChangeInput{
		SubscriptionID: sub.ID, ChangeID: v1.ID, ToPlanID: "ultra", EffectiveMode: EffectiveNextCycle, ExpectedVersion: &v1.Version,
	})
	if err != nil {
		t.Fatalf("ReplaceChange v2: %v", err)
	}
	if v2.Version != 2 {
		t.Fatalf("replaced version = %d, want 2", v2.Version)
	}
	_, _, err = env.svc.ReplaceChange(ReplaceChangeInput{
		SubscriptionID: sub.ID, ChangeID: v1.ID, ToPlanID: "pro", EffectiveMode: EffectiveNextCycle,
	})
	wantErrIs(t, err, ErrVersionMismatch)

	// 旧版本到期推进：superseded，不能再次收费。
	env.setNow(time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC))
	if _, err := env.svc.AdvanceChange(v1.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("advance superseded v1 err = %v, want ErrConflict", err)
	}
	// 当前待生效 v2 正常推进，出 ultra 冻结价 9000。
	adv, err := env.svc.AdvanceChange(v2.ID)
	if err != nil {
		t.Fatalf("advance v2: %v", err)
	}
	if adv.Bill.AmountCents != 9000 {
		t.Fatalf("v2 bill = %d, want 9000", adv.Bill.AmountCents)
	}
	bills, _ := env.svc.ListBills(sub.ID)
	// initial + renewal 共两张，旧版本没有任何账单。
	if len(bills) != 2 {
		t.Fatalf("bills = %d, want 2 (no old-version charge)", len(bills))
	}
}

func TestExternalIDDedupAndConflict(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	env := newTestEnv(t, start)
	env.mustPlan("basic", "基础版", CycleMonthly, 3000)
	env.mustPlan("pro", "专业版", CycleMonthly, 6000)
	env.mustPlan("ultra", "旗舰版", CycleMonthly, 9000)
	sub := env.mustSub("basic")
	env.advance(24 * time.Hour)

	in := func(to, mode string) ApplyChangeInput {
		return ApplyChangeInput{SubscriptionID: sub.ID, ExternalID: "REQ-1", ToPlanID: to, EffectiveMode: mode}
	}
	ch1, _, err := env.svc.ApplyChange(in("pro", EffectiveNextCycle))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	// 同号同参重放：返回同一申请。
	ch2, _, err := env.svc.ApplyChange(in("pro", EffectiveNextCycle))
	if err != nil {
		t.Fatalf("idempotent replay: %v", err)
	}
	if ch1.ID != ch2.ID {
		t.Fatalf("replay produced new change %s != %s", ch2.ID, ch1.ID)
	}
	// 同号换目标套餐：冲突。
	_, _, err = env.svc.ApplyChange(in("ultra", EffectiveNextCycle))
	wantErrIs(t, err, ErrConflict)
	// 同号换生效方式：冲突。
	_, _, err = env.svc.ApplyChange(in("pro", EffectiveImmediate))
	wantErrIs(t, err, ErrConflict)
}

func TestConcurrentReplaceOnlyOneWins(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	env := newTestEnv(t, start)
	env.mustPlan("basic", "基础版", CycleMonthly, 3000)
	env.mustPlan("pro", "专业版", CycleMonthly, 6000)
	env.mustPlan("ultra", "旗舰版", CycleMonthly, 9000)
	sub := env.mustSub("basic")
	env.advance(24 * time.Hour)
	ch, _, err := env.svc.ApplyChange(ApplyChangeInput{SubscriptionID: sub.ID, ToPlanID: "pro", EffectiveMode: EffectiveNextCycle})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}

	// 两个执行者都基于 v1 做替换：只能一个成功。
	var wg sync.WaitGroup
	var ok, fail int32
	var mu sync.Mutex
	doReplace := func(to string) {
		defer wg.Done()
		v := ch.Version
		_, _, err := env.svc.ReplaceChange(ReplaceChangeInput{
			SubscriptionID: sub.ID, ChangeID: ch.ID, ToPlanID: to, EffectiveMode: EffectiveNextCycle, ExpectedVersion: &v,
		})
		mu.Lock()
		defer mu.Unlock()
		if err == nil {
			ok++
		} else if errors.Is(err, ErrVersionMismatch) {
			fail++
		} else {
			t.Errorf("unexpected err: %v", err)
		}
	}
	wg.Add(2)
	go doReplace("ultra")
	go doReplace("pro")
	wg.Wait()
	if ok != 1 || fail != 1 {
		t.Fatalf("replace race: ok=%d fail=%d, want 1/1", ok, fail)
	}
	history, _ := env.svc.ListChanges(sub.ID)
	if len(history) != 2 {
		t.Fatalf("history len = %d, want 2", len(history))
	}
}

// ---------------- 场景 3：原子推进、只生效一次、唯一通知 ----------------

func TestAdvanceAtomicIdempotentSingleNotification(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	env := newTestEnv(t, start)
	env.mustPlan("basic", "基础版", CycleMonthly, 3000)
	env.mustPlan("pro", "专业版", CycleMonthly, 6000)
	sub := env.mustSub("basic")
	env.setNow(time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC))
	ch, _, err := env.svc.ApplyChange(ApplyChangeInput{SubscriptionID: sub.ID, ToPlanID: "pro", EffectiveMode: EffectiveNextCycle})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}

	// 未到期不能推进。
	if _, err := env.svc.AdvanceChange(ch.ID); !errors.Is(err, ErrChangeNotDue) {
		t.Fatalf("early advance err = %v, want ErrChangeNotDue", err)
	}

	env.setNow(time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC))

	// 并发推进同一申请：只生效一次、只有一张账单、一条通知。
	var wg sync.WaitGroup
	var success, replay int32
	var mu sync.Mutex
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := env.svc.AdvanceChange(ch.ID)
			if err != nil {
				t.Errorf("advance: %v", err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if r.Advanced {
				success++
			} else {
				replay++
			}
		}()
	}
	wg.Wait()
	if success != 1 || replay != 7 {
		t.Fatalf("advance race: success=%d replay=%d, want 1/7", success, replay)
	}
	bills := env.tenantBillsForTest(t, sub.ID)
	if len(bills) != 2 { // initial + renewal
		t.Fatalf("bills = %d, want 2", len(bills))
	}
	// 通知去重：通过存储 dedup 索引校验只有一条。
	env.store.mu.Lock()
	n := len(env.store.st.notifDedup)
	env.store.mu.Unlock()
	if n != 1 {
		t.Fatalf("notifications = %d, want exactly 1", n)
	}

	// 重放返回的账单/通知与首次一致。
	r2, err := env.svc.AdvanceChange(ch.ID)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if r2.Advanced || r2.Bill == nil || r2.Notification == nil {
		t.Fatalf("replay result = %+v", r2)
	}
}

func TestAdvanceWritesAllOrNothing(t *testing.T) {
	// 事件组原子性：直接检查推进产生的事件数量——切换、账单、通知同事务。
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	env := newTestEnv(t, start)
	env.mustPlan("basic", "基础版", CycleMonthly, 3000)
	env.mustPlan("pro", "专业版", CycleMonthly, 6000)
	sub := env.mustSub("basic")
	env.setNow(time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC))
	ch, _, _ := env.svc.ApplyChange(ApplyChangeInput{SubscriptionID: sub.ID, ToPlanID: "pro", EffectiveMode: EffectiveNextCycle})

	env.store.mu.Lock()
	seqBefore := env.store.seq
	env.store.mu.Unlock()
	env.setNow(time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC))
	if _, err := env.svc.AdvanceChange(ch.ID); err != nil {
		t.Fatalf("advance: %v", err)
	}
	env.store.mu.Lock()
	seqAfter := env.store.seq
	env.store.mu.Unlock()
	// bill + notification + effective 三事件同一事务追加。
	if seqAfter-seqBefore != 3 {
		t.Fatalf("advance events = %d, want 3", seqAfter-seqBefore)
	}
}

// ---------------- 场景 4：取消 / 生效 / 终止的确定顺序 ----------------

func TestTerminateThenAdvanceNoSwitch(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	env := newTestEnv(t, start)
	env.mustPlan("basic", "基础版", CycleMonthly, 3000)
	env.mustPlan("pro", "专业版", CycleMonthly, 6000)
	sub := env.mustSub("basic")
	env.setNow(time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC))
	ch, _, _ := env.svc.ApplyChange(ApplyChangeInput{SubscriptionID: sub.ID, ToPlanID: "pro", EffectiveMode: EffectiveNextCycle})

	// 终止：原子取消待生效变更。
	if err := env.svc.Terminate(sub.ID); err != nil {
		t.Fatalf("terminate: %v", err)
	}
	env.setNow(time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC))
	if _, err := env.svc.AdvanceChange(ch.ID); !errors.Is(err, ErrSubscriptionTerminated) {
		t.Fatalf("advance after terminate err = %v, want ErrSubscriptionTerminated", err)
	}
	got, _ := env.svc.GetSubscription(sub.ID)
	if got.Plan.PlanID != "basic" || got.Status != SubTerminated {
		t.Fatalf("after terminate+advance: plan=%s status=%s, want basic/terminated", got.Plan.PlanID, got.Status)
	}
	bills, _ := env.svc.ListBills(sub.ID)
	if len(bills) != 1 { // 仅首期，无续费
		t.Fatalf("bills after terminate = %d, want 1", len(bills))
	}
}

func TestLateCancelAfterEffectiveRejected(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	env := newTestEnv(t, start)
	env.mustPlan("basic", "基础版", CycleMonthly, 3000)
	env.mustPlan("pro", "专业版", CycleMonthly, 6000)
	sub := env.mustSub("basic")
	env.setNow(time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC))
	ch, _, _ := env.svc.ApplyChange(ApplyChangeInput{SubscriptionID: sub.ID, ToPlanID: "pro", EffectiveMode: EffectiveNextCycle})

	env.setNow(time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC))
	if _, err := env.svc.AdvanceChange(ch.ID); err != nil {
		t.Fatalf("advance: %v", err)
	}
	// 迟到取消：拒绝，已生效切换不能被还原。
	err := env.svc.CancelChange(CancelChangeInput{SubscriptionID: sub.ID, ChangeID: ch.ID})
	wantErrIs(t, err, ErrConflict)
	got, _ := env.svc.GetSubscription(sub.ID)
	if got.Plan.PlanID != "pro" {
		t.Fatalf("plan after late cancel = %s, want pro (irreversible)", got.Plan.PlanID)
	}
}

func TestCancelThenAdvanceAndTerminateIdempotent(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	env := newTestEnv(t, start)
	env.mustPlan("basic", "基础版", CycleMonthly, 3000)
	env.mustPlan("pro", "专业版", CycleMonthly, 6000)
	sub := env.mustSub("basic")
	env.setNow(time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC))
	ch, _, _ := env.svc.ApplyChange(ApplyChangeInput{SubscriptionID: sub.ID, ToPlanID: "pro", EffectiveMode: EffectiveNextCycle})

	if err := env.svc.CancelChange(CancelChangeInput{SubscriptionID: sub.ID, ChangeID: ch.ID, ExpectedVersion: &ch.Version}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	// 推进已取消变更：冲突，不出账。
	if _, err := env.svc.AdvanceChange(ch.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("advance cancelled err = %v, want ErrConflict", err)
	}
	// 取消后可以再次申请。
	ch2, _, err := env.svc.ApplyChange(ApplyChangeInput{SubscriptionID: sub.ID, ToPlanID: "pro", EffectiveMode: EffectiveNextCycle})
	if err != nil {
		t.Fatalf("re-apply after cancel: %v", err)
	}
	// 终止幂等性：重复终止报错但状态确定。
	if err := env.svc.Terminate(sub.ID); err != nil {
		t.Fatalf("terminate: %v", err)
	}
	if err := env.svc.Terminate(sub.ID); !errors.Is(err, ErrSubscriptionTerminated) {
		t.Fatalf("double terminate err = %v", err)
	}
	// 终止后 ch2 不能再生效。
	env.setNow(time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC))
	if _, err := env.svc.AdvanceChange(ch2.ID); !errors.Is(err, ErrSubscriptionTerminated) {
		t.Fatalf("advance ch2 after terminate err = %v", err)
	}
}

func TestConcurrentCancelAndAdvanceDeterministic(t *testing.T) {
	// 取消与推进同时到达：两种结果之一，且不可兼得——
	// 要么取消成功、推进失败且无账单；要么推进成功、取消失败。
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for iter := 0; iter < 20; iter++ {
		env := newTestEnv(t, start)
		env.mustPlan("basic", "基础版", CycleMonthly, 3000)
		env.mustPlan("pro", "专业版", CycleMonthly, 6000)
		sub := env.mustSub("basic")
		env.setNow(time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC))
		ch, _, _ := env.svc.ApplyChange(ApplyChangeInput{SubscriptionID: sub.ID, ToPlanID: "pro", EffectiveMode: EffectiveNextCycle})

		var wg sync.WaitGroup
		var cancelErr, advanceErr error
		var mu sync.Mutex
		wg.Add(2)
		go func() {
			defer wg.Done()
			err := env.svc.CancelChange(CancelChangeInput{SubscriptionID: sub.ID, ChangeID: ch.ID})
			mu.Lock()
			cancelErr = err
			mu.Unlock()
		}()
		go func() {
			defer wg.Done()
			_, err := env.svc.AdvanceChange(ch.ID)
			mu.Lock()
			advanceErr = err
			mu.Unlock()
		}()
		wg.Wait()

		cancelledOK := cancelErr == nil
		advancedOK := advanceErr == nil
		if cancelledOK == advancedOK {
			t.Fatalf("iter %d: exactly one of cancel/advance must win (cancelErr=%v advanceErr=%v)", iter, cancelErr, advanceErr)
		}
		got, _ := env.svc.GetSubscription(sub.ID)
		bills, _ := env.svc.ListBills(sub.ID)
		switch {
		case cancelledOK:
			if got.Plan.PlanID != "basic" || len(bills) != 1 {
				t.Fatalf("iter %d: cancel-won invariants broken plan=%s bills=%d", iter, got.Plan.PlanID, len(bills))
			}
		case advancedOK:
			if got.Plan.PlanID != "pro" || len(bills) != 2 {
				t.Fatalf("iter %d: advance-won invariants broken plan=%s bills=%d", iter, got.Plan.PlanID, len(bills))
			}
			if err := env.svc.CancelChange(CancelChangeInput{SubscriptionID: sub.ID, ChangeID: ch.ID}); !errors.Is(err, ErrConflict) {
				t.Fatalf("iter %d: late cancel after winning advance: %v", iter, err)
			}
		}
	}
}

// tenantBillsForTest 辅助：直接读取账单数量。
func (e *testEnv) tenantBillsForTest(t *testing.T, subID string) []*Bill {
	t.Helper()
	b, err := e.svc.ListBills(subID)
	if err != nil {
		t.Fatalf("list bills: %v", err)
	}
	return b
}

// ---------------- 场景 5：WAL 持久化与历史 ----------------

func TestWALReplayRestoresState(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/events.jsonl"
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	var changeID, subID string
	func() {
		store, err := Open(path)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		env := &testEnv{t: t, store: store, now: start}
		env.svc = NewService(store, env.clock)
		env.mustPlan("basic", "基础版", CycleMonthly, 3000)
		env.mustPlan("pro", "专业版", CycleMonthly, 6000)
		sub := env.mustSub("basic")
		subID = sub.ID
		env.setNow(time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC))
		ch, _, err := env.svc.ApplyChange(ApplyChangeInput{SubscriptionID: sub.ID, ToPlanID: "pro", EffectiveMode: EffectiveNextCycle})
		if err != nil {
			t.Fatalf("apply: %v", err)
		}
		changeID = ch.ID
		env.setNow(time.Date(2026, 1, 20, 0, 0, 0, 0, time.UTC))
		if _, err := env.svc.AddPlanPrice("pro", 7000, time.Date(2026, 1, 25, 0, 0, 0, 0, time.UTC)); err != nil {
			t.Fatalf("price add: %v", err)
		}
		if err := store.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
	}()

	// 重放
	store2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer store2.Close()
	env2 := &testEnv{t: t, store: store2, now: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)}
	env2.svc = NewService(store2, env2.clock)

	ch, err := env2.svc.GetChange(changeID)
	if err != nil {
		t.Fatalf("get change after replay: %v", err)
	}
	if ch.ToPlan.PriceCents != 6000 {
		t.Fatalf("frozen price after replay = %d, want 6000", ch.ToPlan.PriceCents)
	}
	adv, err := env2.svc.AdvanceChange(changeID)
	if err != nil {
		t.Fatalf("advance after replay: %v", err)
	}
	if adv.Bill.AmountCents != 6000 {
		t.Fatalf("renewal after replay = %d, want frozen 6000", adv.Bill.AmountCents)
	}
	history, err := env2.svc.ListChanges(subID)
	if err != nil || len(history) != 1 {
		t.Fatalf("history after replay = %d (%v)", len(history), err)
	}
}

func TestChangeHistoryPersisted(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	env := newTestEnv(t, start)
	env.mustPlan("basic", "基础版", CycleMonthly, 3000)
	env.mustPlan("pro", "专业版", CycleMonthly, 6000)
	env.mustPlan("ultra", "旗舰版", CycleMonthly, 9000)
	sub := env.mustSub("basic")
	env.setNow(time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC))
	c1, _, _ := env.svc.ApplyChange(ApplyChangeInput{SubscriptionID: sub.ID, ToPlanID: "pro", EffectiveMode: EffectiveNextCycle})
	if err := env.svc.CancelChange(CancelChangeInput{SubscriptionID: sub.ID, ChangeID: c1.ID}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	c2, _, _ := env.svc.ApplyChange(ApplyChangeInput{SubscriptionID: sub.ID, ToPlanID: "ultra", EffectiveMode: EffectiveNextCycle})
	env.setNow(time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC))
	if _, err := env.svc.AdvanceChange(c2.ID); err != nil {
		t.Fatalf("advance: %v", err)
	}
	history, _ := env.svc.ListChanges(sub.ID)
	if len(history) != 2 {
		t.Fatalf("history = %d, want 2", len(history))
	}
	if history[0].Status != ChangeCancelled || history[1].Status != ChangeEffective {
		t.Fatalf("history statuses = %s,%s, want cancelled,effective", history[0].Status, history[1].Status)
	}
	if !history[0].CancelledAt.IsZero() && history[1].EffectiveAt == nil {
		t.Fatalf("timestamps not recorded")
	}
	if history[0].CancelledAt == nil || history[1].EffectiveAt == nil {
		t.Fatalf("timestamps missing: %+v %+v", history[0], history[1])
	}
}
