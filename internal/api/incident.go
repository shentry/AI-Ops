package api

import (
	"context"
	"crypto/subtle"
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
	CreateAgentRun(ctx context.Context, run store.AgentRun) (store.AgentRun, error)
}

// IncidentAPI 暴露 D05 的三个接口：列表、详情（含成员）、手动重诊。
// 只读接口绝不改变告警或执行状态；唯一写路径是 diagnose 落 pending run。
type IncidentAPI struct {
	db            incidentStore
	authToken     string
	severityRoute map[string]string
}

func NewIncidentAPI(db incidentStore, authToken string, severityRoute map[string]string) *IncidentAPI {
	return &IncidentAPI{db: db, authToken: authToken, severityRoute: severityRoute}
}

// Handle 适配 GoFrame 路由。路径参数由 GoFrame 解析后仍在 URL.Path 里，
// 所以直接复用 stdlib 的 ServeHTTP，测试不用起 GoFrame。
func (h *IncidentAPI) Handle(r *ghttp.Request) {
	h.ServeHTTP(r.Response.BufferWriter, r.Request)
}

func (h *IncidentAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+h.authToken)) != 1 {
		writeError(w, http.StatusUnauthorized, "unauthorized")
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

// diagnoseIncident 是手动重诊兜底：severity 升级或 run 失败后由人触发。
// 无论路由结果如何都落 pending —— 人显式要求诊断，skip 只约束自动分流；
// 路由为 skip 时降为 light，用最小预算诊断而不是花 full 的预算。
// retry_of 为空：它是新的一次诊断，不是某个失败 run 的自动重试（D12 才用 retry_of）。
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
	run, err := h.db.CreateAgentRun(r.Context(), store.AgentRun{
		IncidentID: id,
		Mode:       mode,
		Status:     "pending",
		StartedAt:  time.Now().UTC(),
	})
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "create agent run failed")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"run": run})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": message})
}
