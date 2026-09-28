package subscription

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Server 把 Service 暴露为 JSON HTTP API。
type Server struct {
	svc *Service
}

// NewServer 创建 HTTP Server。
func NewServer(svc *Service) *Server { return &Server{svc: svc} }

// Routes 注册路由（Go 1.22+ 方法模式）。
func (s *Server) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/plans", s.listPlans)
	mux.HandleFunc("POST /api/v1/plans", s.configurePlan)
	mux.HandleFunc("GET /api/v1/plans/{planID}", s.getPlan)
	mux.HandleFunc("POST /api/v1/plans/{planID}/prices", s.addPrice)

	mux.HandleFunc("POST /api/v1/subscriptions", s.createSub)
	mux.HandleFunc("GET /api/v1/subscriptions/{subID}", s.getSub)
	mux.HandleFunc("POST /api/v1/subscriptions/{subID}/terminate", s.terminate)
	mux.HandleFunc("GET /api/v1/subscriptions/{subID}/changes", s.listChanges)
	mux.HandleFunc("GET /api/v1/subscriptions/{subID}/bills", s.listBills)

	mux.HandleFunc("POST /api/v1/changes", s.applyChange)
	mux.HandleFunc("GET /api/v1/changes/{changeID}", s.getChange)
	mux.HandleFunc("PUT /api/v1/changes/{changeID}", s.replaceByID)
	mux.HandleFunc("DELETE /api/v1/changes/{changeID}", s.cancelByID)
	mux.HandleFunc("POST /api/v1/changes/{changeID}/advance", s.advance)

	mux.HandleFunc("GET /api/v1/bills/{billID}", s.getBill)
	return mux
}

// Handler 返回可直接挂载的 http.Handler。
func (s *Server) Handler() http.Handler { return s.Routes() }

// ---------------- 请求/响应结构 ----------------

type planRequest struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Cycle      string `json:"cycle"`
	PriceCents int64  `json:"price_cents"`
}

type priceRequest struct {
	PriceCents  int64  `json:"price_cents"`
	EffectiveAt string `json:"effective_at,omitempty"` // RFC3339，空=立即
}

type createSubRequest struct {
	ID     string `json:"id,omitempty"`
	PlanID string `json:"plan_id"`
}

type applyChangeRequest struct {
	SubscriptionID string `json:"subscription_id"`
	ExternalID     string `json:"external_id,omitempty"`
	ToPlanID       string `json:"to_plan_id"`
	EffectiveMode  string `json:"effective_mode"`
}

type replaceChangeRequest struct {
	ToPlanID        string `json:"to_plan_id"`
	EffectiveMode   string `json:"effective_mode"`
	ExpectedVersion *int   `json:"expected_version,omitempty"`
	ExternalID      string `json:"external_id,omitempty"`
}

type cancelChangeRequest struct {
	ExpectedVersion *int `json:"expected_version,omitempty"`
}

type advanceResponse struct {
	Advanced     bool           `json:"advanced"`
	Change       *ChangeRequest `json:"change"`
	Bill         *Bill          `json:"bill,omitempty"`
	Notification *Notification  `json:"notification,omitempty"`
}

type errorResponse struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	ecode := "internal"
	switch {
	case errors.Is(err, ErrInvalidArgument):
		code, ecode = http.StatusBadRequest, "invalid_argument"
	case errors.Is(err, ErrNotFound):
		code, ecode = http.StatusNotFound, "not_found"
	case errors.Is(err, ErrPlanMissing):
		code, ecode = http.StatusBadRequest, "plan_missing"
	case errors.Is(err, ErrAlreadyExists):
		code, ecode = http.StatusConflict, "already_exists"
	case errors.Is(err, ErrPendingChangeExists):
		code, ecode = http.StatusConflict, "pending_change_exists"
	case errors.Is(err, ErrVersionMismatch):
		code, ecode = http.StatusConflict, "version_mismatch"
	case errors.Is(err, ErrChangeNotDue):
		code, ecode = http.StatusConflict, "change_not_due"
	case errors.Is(err, ErrSubscriptionTerminated):
		code, ecode = http.StatusConflict, "subscription_terminated"
	case errors.Is(err, ErrConflict):
		code, ecode = http.StatusConflict, "conflict"
	}
	writeJSON(w, code, errorResponse{Error: err.Error(), Code: ecode})
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	return nil
}

// parseTimeOrNow 解析 RFC3339 时间；空串返回零值（由业务层填当前时间）。
func parseTimeOrNow(s string) (time.Time, error) {
	if strings.TrimSpace(s) == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339, s)
}

// ---------------- handlers ----------------

func (s *Server) listPlans(w http.ResponseWriter, r *http.Request) {
	plans, err := s.svc.ListPlans()
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, plans)
}

