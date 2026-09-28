package api

import (
	"context"
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

// ApprovalAPI exposes approval decisions and list/query operations. Reads accept
// any identity including automation; a decision needs an operator whose identity
// the server proved (session or personal token), and records that identity.
type ApprovalAPI struct {
	svc  approvalService
	auth *Auth
}

func NewApprovalAPI(svc approvalService, auth *Auth) *ApprovalAPI {
	return &ApprovalAPI{svc: svc, auth: auth}
}

func (h *ApprovalAPI) Handle(r *ghttp.Request) {
	h.ServeHTTP(r.Response.BufferWriter, r.Request)
}

func (h *ApprovalAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.svc == nil || r == nil {
		writeError(w, http.StatusServiceUnavailable, "approval service unavailable")
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/approvals"), "/")
	if r.Method == http.MethodGet {
		if _, ok := h.auth.Require(w, r, RoleViewer, true); !ok {
			return
		}
	}
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
	actor, ok := h.auth.Require(w, r, RoleOperator, false)
	if !ok {
		return
	}
	id, err := parseIncidentID(parts[0])
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid approval id")
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
	h.decide(w, r, id, parts[1] == "approve", strings.TrimSpace(input.PlanHash), actor.ID, *input.Reason, actor.Source)
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
