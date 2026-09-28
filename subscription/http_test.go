package subscription

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestImmediateDowngradeRefund(t *testing.T) {
	// 30 天周期（4月），pro 6000 → basic 3000，第 11 天（剩 20 天）降级：
	// 差额 = (3000-6000)*20/30 = -2000，应退 2000。
	start := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	env := newTestEnv(t, start)
	env.mustPlan("basic", "基础版", CycleMonthly, 3000)
	env.mustPlan("pro", "专业版", CycleMonthly, 6000)
	sub := env.mustSub("pro")
	env.setNow(time.Date(2026, 4, 11, 0, 0, 0, 0, time.UTC))

	ch, adv, err := env.svc.ApplyChange(ApplyChangeInput{SubscriptionID: sub.ID, ToPlanID: "basic", EffectiveMode: EffectiveImmediate})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if ch.DiffCents != -2000 || ch.DiffDirection != BillRefund {
		t.Fatalf("diff = %d/%s, want -2000/refund", ch.DiffCents, ch.DiffDirection)
	}
	if adv.Bill == nil || adv.Bill.Direction != BillRefund || adv.Bill.AmountCents != 2000 || adv.Bill.Kind != BillKindProration {
		t.Fatalf("refund bill = %+v", adv.Bill)
	}
	// 账单可按 ID 查询。
	got, err := env.svc.GetBill(adv.Bill.ID)
	if err != nil || got.ID != adv.Bill.ID {
		t.Fatalf("get bill: %v %+v", err, got)
	}
}

func TestHTTPEndToEnd(t *testing.T) {
	start := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	env := newTestEnv(t, start)
	srv := httptest.NewServer(NewServer(env.svc).Handler())
	defer srv.Close()

	post := func(path string, body any, out any) int {
		t.Helper()
		b, _ := json.Marshal(body)
		resp, err := http.Post(srv.URL+path, "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		defer resp.Body.Close()
		if out != nil && resp.StatusCode < 300 {
			if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
				t.Fatalf("decode: %v", err)
			}
		}
		return resp.StatusCode
	}

	// 配置两个套餐
	if code := post("/api/v1/plans", planRequest{ID: "basic", Name: "基础版", Cycle: CycleMonthly, PriceCents: 3000}, nil); code != http.StatusCreated {
		t.Fatalf("create basic = %d", code)
	}
	if code := post("/api/v1/plans", planRequest{ID: "pro", Name: "专业版", Cycle: CycleMonthly, PriceCents: 6000}, nil); code != http.StatusCreated {
		t.Fatalf("create pro = %d", code)
	}

	// 创建订阅
	var subResp struct {
		Subscription *Subscription `json:"subscription"`
	}
	if code := post("/api/v1/subscriptions", createSubRequest{PlanID: "basic"}, &subResp); code != http.StatusCreated {
		t.Fatalf("create sub = %d", code)
	}
	subID := subResp.Subscription.ID

	// 预约下一周期升级
	env.setNow(time.Date(2026, 4, 5, 0, 0, 0, 0, time.UTC))
	var chResp struct {
		Change *ChangeRequest `json:"change"`
	}
	if code := post("/api/v1/changes", applyChangeRequest{SubscriptionID: subID, ExternalID: "EXT-1", ToPlanID: "pro", EffectiveMode: EffectiveNextCycle}, &chResp); code != http.StatusCreated {
		t.Fatalf("apply change = %d", code)
	}
	changeID := chResp.Change.ID

	// 第二笔：409 + pending_change_exists
	code := post("/api/v1/changes", applyChangeRequest{SubscriptionID: subID, ToPlanID: "pro", EffectiveMode: EffectiveImmediate}, nil)
	if code != http.StatusConflict {
		t.Fatalf("second pending = %d, want 409", code)
	}

	// 同号不同参数：409
	code = post("/api/v1/changes", applyChangeRequest{SubscriptionID: subID, ExternalID: "EXT-1", ToPlanID: "basic", EffectiveMode: EffectiveNextCycle}, nil)
	if code != http.StatusConflict {
		t.Fatalf("external conflict = %d, want 409", code)
	}

	// 提前推进：409 not_due
	resp, err := http.Post(srv.URL+"/api/v1/changes/"+changeID+"/advance", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("early advance = %d, want 409", resp.StatusCode)
	}

	// 到边界推进：200，续费账单 6000
	env.setNow(time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC))
	resp, err = http.Post(srv.URL+"/api/v1/changes/"+changeID+"/advance", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("advance = %d", resp.StatusCode)
	}
	var adv advanceResponse
	if err := json.NewDecoder(resp.Body).Decode(&adv); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !adv.Advanced || adv.Bill == nil || adv.Bill.AmountCents != 6000 || adv.Notification == nil {
		t.Fatalf("advance response = %+v", adv)
	}

	// 重复推进：200 但 advanced=false，账单不变
	resp, _ = http.Post(srv.URL+"/api/v1/changes/"+changeID+"/advance", "application/json", nil)
	var adv2 advanceResponse
	json.NewDecoder(resp.Body).Decode(&adv2)
	resp.Body.Close()
	if adv2.Advanced || adv2.Bill == nil || adv2.Bill.ID != adv.Bill.ID {
		t.Fatalf("replay must return same bill, got %+v", adv2)
	}

	// 账单列表：initial + renewal 两张
	resp, err = http.Get(srv.URL + "/api/v1/subscriptions/" + subID + "/bills")
	if err != nil {
		t.Fatal(err)
	}
	var bills []*Bill
	json.NewDecoder(resp.Body).Decode(&bills)
	resp.Body.Close()
	if len(bills) != 2 || bills[1].Kind != BillKindRenewal || bills[1].AmountCents != 6000 {
		t.Fatalf("bills = %s", dump(bills))
	}

	// 变更历史查询
	resp, _ = http.Get(srv.URL + "/api/v1/subscriptions/" + subID + "/changes")
	var history []*ChangeRequest
	json.NewDecoder(resp.Body).Decode(&history)
	resp.Body.Close()
	if len(history) != 1 || history[0].Status != ChangeEffective {
		t.Fatalf("history = %s", dump(history))
	}
}