func (s *Server) configurePlan(w http.ResponseWriter, r *http.Request) {
	var req planRequest
	if err := decode(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid json: " + err.Error(), Code: "invalid_argument"})
		return
	}
	p, err := s.svc.ConfigurePlan(PlanConfigInput{ID: req.ID, Name: req.Name, Cycle: req.Cycle, PriceCents: req.PriceCents})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func (s *Server) getPlan(w http.ResponseWriter, r *http.Request) {
	p, err := s.svc.ListPlans()
	if err != nil {
		writeErr(w, err)
		return
	}
	id := r.PathValue("planID")
	for _, pl := range p {
		if pl.ID == id {
			writeJSON(w, http.StatusOK, pl)
			return
		}
	}
	writeErr(w, fmt.Errorf("%w: plan %s", ErrNotFound, id))
}

func (s *Server) addPrice(w http.ResponseWriter, r *http.Request) {
	var req priceRequest
	if err := decode(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid json: " + err.Error(), Code: "invalid_argument"})
		return
	}
	at, err := parseTimeOrNow(req.EffectiveAt)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid effective_at: " + err.Error(), Code: "invalid_argument"})
		return
	}
	v, err := s.svc.AddPlanPrice(r.PathValue("planID"), req.PriceCents, at)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, v)
}

func (s *Server) createSub(w http.ResponseWriter, r *http.Request) {
	var req createSubRequest
	if err := decode(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid json: " + err.Error(), Code: "invalid_argument"})
		return
	}
	sub, bill, err := s.svc.CreateSubscription(CreateSubscriptionInput{ID: req.ID, PlanID: req.PlanID})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"subscription": sub, "initial_bill": bill})
}

func (s *Server) getSub(w http.ResponseWriter, r *http.Request) {
	sub, err := s.svc.GetSubscription(r.PathValue("subID"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sub)
}

func (s *Server) terminate(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.Terminate(r.PathValue("subID")); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "terminated"})
}

func (s *Server) listChanges(w http.ResponseWriter, r *http.Request) {
	ch, err := s.svc.ListChanges(r.PathValue("subID"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ch)
}

func (s *Server) listBills(w http.ResponseWriter, r *http.Request) {
	b, err := s.svc.ListBills(r.PathValue("subID"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, b)
}

func (s *Server) applyChange(w http.ResponseWriter, r *http.Request) {
	var req applyChangeRequest
	if err := decode(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid json: " + err.Error(), Code: "invalid_argument"})
		return
	}
	ch, adv, err := s.svc.ApplyChange(ApplyChangeInput{
		SubscriptionID: req.SubscriptionID,
		ExternalID:     req.ExternalID,
		ToPlanID:       req.ToPlanID,
		EffectiveMode:  req.EffectiveMode,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	status := http.StatusCreated
	resp := map[string]any{"change": ch}
	if adv != nil {
		resp["settlement"] = advanceResponse{Advanced: adv.Advanced, Change: adv.Change, Bill: adv.Bill, Notification: adv.Notification}
		status = http.StatusCreated
	}
	writeJSON(w, status, resp)
}

func (s *Server) getChange(w http.ResponseWriter, r *http.Request) {
	ch, err := s.svc.GetChange(r.PathValue("changeID"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ch)
}

func (s *Server) replaceByID(w http.ResponseWriter, r *http.Request) {
	ch, err := s.svc.GetChange(r.PathValue("changeID"))
	if err != nil {
		writeErr(w, err)
		return
	}
	var req replaceChangeRequest
	if err := decode(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid json: " + err.Error(), Code: "invalid_argument"})
		return
	}
	out, adv, err := s.svc.ReplaceChange(ReplaceChangeInput{
		SubscriptionID:  ch.SubscriptionID,
		ChangeID:        ch.ID,
		ToPlanID:        req.ToPlanID,
		EffectiveMode:   req.EffectiveMode,
		ExpectedVersion: req.ExpectedVersion,
		ExternalID:      req.ExternalID,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	resp := map[string]any{"change": out}
	if adv != nil {
		resp["settlement"] = advanceResponse{Advanced: adv.Advanced, Change: adv.Change, Bill: adv.Bill, Notification: adv.Notification}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) cancelByID(w http.ResponseWriter, r *http.Request) {
	ch, err := s.svc.GetChange(r.PathValue("changeID"))
	if err != nil {
		writeErr(w, err)
		return
	}
	var req cancelChangeRequest
	if r.ContentLength > 0 {
		if err := decode(r, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid json: " + err.Error(), Code: "invalid_argument"})
			return
		}
	}
	if err := s.svc.CancelChange(CancelChangeInput{SubscriptionID: ch.SubscriptionID, ChangeID: ch.ID, ExpectedVersion: req.ExpectedVersion}); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "cancelled", "change_id": ch.ID})
}

func (s *Server) advance(w http.ResponseWriter, r *http.Request) {
	adv, err := s.svc.AdvanceChange(r.PathValue("changeID"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, advanceResponse{
		Advanced:     adv.Advanced,
		Change:       adv.Change,
		Bill:         adv.Bill,
		Notification: adv.Notification,
	})
}

func (s *Server) getBill(w http.ResponseWriter, r *http.Request) {
	b, err := s.svc.GetBill(r.PathValue("billID"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, b)
}
