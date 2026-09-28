package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/gogf/gf/v2/net/ghttp"

	"oncall-agent/internal/diagnose"
	"oncall-agent/internal/store"
)

// evidenceBuilder 是调试端点对诊断包的收窄接口。
type evidenceBuilder interface {
	BuildForIncident(ctx context.Context, incidentID uint64) (diagnose.Evidence, error)
}

// EvidenceDebugAPI 暴露 D07 的临时调试端点 GET /debug/evidence/{id}：
// 直接在响应里看渲染后的证据文本，验证采集链路，不进 prompt。
// Live evidence collection is a sensitive read: admins and automation only.
type EvidenceDebugAPI struct {
	builder evidenceBuilder
	auth    *Auth
}

func NewEvidenceDebugAPI(builder evidenceBuilder, auth *Auth) *EvidenceDebugAPI {
	return &EvidenceDebugAPI{builder: builder, auth: auth}
}

func (h *EvidenceDebugAPI) Handle(r *ghttp.Request) {
	h.ServeHTTP(r.Response.BufferWriter, r.Request)
}

func (h *EvidenceDebugAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if _, ok := h.auth.Require(w, r, RoleAdmin, true); !ok {
		return
	}
	id, err := parseIncidentID(strings.TrimPrefix(r.URL.Path, "/debug/evidence/"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid incident id")
		return
	}
	evidence, err := h.builder.BuildForIncident(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrIncidentNotFound) {
			writeError(w, http.StatusNotFound, "incident not found")
		} else {
			writeError(w, http.StatusServiceUnavailable, "build evidence failed")
		}
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(evidence.Render()))
}
