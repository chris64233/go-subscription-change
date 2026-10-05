package gosubscriptionchange

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

// ---- 竞态：支付确认 × 取消 ----

// TestPaymentVsCancelRace 支付与取消并发：取消总是生效；
// 支付要么先于取消提交（记录保留作审计），要么被拒绝，
// 绝不出现“取消后支付又把变更激活”的状态。
func TestPaymentVsCancelRace(t *testing.T) {
	for iter := 0; iter < 40; iter++ {
		t.Run(fmt.Sprintf("iter%d", iter), func(t *testing.T) {
			f, ch := paidFixture(t)

			var wg sync.WaitGroup
			start := make(chan struct{})
			var payErr, cancelErr error
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				_, payErr = f.svc.SubmitPayment(f.ctx, ch.ID, PaymentInput{
					ClientToken: "p", Amount: 2000, Result: PaymentSucceeded,
				})
			}()
			go func() { defer wg.Done(); <-start; _, cancelErr = f.svc.CancelChange(f.ctx, "s1") }()
			close(start)
			wg.Wait()

			if cancelErr != nil {
				t.Fatalf("cancel err = %v (pending change must be cancellable)", cancelErr)
			}
			if payErr != nil && !errors.Is(payErr, ErrNotPending) {
				t.Fatalf("pay err = %v want nil or ErrNotPending", payErr)
			}
			got, _ := f.svc.GetChange(f.ctx, ch.ID)
			sub, _ := f.svc.GetSubscription(f.ctx, "s1")
			if got.Status != ChangeCancelled || sub.CurrentPlanID != "a" || sub.PendingChangeID != "" {
				t.Fatalf("invariant broken: change=%s plan=%s pending=%s",
					got.Status, sub.CurrentPlanID, sub.PendingChangeID)
			}
			// 取消后推进：绝不生效、绝不出账（即便支付先提交也要随取消而失效）。
			f.clock.Set(mustDate(t, "2026-02-10T00:00:00Z"))
			if _, err := f.svc.AdvanceSubscription(f.ctx, "s1"); !errors.Is(err, ErrNotPending) {
				t.Fatalf("advance after cancel err = %v", err)
			}
			if len(f.bills("s1")) != 0 {
				t.Fatal("cancelled change billed")
			}
		})
	}
}

// ---- 竞态：支付确认 × 到期推进 ----

// TestPaymentVsAdvanceRace 到期时刻支付与推进并发，以事务提交先后决定终局：
// 支付先提交则生效出账；推进先提交则失败保留原套餐，迟到的支付被拒绝。
func TestPaymentVsAdvanceRace(t *testing.T) {
	for iter := 0; iter < 40; iter++ {
		t.Run(fmt.Sprintf("iter%d", iter), func(t *testing.T) {
			f, ch := paidFixture(t)
			f.clock.Set(mustDate(t, "2026-02-10T00:00:00Z")) // 正好到达生效时间

			var wg sync.WaitGroup
			start := make(chan struct{})
			var payErr error
			var advRes *EffectiveResult
			var advErr error
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				_, payErr = f.svc.SubmitPayment(f.ctx, ch.ID, PaymentInput{
					ClientToken: "p", Amount: 2000, Result: PaymentSucceeded,
				})
			}()
			go func() { defer wg.Done(); <-start; advRes, advErr = f.svc.AdvanceSubscription(f.ctx, "s1") }()
			close(start)
			wg.Wait()

			got, _ := f.svc.GetChange(f.ctx, ch.ID)
			sub, _ := f.svc.GetSubscription(f.ctx, "s1")
			bills := f.bills("s1")
			switch got.Status {
			case ChangeEffective:
				// 支付先提交：推进成功，恰好一张账单，套餐已切换。
				if payErr != nil || advErr != nil || advRes.Bill == nil {
					t.Fatalf("effective branch: payErr=%v advErr=%v", payErr, advErr)
				}
				if sub.CurrentPlanID != "b" || len(bills) != 1 || bills[0].Amount != 2000 {
					t.Fatalf("effective branch invariant broken: plan=%s bills=%d", sub.CurrentPlanID, len(bills))
				}
			case ChangeFailed:
				// 推进先提交：未支付而失败，迟到的支付被拒绝，保留原套餐。
				if !errors.Is(payErr, ErrNotPending) {
					t.Fatalf("failed branch: late payErr = %v want ErrNotPending", payErr)
				}
				if advErr != nil || advRes.Bill != nil || got.FailureReason == "" {
					t.Fatalf("failed branch: advErr=%v bill=%v reason=%q", advErr, advRes.Bill, got.FailureReason)
				}
				if sub.CurrentPlanID != "a" || len(bills) != 0 {
					t.Fatalf("failed branch invariant broken: plan=%s bills=%d", sub.CurrentPlanID, len(bills))
				}
			default:
				t.Fatalf("unexpected final status %s", got.Status)
			}
		})
	}
}
