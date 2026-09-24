package api

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"

	"github.com/gogf/gf/v2/net/ghttp"

	"oncall-agent/internal/metrics"
	"oncall-agent/internal/store"
)

// approvalService 是审批 API 对审批层的收窄接口。
type approvalService interface {
	Decide(ctx context.Context, id uint64, approve bool, expectedPlanHash, decidedBy, decisionReason, decisionSource string) (store.Approval, error)
	List(ctx context.Context, status string) ([]store.Approval, error)
	Get(ctx context.Context, id uint64) (store.Approval, error)
}

// ApprovalAPI exposes approval decisions and list/query operations. Automation
// authenticates with the Bearer token; the browser reaches the same handlers
// through the public console, which requires no login and records a fixed
// anonymous operator on every decision.
type ApprovalAPI struct {
	svc       approvalService
	authToken string
	console   *Console
}

func NewApprovalAPI(svc approvalService, authToken string, console *Console) *ApprovalAPI {
	return &ApprovalAPI{svc: svc, authToken: authToken, console: console}
}

func (h *ApprovalAPI) Handle(r *ghttp.Request) {
	h.ServeHTTP(r.Response.BufferWriter, r.Request)
}

func (h *ApprovalAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.svc == nil || r == nil {
		writeError(w, http.StatusServiceUnavailable, "approval service unavailable")
		return
	}
	web, ok := h.authenticate(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/approvals"), "/")
	if rest == "" {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		approvals, err := h.svc.List(r.Context(), strings.TrimSpace(r.URL.Query().Get("status")))
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "list approvals failed")
			return
		}
		values := make([]ApprovalDTO, 0, len(approvals))
		for _, approval := range approvals {
			values = append(values, approvalDTO(approval))
		}
		writeJSON(w, http.StatusOK, map[string]any{"approvals": values})
		return
	}
	parts := strings.Split(rest, "/")
	if len(parts) == 1 {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		id, err := parseIncidentID(parts[0])
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid approval id")
			return
		}
		approval, err := h.svc.Get(r.Context(), id)
		if err != nil {
			if errors.Is(err, store.ErrApprovalNotFound) {
				writeError(w, http.StatusNotFound, "approval not found")
			} else {
				writeError(w, http.StatusServiceUnavailable, "get approval failed")
			}
			return
		}
		writeJSON(w, http.StatusOK, approvalDTO(approval))
		return
	}
	if len(parts) != 2 || (parts[1] != "approve" && parts[1] != "deny") {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id, err := parseIncidentID(parts[0])
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid approval id")
		return
	}
	operator := strings.TrimSpace(r.Header.Get("X-Operator"))
	source := "api"
	if web {
		actor, _ := h.console.Actor()
		operator = actor.ID
		source = "web"
	} else if operator == "" {
		writeError(w, http.StatusBadRequest, "X-Operator header is required")
		return
	}
	var input struct {
		PlanHash string  `json:"plan_hash"`
		Reason   *string `json:"reason"`
	}
	if err := decodeBoundedJSON(r, &input, 64<<10); err != nil || strings.TrimSpace(input.PlanHash) == "" || input.Reason == nil {
		writeError(w, http.StatusBadRequest, "plan_hash and reason are required")
		return
	}
	h.decide(w, r, id, parts[1] == "approve", strings.TrimSpace(input.PlanHash), operator, *input.Reason, source)
}

// authenticate reports whether the caller is allowed and whether the request
// arrived through the browser console rather than Bearer automation. The two
// channels differ only in the operator recorded on a decision.
func (h *ApprovalAPI) authenticate(r *http.Request) (web bool, ok bool) {
	if r != nil && subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+h.authToken)) == 1 {
		return false, true
	}
	if _, enabled := h.console.Actor(); !enabled {
		return false, false
	}
	return true, true
}

func (h *ApprovalAPI) decide(w http.ResponseWriter, r *http.Request, id uint64, approve bool, planHash, operator, reason, source string) {
	approval, err := h.svc.Decide(r.Context(), id, approve, planHash, operator, reason, source)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, approvalDTO(approval))
		metrics.Inc(map[bool]string{true: metrics.ApprovalApproved, false: metrics.ApprovalDenied}[approve])
	case errors.Is(err, store.ErrApprovalNotFound):
		writeError(w, http.StatusNotFound, "approval not found")
	case errors.Is(err, store.ErrApprovalConflict):
		writeError(w, http.StatusConflict, "approval changed, already decided or expired")
	default:
		writeError(w, http.StatusServiceUnavailable, "decide approval failed")
	}
}
