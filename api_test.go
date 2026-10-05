package gosubscriptionchange

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newTestServer(t *testing.T, now string) (http.Handler, *Service, *FakeClock) {
	t.Helper()
	clk := NewFakeClock(mustDate(t, now))
	st := NewStore()
	svc := NewService(st)
	h := NewAPIWithClock(svc, clk)
	return h, svc, clk
}

func doJSON(t *testing.T, h http.Handler, method, path string, body any) (int, any) {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var out any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode response %q: %v", rec.Body.String(), err)
		}
	}
	return rec.Code, out
}

func asMap(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("response is not a JSON object: %#v", v)
	}
	return m
}

func asSlice(t *testing.T, v any) []any {
	t.Helper()
	s, ok := v.([]any)
	if !ok {
		t.Fatalf("response is not a JSON array: %#v", v)
	}
	return s
}

func TestAPIFullFlow(t *testing.T) {
	// 1/1 建套餐与订阅（周期 1/1~2/1），时钟走到 1/11 再申请立即变更（剩21天）。
	h, _, clk := newTestServer(t, "2026-01-01T00:00:00Z")

	// 创建两个套餐。
	if st, _ := doJSON(t, h, "POST", "/plans", map[string]any{"id": "basic", "name": "Basic", "currency": "CNY", "price": 1000}); st != http.StatusCreated {
		t.Fatalf("create basic status=%d", st)
	}
	if st, _ := doJSON(t, h, "POST", "/plans", map[string]any{"id": "pro", "name": "Pro", "currency": "CNY", "price": 4100}); st != http.StatusCreated {
		t.Fatalf("create pro status=%d", st)
	}
	// 重复 ID -> 409。
	if st, _ := doJSON(t, h, "POST", "/plans", map[string]any{"id": "basic", "name": "X", "currency": "CNY", "price": 1}); st != http.StatusConflict {
		t.Fatalf("dup plan status=%d want 409", st)
	}
	// 缺字段 -> 400。
	if st, body := doJSON(t, h, "POST", "/plans", map[string]any{"name": "x"}); st != http.StatusBadRequest {
		t.Fatalf("invalid plan status=%d body=%v", st, body)
	}

	// 套餐列表/详情。
	if st, ps := doJSON(t, h, "GET", "/plans", nil); st != 200 || len(asSlice(t, ps)) != 2 {
		t.Fatalf("list plans st=%d", st)
	}
	if st, p := doJSON(t, h, "GET", "/plans/pro", nil); st != 200 || asMap(t, p)["id"] != "pro" {
		t.Fatalf("get plan st=%d", st)
	}
	if st, _ := doJSON(t, h, "GET", "/plans/nope", nil); st != http.StatusNotFound {
		t.Fatalf("missing plan st=%d", st)
	}

	// 创建订阅。
	if st, s := doJSON(t, h, "POST", "/subscriptions", map[string]any{"id": "s1", "planId": "basic"}); st != http.StatusCreated || asMap(t, s)["id"] != "s1" {
		t.Fatalf("create sub st=%d", st)
	}

	// 时钟走到 1/11：周期剩 21 天，立即变更差额 3100*21/31=2100。
	clk.Set(mustDate(t, "2026-01-11T00:00:00Z"))

	// 申请立即变更（应补 2100）。
	st, chAny := doJSON(t, h, "POST", "/subscriptions/s1/changes",
		map[string]any{"clientToken": "req-1", "targetPlanId": "pro", "effectiveType": "IMMEDIATE"})
	ch := asMap(t, chAny)
	if st != http.StatusCreated {
		t.Fatalf("apply change st=%d body=%v", st, chAny)
	}
	changeID := ch["id"].(string)
	if int64(ch["settlementAmount"].(float64)) != 2100 {
		t.Fatalf("settlement=%v", ch["settlementAmount"])
	}
	// 幂等重试：同 token 同内容 -> 201 返回同一申请。
	if st, ch2 := doJSON(t, h, "POST", "/subscriptions/s1/changes",
		map[string]any{"clientToken": "req-1", "targetPlanId": "pro", "effectiveType": "IMMEDIATE"}); st != http.StatusCreated || asMap(t, ch2)["id"] != changeID {
		t.Fatalf("idempotent retry st=%d", st)
	}
	// 第二笔待生效变更 -> 409。
	if st, _ := doJSON(t, h, "POST", "/subscriptions/s1/changes",
		map[string]any{"targetPlanId": "pro", "effectiveType": "NEXT_PERIOD"}); st != http.StatusConflict {
		t.Fatalf("second pending st=%d want 409", st)
	}
	// 同 token 换目标 -> 409。
	if st, _ := doJSON(t, h, "POST", "/subscriptions/s1/changes",
		map[string]any{"clientToken": "req-1", "targetPlanId": "basic", "effectiveType": "IMMEDIATE"}); st != http.StatusConflict {
		t.Fatalf("token conflict st=%d want 409", st)
	}
	// 变更详情。
	if st, got := doJSON(t, h, "GET", "/changes/"+changeID, nil); st != 200 || asMap(t, got)["id"] != changeID {
		t.Fatalf("get change st=%d", st)
	}

	// 到期推进（立即变更即刻到期）：原子生效。
	if st, res := doJSON(t, h, "POST", "/subscriptions/s1/advance", nil); st != 200 {
		t.Fatalf("advance st=%d res=%v", st, res)
	}
	// 再次推进 -> 409，不能重复收费。
	if st, _ := doJSON(t, h, "POST", "/subscriptions/s1/advance", nil); st != http.StatusConflict {
		t.Fatalf("double advance st=%d want 409", st)
	}
	// 订阅已切到 pro。
	if st, s := doJSON(t, h, "GET", "/subscriptions/s1", nil); st != 200 || asMap(t, s)["currentPlanId"] != "pro" {
		t.Fatalf("sub after advance st=%d", st)
	}
	// 账单：恰好一张 +2100。
	if st, bsAny := doJSON(t, h, "GET", "/subscriptions/s1/bills", nil); st != 200 {
		t.Fatalf("bills st=%d", st)
	} else {
		bills := asSlice(t, bsAny)
		if len(bills) != 1 || int64(asMap(t, bills[0])["amount"].(float64)) != 2100 {
			t.Fatalf("bills = %v", bills)
		}
	}
	// 历史：3 条（创建、受理、生效）。
	if st, hs := doJSON(t, h, "GET", "/subscriptions/s1/history", nil); st != 200 || len(asSlice(t, hs)) != 3 {
		t.Fatalf("history st=%d", st)
	}
	// 终止。
	if st, s := doJSON(t, h, "POST", "/subscriptions/s1/terminate", nil); st != 200 || asMap(t, s)["status"] != SubTerminated {
		t.Fatalf("terminate st=%d", st)
	}
	if st, _ := doJSON(t, h, "POST", "/subscriptions/s1/advance", nil); st != http.StatusConflict {
		t.Fatalf("advance after terminate st=%d want 409", st)
	}
}

