package gosubscriptionchange

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// API 是基于标准库 net/http 的 JSON HTTP 层，全部能力对外暴露：
//
//	POST   /plans                         创建套餐（含首期价格）
//	GET    /plans                         套餐列表
//	GET    /plans/{planID}                套餐详情（含价格版本）
//	POST   /plans/{planID}/prices         追加新价格版本
//	POST   /subscriptions                 创建订阅
//	GET    /subscriptions/{subID}         订阅详情
//	POST   /subscriptions/{subID}/changes 提交变更申请
//	PUT    /subscriptions/{subID}/changes 用当前版本替换待生效变更
//	DELETE /subscriptions/{subID}/changes 取消待生效变更
//	POST   /subscriptions/{subID}/advance 到期推进（原子生效+结算+通知）
//	POST   /subscriptions/{subID}/terminate 终止订阅
//	GET    /subscriptions/{subID}/bills   账单查询
//	GET    /subscriptions/{subID}/history 变更历史
//	GET    /changes/{changeID}            变更申请详情
//	POST   /changes/{changeID}/payments   提交支付结果（幂等，可重试）
//	GET    /changes/{changeID}/payments   支付记录查询
type API struct {
	svc   *Service
	clock Clock
}

// NewAPI 创建 HTTP 处理器，使用系统时钟。
func NewAPI(svc *Service) http.Handler {
	return NewAPIWithClock(svc, SystemClock{})
}

// NewAPIWithClock 创建使用指定时钟的 HTTP 处理器（测试可注入 FakeClock）。
func NewAPIWithClock(svc *Service, clock Clock) http.Handler {
	a := &API{svc: svc, clock: clock}
	mux := http.NewServeMux()

	mux.HandleFunc("POST /plans", a.createPlan)
	mux.HandleFunc("GET /plans", a.listPlans)
	mux.HandleFunc("GET /plans/{planID}", a.getPlan)
	mux.HandleFunc("POST /plans/{planID}/prices", a.addPlanPrice)

	mux.HandleFunc("POST /subscriptions", a.createSubscription)
	mux.HandleFunc("GET /subscriptions/{subID}", a.getSubscription)
	mux.HandleFunc("POST /subscriptions/{subID}/changes", a.applyChange)
	mux.HandleFunc("PUT /subscriptions/{subID}/changes", a.replaceChange)
	mux.HandleFunc("DELETE /subscriptions/{subID}/changes", a.cancelChange)
	mux.HandleFunc("POST /subscriptions/{subID}/advance", a.advance)
	mux.HandleFunc("POST /subscriptions/{subID}/terminate", a.terminate)
	mux.HandleFunc("GET /subscriptions/{subID}/bills", a.listBills)
	mux.HandleFunc("GET /subscriptions/{subID}/history", a.listHistory)

	mux.HandleFunc("GET /changes/{changeID}", a.getChange)
	mux.HandleFunc("POST /changes/{changeID}/payments", a.submitPayment)
	mux.HandleFunc("GET /changes/{changeID}/payments", a.listPayments)

	return a.wrap(mux)
}

// wrap 注入通用中间件（panic 恢复、JSON 内容类型、时钟到请求 context）。
func (a *API) wrap(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				writeError(w, http.StatusInternalServerError, "INTERNAL", "internal server error")
			}
		}()
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		h.ServeHTTP(w, r.WithContext(withClock(r.Context(), a.clock)))
	})
}

// ---------------- 请求/响应结构 ----------------

type createPlanRequest struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Currency    string `json:"currency"`
	Price       int64  `json:"price"`
	EffectiveAt string `json:"effectiveAt,omitempty"` // RFC3339，可空
}

type addPriceRequest struct {
	Amount      int64  `json:"amount"`
	EffectiveAt string `json:"effectiveAt"` // RFC3339
}

type createSubscriptionRequest struct {
	ID     string `json:"id"`
	PlanID string `json:"planId"`
}

type changeRequest struct {
	ClientToken      string `json:"clientToken"`
	TargetPlanID     string `json:"targetPlanId"`
	EffectiveType    string `json:"effectiveType"`
	EffectiveAt      string `json:"effectiveAt,omitempty"`    // RFC3339，仅 SCHEDULED
	ExpectedAmount   *int64 `json:"expectedAmount,omitempty"` // 期望冻结金额（价格乐观校验）
	ExpectedChangeID string `json:"expectedChangeId,omitempty"`
}

type paymentRequest struct {
	ClientToken   string `json:"clientToken"`
	Amount        int64  `json:"amount"`
	Result        string `json:"result"` // SUCCEEDED / FAILED
	FailureReason string `json:"failureReason,omitempty"`
}

type errorResponse struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// ---------------- 工具 ----------------

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	var e errorResponse
	e.Error.Code = code
	e.Error.Message = msg
	writeJSON(w, status, e)
}

func parseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339, strings.TrimSpace(s))
}

// mapError 把领域错误映射为确定的 HTTP 状态码与错误码。
func mapError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrInvalidArgument):
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusNotFound, "NOT_FOUND", err.Error())
	case errors.Is(err, ErrAlreadyExists):
		writeError(w, http.StatusConflict, "ALREADY_EXISTS", err.Error())
	case errors.Is(err, ErrPendingChangeExist):
		writeError(w, http.StatusConflict, "PENDING_CHANGE_EXISTS", err.Error())
	case errors.Is(err, ErrNotPending):
		writeError(w, http.StatusConflict, "NOT_PENDING", err.Error())
	case errors.Is(err, ErrSubscriptionEnded):
		writeError(w, http.StatusConflict, "SUBSCRIPTION_ENDED", err.Error())
	case errors.Is(err, ErrSamePlan):
		writeError(w, http.StatusConflict, "SAME_PLAN", err.Error())
	case errors.Is(err, ErrAlreadyPaid):
		writeError(w, http.StatusConflict, "ALREADY_PAID", err.Error())
	case errors.Is(err, ErrConflict):
		writeError(w, http.StatusConflict, "CONFLICT", err.Error())
	case errors.Is(err, ErrConcurrentUpdate):
		writeError(w, http.StatusConflict, "CONCURRENT_UPDATE", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "INTERNAL", err.Error())
	}
}

