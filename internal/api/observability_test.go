package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"oncall-agent/internal/grafana"
)

type fakePromQuerier struct {
	expr       string
	start, end time.Time
	step       time.Duration
	resultType string
	result     string
	err        error
}

func (f *fakePromQuerier) QueryRange(_ context.Context, expr string, start, end time.Time, step time.Duration) (string, json.RawMessage, error) {
	f.expr, f.start, f.end, f.step = expr, start, end, step
	return f.resultType, json.RawMessage(f.result), f.err
}

func observabilityAPI(t *testing.T, prom *fakePromQuerier) *ObservabilityAPI {
	t.Helper()
	dashboards, err := grafana.Load()
	if err != nil {
		t.Fatal(err)
	}
	return NewObservabilityAPI(prom, dashboards, testAuth(t))
}

func queryRangeRequest(body string) *http.Request {
	return withBearer(httptest.NewRequest(http.MethodPost, "/api/v1/prometheus/query_range", strings.NewReader(body)), testViewerToken)
}

func TestQueryRangePassesMatrixThrough(t *testing.T) {
	prom := &fakePromQuerier{resultType: "matrix", result: `[{"metric":{"job":"loki"},"values":[[1790596800,"1"]]}]`}
	resp := httptest.NewRecorder()
	observabilityAPI(t, prom).ServeHTTP(resp, queryRangeRequest(`{"expr":" up ","start":"2026-09-28T11:00:00Z","end":"2026-09-28T12:00:00Z","step":"15s"}`))
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.Code, resp.Body)
	}
	if prom.expr != "up" || prom.step != 15*time.Second || !prom.end.Equal(time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("query = %q %v..%v step %v", prom.expr, prom.start, prom.end, prom.step)
	}
	var body queryRangeResp
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.ResultType != "matrix" || !strings.Contains(string(body.Result), `"job":"loki"`) || body.From != "2026-09-28T11:00:00Z" {
		t.Fatalf("body = %s", resp.Body)
	}
}

func TestQueryRangeRejectsBadInput(t *testing.T) {
	prom := &fakePromQuerier{resultType: "matrix", result: "[]"}
	api := observabilityAPI(t, prom)
	for _, body := range []string{
		`{"expr":"","start":"2026-09-28T11:00:00Z","end":"2026-09-28T12:00:00Z","step":"15s"}`,
		`{"expr":"` + strings.Repeat("x", maxExprBytes+1) + `","start":"2026-09-28T11:00:00Z","end":"2026-09-28T12:00:00Z","step":"15s"}`,
		`{"expr":"up","start":"yesterday","end":"2026-09-28T12:00:00Z","step":"15s"}`,
		`{"expr":"up","start":"2026-09-28T12:00:00Z","end":"2026-09-28T11:00:00Z","step":"15s"}`,
		`{"expr":"up","start":"2026-09-28T11:00:00Z","end":"2026-09-28T12:00:00Z","step":"0s"}`,
		`{"expr":"up","start":"2026-09-28T11:00:00Z","end":"2026-09-28T12:00:00Z","step":""}`,
		`not json`,
	} {
		prom.expr = ""
		resp := httptest.NewRecorder()
		api.ServeHTTP(resp, queryRangeRequest(body))
		if resp.Code != http.StatusBadRequest || prom.expr != "" {
			t.Fatalf("body %.60s: status=%d reached prom=%v", body, resp.Code, prom.expr != "")
		}
	}
}

func TestQueryRangeSurfacesPrometheusErrorsAndNonMatrix(t *testing.T) {
	prom := &fakePromQuerier{err: errors.New("prometheus bad_data: parse error")}
	resp := httptest.NewRecorder()
	observabilityAPI(t, prom).ServeHTTP(resp, queryRangeRequest(`{"expr":"up(","start":"2026-09-28T11:00:00Z","end":"2026-09-28T12:00:00Z","step":"15s"}`))
	if resp.Code != http.StatusBadRequest || !strings.Contains(resp.Body.String(), "parse error") {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body)
	}
	prom = &fakePromQuerier{resultType: "vector", result: `[{"metric":{},"value":[1,"1"]}]`}
	resp = httptest.NewRecorder()
	observabilityAPI(t, prom).ServeHTTP(resp, queryRangeRequest(`{"expr":"up","start":"2026-09-28T11:00:00Z","end":"2026-09-28T12:00:00Z","step":"15s"}`))
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), `"result":[]`) {
		t.Fatalf("non-matrix result must be emptied: %s", resp.Body)
	}
}

func TestObservabilityRequiresSessionAndMethods(t *testing.T) {
	api := observabilityAPI(t, &fakePromQuerier{resultType: "matrix", result: "[]"})
	resp := httptest.NewRecorder()
	api.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/api/v1/observability/dashboards", nil))
	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d", resp.Code)
	}
	resp = httptest.NewRecorder()
	api.ServeHTTP(resp, withBearer(httptest.NewRequest(http.MethodGet, "/api/v1/observability/dashboards", nil), testMachineToken))
	if resp.Code != http.StatusForbidden {
		t.Fatalf("machine token status = %d", resp.Code)
	}
	resp = httptest.NewRecorder()
	api.ServeHTTP(resp, withBearer(httptest.NewRequest(http.MethodGet, "/api/v1/prometheus/query_range", nil), testViewerToken))
	if resp.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET query_range status = %d", resp.Code)
	}
}

func TestObservabilityServesEmbeddedDashboards(t *testing.T) {
	api := observabilityAPI(t, &fakePromQuerier{})
	resp := httptest.NewRecorder()
	api.ServeHTTP(resp, withBearer(httptest.NewRequest(http.MethodGet, "/api/v1/observability/dashboards", nil), testViewerToken))
	var list struct {
		Dashboards []grafana.Summary `json:"dashboards"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &list); err != nil || len(list.Dashboards) != 4 || list.Dashboards[0].UID != "sub2api" {
		t.Fatalf("list = %s (%v)", resp.Body, err)
	}
	resp = httptest.NewRecorder()
	api.ServeHTTP(resp, withBearer(httptest.NewRequest(http.MethodGet, "/api/v1/observability/dashboards/oncall-agent", nil), testViewerToken))
	var got struct {
		Dashboard struct {
			UID    string            `json:"uid"`
			Panels []json.RawMessage `json:"panels"`
		} `json:"dashboard"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &got); err != nil || got.Dashboard.UID != "oncall-agent" || len(got.Dashboard.Panels) == 0 {
		t.Fatalf("dashboard = %.200s (%v)", resp.Body, err)
	}
	resp = httptest.NewRecorder()
	api.ServeHTTP(resp, withBearer(httptest.NewRequest(http.MethodGet, "/api/v1/observability/dashboards/nope", nil), testViewerToken))
	if resp.Code != http.StatusNotFound {
		t.Fatalf("unknown uid status = %d", resp.Code)
	}
}