func TestAPICancelAndReplace(t *testing.T) {
	h, _, clk := newTestServer(t, "2026-01-10T00:00:00Z")
	must201 := func(st int) {
		t.Helper()
		if st != 201 {
			t.Fatalf("setup status=%d want 201", st)
		}
	}
	st, _ := doJSON(t, h, "POST", "/plans", map[string]any{"id": "a", "name": "A", "currency": "CNY", "price": 1000})
	must201(st)
	st, _ = doJSON(t, h, "POST", "/plans", map[string]any{"id": "b", "name": "B", "currency": "CNY", "price": 2000})
	must201(st)
	st, _ = doJSON(t, h, "POST", "/plans", map[string]any{"id": "c", "name": "C", "currency": "CNY", "price": 3000})
	must201(st)
	st, _ = doJSON(t, h, "POST", "/subscriptions", map[string]any{"id": "s1", "planId": "a"})
	must201(st)

	st, chAny := doJSON(t, h, "POST", "/subscriptions/s1/changes",
		map[string]any{"clientToken": "r1", "targetPlanId": "b", "effectiveType": "NEXT_PERIOD"})
	if st != 201 {
		t.Fatalf("apply st=%d", st)
	}
	oldID := asMap(t, chAny)["id"].(string)

	// 替换为 c / IMMEDIATE，新 token。
	st, ch2Any := doJSON(t, h, "PUT", "/subscriptions/s1/changes",
		map[string]any{"clientToken": "r2", "targetPlanId": "c", "effectiveType": "IMMEDIATE"})
	ch2 := asMap(t, ch2Any)
	if st != 200 {
		t.Fatalf("replace st=%d body=%v", st, ch2Any)
	}
	if ch2["supersedesChangeId"] != oldID {
		t.Fatalf("supersedes=%v want %s", ch2["supersedesChangeId"], oldID)
	}
	// 旧申请 SUPERSEDED。
	st, oldAny := doJSON(t, h, "GET", "/changes/"+oldID, nil)
	if st != 200 || asMap(t, oldAny)["status"] != ChangeSuperseded {
		t.Fatalf("old change = %v", oldAny)
	}

	// 取消当前待生效。
	st, cancelledAny := doJSON(t, h, "DELETE", "/subscriptions/s1/changes", nil)
	if st != 200 || asMap(t, cancelledAny)["status"] != ChangeCancelled {
		t.Fatalf("cancel st=%d body=%v", st, cancelledAny)
	}
	// 再次取消 -> 409。
	if st, _ := doJSON(t, h, "DELETE", "/subscriptions/s1/changes", nil); st != http.StatusConflict {
		t.Fatalf("double cancel st=%d", st)
	}
	// 时钟走到边界后推进：已取消，不生效。
	clk.Set(mustDate(t, "2026-02-01T00:00:00Z"))
	if st, _ := doJSON(t, h, "POST", "/subscriptions/s1/advance", nil); st != http.StatusConflict {
		t.Fatalf("advance cancelled st=%d want 409", st)
	}
	if st, s := doJSON(t, h, "GET", "/subscriptions/s1", nil); st != 200 || asMap(t, s)["currentPlanId"] != "a" {
		t.Fatalf("plan changed despite cancel: %v", s)
	}
}

