package subscription

import (
	"bufio"
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestWALImmediateChangeEventPurityAndReplay(t *testing.T) {
	path := t.TempDir() + "/events.jsonl"
	start := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)

	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	env := &testEnv{t: t, store: store, now: start}
	env.svc = NewService(store, env.clock)
	env.mustPlan("basic", "基础版", CycleMonthly, 3000)
	env.mustPlan("pro", "专业版", CycleMonthly, 6000)
	sub := env.mustSub("basic")
	env.setNow(time.Date(2026, 4, 11, 0, 0, 0, 0, time.UTC))
	ch, adv, err := env.svc.ApplyChange(ApplyChangeInput{SubscriptionID: sub.ID, ToPlanID: "pro", EffectiveMode: EffectiveImmediate})
	if err != nil {
		t.Fatal(err)
	}
	if !adv.Advanced {
		t.Fatal("immediate change must settle")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// 校验 WAL 事件内容：change_applied=pending，change_effective=effective。
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var appliedStatus, effectiveStatus string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		if e.Type == EvChangeApplied && e.Change != nil {
			appliedStatus = e.Change.Status
		}
		if e.Type == EvChangeEffective && e.Change != nil {
			effectiveStatus = e.Change.Status
			if e.Change.BillID == "" || e.Change.NotificationID == "" {
				t.Fatal("effective event must reference bill and notification")
			}
		}
	}
	if appliedStatus != ChangePending {
		t.Fatalf("applied event status = %q, want %q (event purity)", appliedStatus, ChangePending)
	}
	if effectiveStatus != ChangeEffective {
		t.Fatalf("effective event status = %q, want %q", effectiveStatus, ChangeEffective)
	}

	// 重放：立即变更最终为 effective，只一张差额账单、一条通知。
	store2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()
	err = store2.View(func(v View) error {
		got, ok := v.Change(ch.ID)
		if !ok {
			t.Fatal("change missing after replay")
		}
		if got.Status != ChangeEffective {
			t.Fatalf("after replay status = %s, want effective", got.Status)
		}
		if b, ok := v.BillByChange(ch.ID); !ok || b.AmountCents != 2000 || b.Direction != BillCharge {
			t.Fatalf("after replay proration bill = %+v, want charge 2000", b)
		}
		if n, ok := v.NotificationByDedup("change-effective:" + ch.ID); !ok || n.ChangeID != ch.ID {
			t.Fatalf("notification after replay = %+v", n)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestWALImmediateZeroDiffNoBill(t *testing.T) {
	// 等价套餐立即互转（同价）：差额 0，不出账单，但通知仍唯一。
	path := t.TempDir() + "/e.jsonl"
	store, _ := Open(path)
	defer store.Close()
	env := &testEnv{t: t, store: store, now: time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)}
	env.svc = NewService(store, env.clock)
	env.mustPlan("a", "A", CycleMonthly, 3000)
	env.mustPlan("b", "B", CycleMonthly, 3000)
	sub := env.mustSub("a")
	env.setNow(time.Date(2026, 4, 11, 0, 0, 0, 0, time.UTC))
	ch, adv, err := env.svc.ApplyChange(ApplyChangeInput{SubscriptionID: sub.ID, ToPlanID: "b", EffectiveMode: EffectiveImmediate})
	if err != nil {
		t.Fatal(err)
	}
	if ch.DiffCents != 0 {
		t.Fatalf("diff = %d, want 0", ch.DiffCents)
	}
	if adv.Bill != nil {
		t.Fatalf("zero diff must not write bill, got %+v", adv.Bill)
	}
	if adv.Notification == nil || !adv.Advanced {
		t.Fatal("zero-diff switch still effective with exactly one notification")
	}
	bills, _ := env.svc.ListBills(sub.ID)
	if len(bills) != 1 { // 仅首期
		t.Fatalf("bills = %d, want 1", len(bills))
	}
	// 重放幂等：immediate 在受理事务内已生效，再次推进返回原通知、不出账。
	r, err := env.svc.AdvanceChange(ch.ID)
	if err != nil || r.Advanced || r.Bill != nil || r.Notification == nil {
		t.Fatalf("replay zero-diff: %+v %v", r, err)
	}
}
