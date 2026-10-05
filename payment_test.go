package gosubscriptionchange

import (
	"errors"
	"testing"
)

// paidFixture 创建一笔需要支付的 NEXT_PERIOD 待生效变更（差额 2000，边界 2/10）。
func paidFixture(t *testing.T) (*fixture, *ChangeApplication) {
	t.Helper()
	f := newFixture(t, "2026-01-10T00:00:00Z")
	f.createPlan("a", "A", 1000)
	f.createPlan("b", "B", 2000)
	f.createSub("s1", "a")
	ch := f.mustChange("s1", "chg-1", "b", EffectiveNextPeriod)
	if ch.SettlementAmount != 2000 || !ch.paymentRequired() {
		t.Fatalf("settlement = %d want 2000 (payment required)", ch.SettlementAmount)
	}
	return f, ch
}

// ---- 支付确认后到期生效 ----

func TestPaymentThenAdvanceEffective(t *testing.T) {
	f, ch := paidFixture(t)

	// 支付失败可换新支付号重试。
	fp, err := f.svc.SubmitPayment(f.ctx, ch.ID, PaymentInput{
		ClientToken: "p-fail", Amount: 2000, Result: PaymentFailed, FailureReason: "card declined",
	})
	if err != nil {
		t.Fatalf("record failed payment: %v", err)
	}
	if fp.Status != PaymentFailed || fp.FailureReason != "card declined" {
		t.Fatalf("failed payment = %+v", fp)
	}
	got, _ := f.svc.GetChange(f.ctx, ch.ID)
	if got.paid() {
		t.Fatal("failed payment must not mark change paid")
	}

	p := f.mustPay(ch.ID, "p-ok", 2000)
	if p.Status != PaymentSucceeded {
		t.Fatalf("payment = %+v", p)
	}
	got, _ = f.svc.GetChange(f.ctx, ch.ID)
	if !got.paid() {
		t.Fatal("change not marked paid")
	}

	// 到期推进：生效并按冻结金额出账。
	f.clock.Set(mustDate(t, "2026-02-10T00:00:00Z"))
	res, err := f.svc.AdvanceSubscription(f.ctx, "s1")
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if res.Change.Status != ChangeEffective || res.Bill.Amount != 2000 {
		t.Fatalf("result = %+v", res)
	}
	// 支付尝试都可查（失败 1 笔 + 成功 1 笔）。
	ps, err := f.svc.ListPayments(f.ctx, ch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 2 || ps[0].Status != PaymentFailed || ps[1].Status != PaymentSucceeded {
		t.Fatalf("payments = %+v", ps)
	}
}

// ---- 重复支付：幂等、冲突与唯一确认 ----

func TestDuplicatePayment(t *testing.T) {
	f, ch := paidFixture(t)
	first := f.mustPay(ch.ID, "p-1", 2000)

	// 同支付号同内容重试：原样返回，不新建。
	again, err := f.svc.SubmitPayment(f.ctx, ch.ID, PaymentInput{
		ClientToken: "p-1", Amount: 2000, Result: PaymentSucceeded,
	})
	if err != nil || again.ID != first.ID {
		t.Fatalf("idempotent retry = %v/%v", again, err)
	}
	// 同支付号不同结果：冲突。
	_, err = f.svc.SubmitPayment(f.ctx, ch.ID, PaymentInput{
		ClientToken: "p-1", Amount: 2000, Result: PaymentFailed,
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("token reuse err = %v want ErrConflict", err)
	}
	// 换支付号重复确认：拒绝，一笔变更至多一笔成功支付。
	_, err = f.svc.SubmitPayment(f.ctx, ch.ID, PaymentInput{
		ClientToken: "p-2", Amount: 2000, Result: PaymentSucceeded,
	})
	if !errors.Is(err, ErrAlreadyPaid) {
		t.Fatalf("second success err = %v want ErrAlreadyPaid", err)
	}
	ps, _ := f.svc.ListPayments(f.ctx, ch.ID)
	if len(ps) != 1 {
		t.Fatalf("payments = %d want 1 (no duplicate records)", len(ps))
	}
}

func TestPaymentValidation(t *testing.T) {
	f, ch := paidFixture(t)

	// 金额必须等于冻结差额。
	if _, err := f.svc.SubmitPayment(f.ctx, ch.ID, PaymentInput{Amount: 1999, Result: PaymentSucceeded}); !errors.Is(err, ErrConflict) {
		t.Fatalf("wrong amount err = %v want ErrConflict", err)
	}
	// 非法结果。
	if _, err := f.svc.SubmitPayment(f.ctx, ch.ID, PaymentInput{Amount: 2000, Result: "MAYBE"}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("bad result err = %v want ErrInvalidArgument", err)
	}
	// 无需支付的变更（立即变更）拒绝支付。
	f.createSub("s2", "a")
	ch2 := f.mustChange("s2", "chg-2", "b", EffectiveImmediate)
	if _, err := f.svc.SubmitPayment(f.ctx, ch2.ID, PaymentInput{Amount: 1, Result: PaymentSucceeded}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("pay immediate err = %v want ErrInvalidArgument", err)
	}
	// 不存在的变更。
	if _, err := f.svc.SubmitPayment(f.ctx, "change-999", PaymentInput{Amount: 1, Result: PaymentSucceeded}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing change err = %v want ErrNotFound", err)
	}
}

// ---- 迟到的旧支付不能激活已取消/已生效的变更 ----

func TestLatePaymentCannotActivateClosedChange(t *testing.T) {
	f, ch := paidFixture(t)

	// 取消后到达的支付：拒绝，变更保持 CANCELLED，套餐不变。
	if _, err := f.svc.CancelChange(f.ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	_, err := f.svc.SubmitPayment(f.ctx, ch.ID, PaymentInput{
		ClientToken: "late-1", Amount: 2000, Result: PaymentSucceeded,
	})
	if !errors.Is(err, ErrNotPending) {
		t.Fatalf("late pay after cancel err = %v want ErrNotPending", err)
	}
	got, _ := f.svc.GetChange(f.ctx, ch.ID)
	if got.Status != ChangeCancelled || got.paid() {
		t.Fatalf("cancelled change activated by late payment: %+v", got)
	}
	sub, _ := f.svc.GetSubscription(f.ctx, "s1")
	if sub.CurrentPlanID != "a" {
		t.Fatal("plan switched by late payment")
	}
	if ps, _ := f.svc.ListPayments(f.ctx, ch.ID); len(ps) != 0 {
		t.Fatalf("rejected payment must not be recorded, got %d", len(ps))
	}

	// 已生效的变更：迟到的重复支付同样被拒绝，不会二次收费。
	f2, ch2 := paidFixture(t)
	f2.mustPay(ch2.ID, "p-1", 2000)
	f2.clock.Set(mustDate(t, "2026-02-10T00:00:00Z"))
	if _, err := f2.svc.AdvanceSubscription(f2.ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	_, err = f2.svc.SubmitPayment(f2.ctx, ch2.ID, PaymentInput{
		ClientToken: "late-2", Amount: 2000, Result: PaymentSucceeded,
	})
	if !errors.Is(err, ErrNotPending) {
		t.Fatalf("late pay after effective err = %v want ErrNotPending", err)
	}
	if bs := f2.bills("s1"); len(bs) != 1 {
		t.Fatalf("bills = %d want 1 (no double charge)", len(bs))
	}
}