func TestAPINextPeriodAdvanceBeforeDue(t *testing.T) {
	h, _, _ := newTestServer(t, "2026-01-10T00:00:00Z")
	doJSON(t, h, "POST", "/plans", map[string]any{"id": "a", "name": "A", "currency": "CNY", "price": 1000})
	doJSON(t, h, "POST", "/plans", map[string]any{"id": "b", "name": "B", "currency": "CNY", "price": 2000})
	doJSON(t, h, "POST", "/subscriptions", map[string]any{"id": "s1", "planId": "a"})
	if st, _ := doJSON(t, h, "POST", "/subscriptions/s1/changes",
		map[string]any{"targetPlanId": "b", "effectiveType": "NEXT_PERIOD"}); st != 201 {
		t.Fatalf("apply st=%d", st)
	}
	if st, body := doJSON(t, h, "POST", "/subscriptions/s1/advance", nil); st != http.StatusBadRequest {
		t.Fatalf("early advance st=%d body=%v want 400", st, body)
	}
}

func TestAPIBadJSON(t *testing.T) {
	h, _, _ := newTestServer(t, "2026-01-10T00:00:00Z")
	req := httptest.NewRequest("POST", "/plans", bytes.NewReader([]byte("{not json")))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json st=%d want 400", rec.Code)
	}
}