func TestHTTPReplaceCancelVersionFlow(t *testing.T) {
	start := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	env := newTestEnv(t, start)
	srv := httptest.NewServer(NewServer(env.svc).Handler())
	defer srv.Close()

	must := func(req *http.Request, want int) *http.Response {
		t.Helper()
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != want {
			t.Fatalf("%s %s = %d, want %d", req.Method, req.URL.Path, resp.StatusCode, want)
		}
		return resp
	}
	postJSON := func(path string, body any) *http.Request {
		t.Helper()
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest(http.MethodPost, srv.URL+path, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		return req
	}

	must(postJSON("/api/v1/plans", planRequest{ID: "basic", Cycle: CycleMonthly, PriceCents: 3000}), http.StatusCreated).Body.Close()
	must(postJSON("/api/v1/plans", planRequest{ID: "pro", Cycle: CycleMonthly, PriceCents: 6000}), http.StatusCreated).Body.Close()
	var subResp struct {
		Subscription *Subscription `json:"subscription"`
	}
	r := must(postJSON("/api/v1/subscriptions", createSubRequest{PlanID: "basic"}), http.StatusCreated)
	json.NewDecoder(r.Body).Decode(&subResp)
	r.Body.Close()

	env.setNow(time.Date(2026, 4, 5, 0, 0, 0, 0, time.UTC))
	var chResp struct{ Change *ChangeRequest }
	r = must(postJSON("/api/v1/changes", applyChangeRequest{SubscriptionID: subResp.Subscription.ID, ToPlanID: "pro", EffectiveMode: EffectiveNextCycle}), http.StatusCreated)
	json.NewDecoder(r.Body).Decode(&chResp)
	r.Body.Close()
	changeID := chResp.Change.ID

	// 用错误版本号替换：409
	b, _ := json.Marshal(replaceChangeRequest{ToPlanID: "pro", EffectiveMode: EffectiveNextCycle, ExpectedVersion: ptr(7)})
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/api/v1/changes/"+changeID, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp := must(req, http.StatusConflict)
	var er errorResponse
	json.NewDecoder(resp.Body).Decode(&er)
	resp.Body.Close()
	if er.Code != "version_mismatch" {
		t.Fatalf("error code = %s, want version_mismatch", er.Code)
	}

	// 取消
	req, _ = http.NewRequest(http.MethodDelete, srv.URL+"/api/v1/changes/"+changeID, nil)
	must(req, http.StatusOK).Body.Close()

	// 取消后推进：409
	must(postJSON("/api/v1/changes/"+changeID+"/advance", nil), http.StatusConflict).Body.Close()

	// 已取消后再取消（无 pending）：404
	req, _ = http.NewRequest(http.MethodDelete, srv.URL+"/api/v1/changes/"+changeID, nil)
	must(req, http.StatusNotFound).Body.Close()
}

func ptr[T any](v T) *T { return &v }

func dump(v any) string { b, _ := json.Marshal(v); return string(b) }
