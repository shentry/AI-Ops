package api

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gogf/gf/v2/net/ghttp"

	"oncall-agent/internal/metrics"
	"oncall-agent/internal/store"
)

// approvalService 是审批 API 对审批层的收窄接口。
type approvalService interface {
	Decide(ctx context.Context, id uint64, approve bool, decidedBy string) error
	List(ctx context.Context, status string) ([]store.Approval, error)
}

// ApprovalAPI 暴露审批操作：approve/deny 幂等决策 + 列表查询。
// 所有端点 Bearer 鉴权，decided_by 取请求头 X-Operator（空则拒绝 ——
// 审批必须能回答"谁批的"）。
type ApprovalAPI struct {
	svc       approvalService
	authToken string
}

func NewApprovalAPI(svc approvalService, authToken string) *ApprovalAPI {
	return &ApprovalAPI{svc: svc, authToken: authToken}
}

func (h *ApprovalAPI) Handle(r *ghttp.Request) {
	h.ServeHTTP(r.Response.BufferWriter, r.Request)
}

func (h *ApprovalAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+h.authToken)) != 1 {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/approvals"), "/")
	switch {
	case rest == "" && r.Method == http.MethodGet:
		approvals, err := h.svc.List(r.Context(), strings.TrimSpace(r.URL.Query().Get("status")))
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "list approvals failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"approvals": approvals})
	case strings.HasSuffix(rest, "/approve") || strings.HasSuffix(rest, "/deny"):
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		parts := strings.Split(rest, "/")
		id, err := parseIncidentID(parts[0])
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid approval id")
			return
		}
		operator := strings.TrimSpace(r.Header.Get("X-Operator"))
		if operator == "" {
			writeError(w, http.StatusBadRequest, "X-Operator header is required")
			return
		}
		h.decide(w, r, id, strings.HasSuffix(rest, "/approve"), operator)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (h *ApprovalAPI) decide(w http.ResponseWriter, r *http.Request, id uint64, approve bool, operator string) {
	err := h.svc.Decide(r.Context(), id, approve, operator)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "status": map[bool]string{true: "approved", false: "denied"}[approve], "decided_by": operator, "decided_at": time.Now().UTC()})
		metrics.Inc(map[bool]string{true: metrics.ApprovalApproved, false: metrics.ApprovalDenied}[approve])
	case errors.Is(err, store.ErrApprovalNotFound):
		writeError(w, http.StatusNotFound, "approval not found")
	case errors.Is(err, store.ErrApprovalConflict):
		writeError(w, http.StatusConflict, "approval already decided or expired")
	default:
		writeError(w, http.StatusServiceUnavailable, "decide approval failed")
	}
}