func TestAPIAddPlanPriceFreeze(t *testing.T) {
	h, _, clk := newTestServer(t, "2026-01-11T00:00:00Z")
	doJSON(t, h, "POST", "/plans", map[string]any{"id": "basic", "name": "Basic", "currency": "CNY", "price": 1000})
	doJSON(t, h, "POST", "/plans", map[string]any{"id": "pro", "name": "Pro", "currency": "CNY", "price": 4100})
	doJSON(t, h, "POST", "/subscriptions", map[string]any{"id": "s1", "planId": "basic"})

	// 受理一笔下一周期变更，冻结 pro=4100 v1。
	st, chAny := doJSON(t, h, "POST", "/subscriptions/s1/changes",
		map[string]any{"targetPlanId": "pro", "effectiveType": "NEXT_PERIOD"})
	if st != 201 {
		t.Fatalf("apply st=%d", st)
	}
	changeID := asMap(t, chAny)["id"].(string)
	// 之后 pro 涨价，新价格生效时间早于边界。
	if st, _ := doJSON(t, h, "POST", "/plans/pro/prices",
		map[string]any{"amount": 9999, "effectiveAt": "2026-01-20T00:00:00Z"}); st != http.StatusCreated {
		t.Fatalf("add price st=%d", st)
	}
	// 差额为正：生效前确认支付（按冻结价 4100）。
	if st, body := doJSON(t, h, "POST", "/changes/"+changeID+"/payments",
		map[string]any{"clientToken": "p1", "amount": 4100, "result": "SUCCEEDED"}); st != http.StatusCreated {
		t.Fatalf("pay st=%d body=%v", st, body)
	}
	clk.Set(mustDate(t, "2026-02-11T00:00:00Z"))
	st, resAny := doJSON(t, h, "POST", "/subscriptions/s1/advance", nil)
	if st != 200 {
		t.Fatalf("advance st=%d", st)
	}
	res := asMap(t, resAny)
	bill := asMap(t, res["bill"])
	if int64(bill["amount"].(float64)) != 4100 {
		t.Fatalf("bill amount=%v want frozen 4100", bill["amount"])
	}
}

