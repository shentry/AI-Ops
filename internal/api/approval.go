package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/gogf/gf/v2/net/ghttp"

	"oncall-agent/internal/auth"
	"oncall-agent/internal/metrics"
	"oncall-agent/internal/store"
)

// approvalService 是审批 API 对审批层的收窄接口。
type approvalService interface {
	Decide(ctx context.Context, id uint64, approve bool, decidedBy, decisionReason, decisionSource string) (store.Approval, error)
	List(ctx context.Context, status string) ([]store.Approval, error)
	Get(ctx context.Context, id uint64) (store.Approval, error)
}

// ApprovalAPI exposes approval decisions and list/query operations. Bearer
// automation remains supported; the Web route assembly may deliberately pass
// AnonymousConsoleAuthenticator so browser decisions require no login or CSRF.
type ApprovalAPI struct {
	svc       approvalService
	authToken string
	session   SessionAuthenticator
}

func NewApprovalAPI(svc approvalService, authToken string, sessions ...SessionAuthenticator) *ApprovalAPI {
	var session SessionAuthenticator
	if len(sessions) > 0 {
		session = sessions[0]
	}
	return &ApprovalAPI{svc: svc, authToken: authToken, session: session}
}

// NewApprovalAPIWithSession is the explicit constructor used by Web assembly.
func NewApprovalAPIWithSession(svc approvalService, authToken string, session SessionAuthenticator) *ApprovalAPI {
	return NewApprovalAPI(svc, authToken, session)
}

func (h *ApprovalAPI) Handle(r *ghttp.Request) {
	h.ServeHTTP(r.Response.BufferWriter, r.Request)
}

func (h *ApprovalAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.svc == nil || r == nil {
		writeError(w, http.StatusServiceUnavailable, "approval service unavailable")
		return
	}
	_, web, status, message := h.authenticate(r)
	if status != 0 {
		writeError(w, status, message)
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
		actor, _, writeStatus, writeMessage := requireWriteSession(h.session, r)
		if writeStatus != 0 {
			writeError(w, writeStatus, writeMessage)
			return
		}
		operator = strings.TrimSpace(actor.ID)
		source = "web"
	} else if operator == "" {
		writeError(w, http.StatusBadRequest, "X-Operator header is required")
		return
	}
	reason, err := decodeDecisionReason(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid decision reason")
		return
	}
	h.decide(w, r, id, parts[1] == "approve", operator, reason, source, web)
}

func (h *ApprovalAPI) authenticate(r *http.Request) (auth.Actor, bool, int, string) {
	if r != nil && subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+h.authToken)) == 1 {
		return auth.Actor{}, false, 0, ""
	}
	if h.session == nil {
		return auth.Actor{}, false, http.StatusUnauthorized, "unauthorized"
	}
	actor, _, err := h.session.Authenticate(r.Context(), r)
	if err != nil {
		if errors.Is(err, auth.ErrUnauthenticated) || errors.Is(err, auth.ErrSessionUnavailable) {
			return auth.Actor{}, false, http.StatusUnauthorized, "unauthorized"
		}
		return auth.Actor{}, false, http.StatusServiceUnavailable, "session lookup failed"
	}
	return actor, true, 0, ""
}

func (h *ApprovalAPI) decide(w http.ResponseWriter, r *http.Request, id uint64, approve bool, operator, reason, source string, web bool) {
	approval, err := h.svc.Decide(r.Context(), id, approve, operator, reason, source)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, approvalDTO(approval))
		metrics.Inc(map[bool]string{true: metrics.ApprovalApproved, false: metrics.ApprovalDenied}[approve])
	case errors.Is(err, store.ErrApprovalNotFound):
		writeError(w, http.StatusNotFound, "approval not found")
	case errors.Is(err, store.ErrApprovalConflict):
		writeError(w, http.StatusConflict, "approval already decided or expired")
	default:
		writeError(w, http.StatusServiceUnavailable, "decide approval failed")
	}
}

func decodeDecisionReason(r *http.Request) (string, error) {
	if r.Body == nil {
		return "", nil
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		return "", err
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return "", nil
	}
	var input struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(body, &input); err != nil {
		return "", err
	}
	return input.Reason, nil
}
