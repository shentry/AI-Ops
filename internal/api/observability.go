// Copied from ongridio/ongrid@81e08b5efbe9ccd9a5781574d5f3ba10215eccac,
// internal/manager/server/prometheus/http.go (queryRange) — Copyright the
// ongrid authors, licensed under AGPL-3.0 (see LICENSE and NOTICE).
//
// Modified 2026-09-28 for oncall-agent: ported from chi to net/http with this
// console's session auth and error shape; dashboards are served from the
// embedded catalog (internal/grafana) instead of being proxied from Grafana.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gogf/gf/v2/net/ghttp"

	"oncall-agent/internal/grafana"
)

// maxExprBytes caps the PromQL expression so an authenticated user can't
// pin Prom with a multi-MB query body. A reasonable hand-built expression
// is well under this and Prom's own limits sit at 1 MiB.
const maxExprBytes = 4 * 1024

// PromQuerier is the narrow PromQL surface this handler depends on.
// *tools.PrometheusClient satisfies it; tests stub it.
type PromQuerier interface {
	QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) (string, json.RawMessage, error)
}

// ObservabilityAPI serves the Monitor page: the dashboard catalog and the
// PromQL range-query passthrough its panels render from.
type ObservabilityAPI struct {
	prom       PromQuerier
	dashboards *grafana.Dashboards
	auth       *Auth
}

func NewObservabilityAPI(prom PromQuerier, dashboards *grafana.Dashboards, auth *Auth) *ObservabilityAPI {
	return &ObservabilityAPI{prom: prom, dashboards: dashboards, auth: auth}
}

func (h *ObservabilityAPI) Handle(r *ghttp.Request) {
	h.ServeHTTP(r.Response.BufferWriter, r.Request)
}

func (h *ObservabilityAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.auth.Require(w, r, RoleViewer, false); !ok {
		return
	}
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/"), "/")
	switch {
	case path == "prometheus/query_range":
		if r.Method != http.MethodPost {
			writeMethodNotAllowed(w)
			return
		}
		h.queryRange(w, r)
	case path == "observability/dashboards":
		if r.Method != http.MethodGet {
			writeMethodNotAllowed(w)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"dashboards": h.dashboards.List()})
	case strings.HasPrefix(path, "observability/dashboards/"):
		if r.Method != http.MethodGet {
			writeMethodNotAllowed(w)
			return
		}
		body, err := h.dashboards.Get(strings.TrimPrefix(path, "observability/dashboards/"))
		if errors.Is(err, grafana.ErrNotFound) {
			writeError(w, http.StatusNotFound, "dashboard not found")
			return
		}
		// Same { dashboard, meta } envelope as Grafana's dashboard API.
		writeJSON(w, http.StatusOK, map[string]any{"dashboard": body})
	default:
		writeNotFound(w)
	}
}

// queryRangeReq is the SPA's request body. start/end are RFC3339 strings,
// step is a Go duration ("30s" / "1m" / "5m").
type queryRangeReq struct {
	Expr  string `json:"expr"`
	Start string `json:"start"`
	End   string `json:"end"`
	Step  string `json:"step"`
}

// queryRangeResp is the JSON shape returned to the SPA. We pass the
// matrix through verbatim from Prom — each entry is
// `{ "metric": {label:value, ...}, "values": [[ts, "value"], ...] }`.
// `result_type` is hard-set to "matrix"; if Prom ever returns something
// else (shouldn't on query_range) we ship an empty result.
type queryRangeResp struct {
	ResultType string          `json:"result_type"`
	Result     json.RawMessage `json:"result"`
	From       string          `json:"from"`
	To         string          `json:"to"`
}

// queryRange is the auth'd PromQL range-query passthrough used by the
// Monitor page's PromQLPanel renderer. Any signed-in viewer may call it:
// PromQL is read-only and Prometheus only listens on loopback.
func (h *ObservabilityAPI) queryRange(w http.ResponseWriter, r *http.Request) {
	var req queryRangeReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2*maxExprBytes)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	expr := strings.TrimSpace(req.Expr)
	if expr == "" {
		writeError(w, http.StatusBadRequest, "expr is required")
		return
	}
	if len(expr) > maxExprBytes {
		writeError(w, http.StatusBadRequest, "expr too large")
		return
	}
	start, err := time.Parse(time.RFC3339, strings.TrimSpace(req.Start))
	if err != nil {
		writeError(w, http.StatusBadRequest, "start: "+err.Error())
		return
	}
	end, err := time.Parse(time.RFC3339, strings.TrimSpace(req.End))
	if err != nil {
		writeError(w, http.StatusBadRequest, "end: "+err.Error())
		return
	}
	if !end.After(start) {
		writeError(w, http.StatusBadRequest, "end must be after start")
		return
	}
	stepStr := strings.TrimSpace(req.Step)
	if stepStr == "" {
		writeError(w, http.StatusBadRequest, "step is required")
		return
	}
	step, err := time.ParseDuration(stepStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "step: "+err.Error())
		return
	}
	if step <= 0 {
		writeError(w, http.StatusBadRequest, "step must be > 0")
		return
	}

	// Bound the call so a misconfigured Prom can't tie up a goroutine past 30s.
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	resultType, result, err := h.prom.QueryRange(ctx, expr, start, end, step)
	if err != nil {
		// Prom parse errors and upstream failures look alike from here, so
		// 400 keeps the UI surface small. Operators see the message.
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	matrix := json.RawMessage("[]")
	if resultType == "matrix" && len(result) > 0 {
		matrix = result
	}
	writeJSON(w, http.StatusOK, queryRangeResp{
		ResultType: "matrix",
		Result:     matrix,
		From:       start.UTC().Format(time.RFC3339),
		To:         end.UTC().Format(time.RFC3339),
	})
}