func TestAPIScheduledChangeWithPayment(t *testing.T) {
	h, _, clk := newTestServer(t, "2026-01-10T00:00:00Z")
	doJSON(t, h, "POST", "/plans", map[string]any{"id": "a", "name": "A", "currency": "CNY", "price": 1000})
	doJSON(t, h, "POST", "/plans", map[string]any{"id": "b", "name": "B", "currency": "CNY", "price": 2000})
	doJSON(t, h, "POST", "/subscriptions", map[string]any{"id": "s1", "planId": "a"})
	doJSON(t, h, "POST", "/subscriptions", map[string]any{"id": "s2", "planId": "a"})

	// 预约 1/20 生效：周期 1/10~2/10 剩 21 天，差额 (2000-1000)*21/31 = 677。
	st, chAny := doJSON(t, h, "POST", "/subscriptions/s1/changes",
		map[string]any{"clientToken": "sch-1", "targetPlanId": "b", "effectiveType": "SCHEDULED",
			"effectiveAt": "2026-01-20T00:00:00Z"})
	ch := asMap(t, chAny)
	if st != http.StatusCreated || ch["effectiveAt"] != "2026-01-20T00:00:00Z" {
		t.Fatalf("apply scheduled st=%d body=%v", st, chAny)
	}
	if int64(ch["settlementAmount"].(float64)) != 677 {
		t.Fatalf("settlement = %v want 677", ch["settlementAmount"])
	}
	changeID := ch["id"].(string)

	// 生效时间缺失 -> 400。
	if st, _ := doJSON(t, h, "POST", "/subscriptions/s2/changes",
		map[string]any{"targetPlanId": "b", "effectiveType": "SCHEDULED"}); st != http.StatusBadRequest {
		t.Fatalf("missing effectiveAt st=%d want 400", st)
	}
	// 到期前推进 -> 400。
	if st, _ := doJSON(t, h, "POST", "/subscriptions/s1/advance", nil); st != http.StatusBadRequest {
		t.Fatalf("early advance st=%d want 400", st)
	}
	// 支付金额不符 -> 409。
	if st, _ := doJSON(t, h, "POST", "/changes/"+changeID+"/payments",
		map[string]any{"clientToken": "p1", "amount": 1, "result": "SUCCEEDED"}); st != http.StatusConflict {
		t.Fatalf("wrong amount st=%d want 409", st)
	}
	// 确认支付 -> 201；同号重试 -> 201 同一支付。
	st, pAny := doJSON(t, h, "POST", "/changes/"+changeID+"/payments",
		map[string]any{"clientToken": "p1", "amount": 677, "result": "SUCCEEDED"})
	if st != http.StatusCreated {
		t.Fatalf("pay st=%d body=%v", st, pAny)
	}
	payID := asMap(t, pAny)["id"].(string)
	st, p2Any := doJSON(t, h, "POST", "/changes/"+changeID+"/payments",
		map[string]any{"clientToken": "p1", "amount": 677, "result": "SUCCEEDED"})
	if st != http.StatusCreated || asMap(t, p2Any)["id"] != payID {
		t.Fatalf("idempotent pay retry st=%d", st)
	}
	// 换号重复确认 -> 409 ALREADY_PAID。
	if st, body := doJSON(t, h, "POST", "/changes/"+changeID+"/payments",
		map[string]any{"clientToken": "p2", "amount": 677, "result": "SUCCEEDED"}); st != http.StatusConflict {
		t.Fatalf("duplicate pay st=%d want 409 body=%v", st, body)
	}
	// 支付记录可查。
	if st, ps := doJSON(t, h, "GET", "/changes/"+changeID+"/payments", nil); st != 200 || len(asSlice(t, ps)) != 1 {
		t.Fatalf("list payments st=%d", st)
	}
	// 到达生效时间推进：已支付则生效出账。
	clk.Set(mustDate(t, "2026-01-20T00:00:00Z"))
	st, resAny := doJSON(t, h, "POST", "/subscriptions/s1/advance", nil)
	res := asMap(t, resAny)
	if st != 200 || asMap(t, res["change"])["status"] != ChangeEffective {
		t.Fatalf("advance st=%d res=%v", st, resAny)
	}
	if int64(asMap(t, res["bill"])["amount"].(float64)) != 677 {
		t.Fatalf("bill = %v want 677", res["bill"])
	}

	// 另一订阅：到期未支付 -> 切换失败，保留原套餐并记录原因。
	st, ch2Any := doJSON(t, h, "POST", "/subscriptions/s2/changes",
		map[string]any{"targetPlanId": "b", "effectiveType": "SCHEDULED", "effectiveAt": "2026-01-25T00:00:00Z"})
	ch2ID := asMap(t, ch2Any)["id"].(string)
	if st != 201 {
		t.Fatalf("apply s2 st=%d", st)
	}
	clk.Set(mustDate(t, "2026-01-25T00:00:00Z"))
	st, res2Any := doJSON(t, h, "POST", "/subscriptions/s2/advance", nil)
	res2 := asMap(t, res2Any)
	ch2 := asMap(t, res2["change"])
	if st != 200 || ch2["status"] != ChangeFailed || ch2["failureReason"] == "" {
		t.Fatalf("unpaid advance st=%d res=%v", st, res2Any)
	}
	if res2["bill"] != nil {
		t.Fatalf("failed change must not bill: %v", res2["bill"])
	}
	if st, s := doJSON(t, h, "GET", "/subscriptions/s2", nil); st != 200 || asMap(t, s)["currentPlanId"] != "a" {
		t.Fatalf("original plan not kept: %v", s)
	}
	// 失败后迟到的支付 -> 409，不能激活已失败变更。
	if st, _ := doJSON(t, h, "POST", "/changes/"+ch2ID+"/payments",
		map[string]any{"amount": 677, "result": "SUCCEEDED"}); st != http.StatusConflict {
		t.Fatalf("late pay st=%d want 409", st)
	}
}
