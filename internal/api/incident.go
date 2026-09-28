package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gogf/gf/v2/net/ghttp"

	"oncall-agent/internal/incident"
	"oncall-agent/internal/store"
)

// incidentStore 是查询 API 对存储层的收窄接口：只读查询 + 手动重诊落 run。
// *store.DB 天然满足，单测换假实现即可。
type incidentStore interface {
	ListIncidents(ctx context.Context, status string) ([]store.Incident, error)
	GetIncident(ctx context.Context, id uint64) (store.Incident, error)
	ListIncidentMembers(ctx context.Context, incidentID uint64) ([]store.IncidentMember, error)
	RequestRun(ctx context.Context, request store.RunRequest) (store.AgentRun, bool, error)
}

// IncidentAPI 暴露 D05 的三个接口：列表、详情（含成员）、手动重诊。
// 只读接口绝不改变告警或执行状态；唯一写路径是 diagnose 落 pending run。
type IncidentAPI struct {
	db            incidentStore
	auth          *Auth
	severityRoute map[string]string
}

func NewIncidentAPI(db incidentStore, auth *Auth, severityRoute map[string]string) *IncidentAPI {
	return &IncidentAPI{db: db, auth: auth, severityRoute: severityRoute}
}

// Handle 适配 GoFrame 路由。路径参数由 GoFrame 解析后仍在 URL.Path 里，
// 所以直接复用 stdlib 的 ServeHTTP，测试不用起 GoFrame。
func (h *IncidentAPI) Handle(r *ghttp.Request) {
	h.ServeHTTP(r.Response.BufferWriter, r.Request)
}

// Reads accept any identity; queuing a diagnosis is an operator action that the
// automation token may also trigger (it only enqueues read-only work).
func (h *IncidentAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	role := RoleViewer
	if r.Method != http.MethodGet {
		role = RoleOperator
	}
	if _, ok := h.auth.Require(w, r, role, true); !ok {
		return
	}
	// 三段式路径：/api/v1/incidents[/{id}[/diagnose]]
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/incidents")
	rest = strings.Trim(rest, "/")
	switch {
	case rest == "" && r.Method == http.MethodGet:
		h.listIncidents(w, r)
	case strings.HasSuffix(rest, "/diagnose") && r.Method == http.MethodPost:
		id, err := parseIncidentID(strings.TrimSuffix(rest, "/diagnose"))
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid incident id")
			return
		}
		h.diagnoseIncident(w, r, id)
	case !strings.Contains(rest, "/") && r.Method == http.MethodGet:
		id, err := parseIncidentID(rest)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid incident id")
			return
		}
		h.getIncident(w, r, id)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func parseIncidentID(segment string) (uint64, error) {
	id, err := strconv.ParseUint(strings.TrimSpace(segment), 10, 64)
	if err != nil || id == 0 {
		return 0, errors.New("invalid incident id")
	}
	return id, nil
}

// listIncidents 支持 ?status= 精确过滤；空参数返回全部，按 id 倒序（store 保证稳定）。
func (h *IncidentAPI) listIncidents(w http.ResponseWriter, r *http.Request) {
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	incidents, err := h.db.ListIncidents(r.Context(), status)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "list incidents failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"incidents": incidents})
}

// getIncident 返回 incident 本体和成员视图（成员当前状态来自 last_alert 快照）。
func (h *IncidentAPI) getIncident(w http.ResponseWriter, r *http.Request, id uint64) {
	found, err := h.db.GetIncident(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrIncidentNotFound) {
			writeError(w, http.StatusNotFound, "incident not found")
		} else {
			writeError(w, http.StatusServiceUnavailable, "get incident failed")
		}
		return
	}
	members, err := h.db.ListIncidentMembers(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "list incident members failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"incident": found, "members": members})
}

// diagnoseIncident uses the same atomic admission rules as Web rediagnosis.
// A manual request overrides skip with light mode but cannot bypass an active
// processing cycle, incident status or cooldown. It starts a new retry chain.
func (h *IncidentAPI) diagnoseIncident(w http.ResponseWriter, r *http.Request, id uint64) {
	found, err := h.db.GetIncident(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrIncidentNotFound) {
			writeError(w, http.StatusNotFound, "incident not found")
		} else {
			writeError(w, http.StatusServiceUnavailable, "get incident failed")
		}
		return
	}
	mode := incident.RouteMode(int(found.Severity), h.severityRoute)
	if mode == incident.ModeSkip {
		mode = incident.ModeLight
	}
	run, _, err := h.db.RequestRun(r.Context(), store.RunRequest{
		IncidentID:  id,
		Mode:        mode,
		Trigger:     store.RunTriggerManual,
		Reason:      "manual diagnosis requested via API",
		RequestedAt: time.Now().UTC(),
	})
	if err != nil {
		writeRunAdmissionError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"run": run})
}

// writeRunAdmissionError is shared by both manual diagnosis routes. Refusals
// remain structured; storage errors never expose database details to clients.
func writeRunAdmissionError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrIncidentNotFound) {
		writeError(w, http.StatusNotFound, "incident not found")
		return
	}
	var admission *store.RunAdmissionError
	if errors.As(err, &admission) {
		status := http.StatusConflict
		body := map[string]any{"error": admission.Code, "code": admission.Code}
		if admission.RunID != 0 {
			body["run_id"] = admission.RunID
		}
		if admission.ApprovalID != 0 {
			body["approval_id"] = admission.ApprovalID
		}
		if admission.Code == "cooldown" {
			status = http.StatusTooManyRequests
			w.Header().Set("Retry-After", strconv.Itoa(admission.RetryAfterSeconds))
			body["retry_after_seconds"] = admission.RetryAfterSeconds
		}
		writeJSON(w, status, body)
		return
	}
	writeError(w, http.StatusServiceUnavailable, "queue rediagnosis failed")
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": message})
}