// ---------------- handlers ----------------

func (a *API) createPlan(w http.ResponseWriter, r *http.Request) {
	var req createPlanRequest
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "malformed JSON body")
		return
	}
	eff, err := parseTime(req.EffectiveAt)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "effectiveAt must be RFC3339")
		return
	}
	p, err := a.svc.CreatePlan(r.Context(), CreatePlanInput{
		ID: req.ID, Name: req.Name, Currency: req.Currency,
		Price: req.Price, EffectiveAt: eff,
	})
	if err != nil {
		mapError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func (a *API) listPlans(w http.ResponseWriter, r *http.Request) {
	ps, err := a.svc.ListPlans(r.Context())
	if err != nil {
		mapError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ps)
}

func (a *API) getPlan(w http.ResponseWriter, r *http.Request) {
	p, err := a.svc.GetPlan(r.Context(), r.PathValue("planID"))
	if err != nil {
		mapError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (a *API) addPlanPrice(w http.ResponseWriter, r *http.Request) {
	var req addPriceRequest
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "malformed JSON body")
		return
	}
	eff, err := parseTime(req.EffectiveAt)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "effectiveAt must be RFC3339")
		return
	}
	pv, err := a.svc.AddPlanPrice(r.Context(), r.PathValue("planID"), req.Amount, eff)
	if err != nil {
		mapError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, pv)
}

func (a *API) createSubscription(w http.ResponseWriter, r *http.Request) {
	var req createSubscriptionRequest
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "malformed JSON body")
		return
	}
	sub, err := a.svc.CreateSubscription(r.Context(), CreateSubscriptionInput{ID: req.ID, PlanID: req.PlanID})
	if err != nil {
		mapError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, sub)
}

func (a *API) getSubscription(w http.ResponseWriter, r *http.Request) {
	sub, err := a.svc.GetSubscription(r.Context(), r.PathValue("subID"))
	if err != nil {
		mapError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sub)
}

func (a *API) changeInput(r *http.Request) (ChangeInput, error) {
	var req changeRequest
	if err := decode(r, &req); err != nil {
		return ChangeInput{}, err
	}
	effAt, err := parseTime(req.EffectiveAt)
	if err != nil {
		return ChangeInput{}, err
	}
	return ChangeInput{
		SubscriptionID:   r.PathValue("subID"),
		ClientToken:      strings.TrimSpace(req.ClientToken),
		TargetPlanID:     req.TargetPlanID,
		EffectiveType:    req.EffectiveType,
		EffectiveAt:      effAt,
		ExpectedAmount:   req.ExpectedAmount,
		ExpectedChangeID: req.ExpectedChangeID,
	}, nil
}

func (a *API) applyChange(w http.ResponseWriter, r *http.Request) {
	in, err := a.changeInput(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "malformed JSON body")
		return
	}
	ch, err := a.svc.ApplyChange(r.Context(), in)
	if err != nil {
		mapError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, ch)
}

func (a *API) replaceChange(w http.ResponseWriter, r *http.Request) {
	in, err := a.changeInput(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "malformed JSON body")
		return
	}
	ch, err := a.svc.ReplaceChange(r.Context(), in)
	if err != nil {
		mapError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ch)
}

func (a *API) cancelChange(w http.ResponseWriter, r *http.Request) {
	ch, err := a.svc.CancelChange(r.Context(), r.PathValue("subID"))
	if err != nil {
		mapError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ch)
}

func (a *API) advance(w http.ResponseWriter, r *http.Request) {
	res, err := a.svc.AdvanceSubscription(r.Context(), r.PathValue("subID"))
	if err != nil {
		mapError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (a *API) terminate(w http.ResponseWriter, r *http.Request) {
	sub, err := a.svc.TerminateSubscription(r.Context(), r.PathValue("subID"))
	if err != nil {
		mapError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sub)
}

func (a *API) listBills(w http.ResponseWriter, r *http.Request) {
	bills, err := a.svc.ListBills(r.Context(), r.PathValue("subID"))
	if err != nil {
		mapError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, bills)
}

func (a *API) listHistory(w http.ResponseWriter, r *http.Request) {
	events, err := a.svc.ListHistory(r.Context(), r.PathValue("subID"))
	if err != nil {
		mapError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, events)
}

func (a *API) getChange(w http.ResponseWriter, r *http.Request) {
	ch, err := a.svc.GetChange(r.Context(), r.PathValue("changeID"))
	if err != nil {
		mapError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ch)
}

func (a *API) submitPayment(w http.ResponseWriter, r *http.Request) {
	var req paymentRequest
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "malformed JSON body")
		return
	}
	p, err := a.svc.SubmitPayment(r.Context(), r.PathValue("changeID"), PaymentInput{
		ClientToken:   strings.TrimSpace(req.ClientToken),
		Amount:        req.Amount,
		Result:        req.Result,
		FailureReason: req.FailureReason,
	})
	if err != nil {
		mapError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func (a *API) listPayments(w http.ResponseWriter, r *http.Request) {
	ps, err := a.svc.ListPayments(r.Context(), r.PathValue("changeID"))
	if err != nil {
		mapError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ps)
}
