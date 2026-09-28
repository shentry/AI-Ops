package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"gorm.io/datatypes"

	"oncall-agent/internal/config"
	"oncall-agent/internal/store"
	"oncall-agent/internal/tools"
	"oncall-agent/internal/topology"
)

type fakeIncidentAlerts struct {
	alerts []store.Alert
	err    error
	asked  uint64
}

func (f *fakeIncidentAlerts) ListIncidentAlerts(_ context.Context, id uint64) ([]store.Alert, error) {
	f.asked = id
	return f.alerts, f.err
}

func topologyRequest(t *testing.T, alerts *fakeIncidentAlerts, target, token string) (*httptest.ResponseRecorder, topologyResponse) {
	t.Helper()
	graph := topology.New(config.TopologyConfig{
		Nodes: []config.TopologyNode{{ID: "postgres", Kind: "datastore"}, {ID: "redis", Kind: "datastore"}},
		Edges: []config.TopologyEdge{{From: "sub2api", To: "postgres", Type: config.TopologyDependsOn}},
	}, config.ServiceConfig{Name: "sub2api", Container: "sub2api"}, tools.NewRegistry())
	resp := httptest.NewRecorder()
	NewTopologyAPI(graph, alerts, testAuth(t)).ServeHTTP(resp, withBearer(httptest.NewRequest(http.MethodGet, target, nil), token))
	var body topologyResponse
	_ = json.Unmarshal(resp.Body.Bytes(), &body)
	return resp, body
}

func TestTopologyReturnsGraphAndIncidentHighlight(t *testing.T) {
	alerts := &fakeIncidentAlerts{alerts: []store.Alert{
		{Labels: datatypes.JSON(`{"alertname":"Sub2APIPostgresUnreachable","service":"sub2api","component":"postgres"}`)},
		{Labels: datatypes.JSON(`{"alertname":"Sub2APIPostgresConnectionsHigh","service":"sub2api","component":"postgres"}`)},
		{Labels: datatypes.JSON(`{"alertname":"Sub2APIDown","service":"sub2api","container":"sub2api"}`)},
		{Labels: datatypes.JSON(`{"alertname":"HostDiskAlmostFull"}`)},
	}}
	resp, body := topologyRequest(t, alerts, "/api/v1/topology?incident=42", testViewerToken)
	if resp.Code != http.StatusOK || alerts.asked != 42 {
		t.Fatalf("status = %d asked=%d body=%s", resp.Code, alerts.asked, resp.Body)
	}
	if !reflect.DeepEqual(body.Highlight, []string{"postgres", "sub2api"}) || len(body.Nodes) != 3 || body.Nodes[0].ID != "sub2api" || len(body.Edges) != 1 {
		t.Fatalf("body = %s", resp.Body)
	}
	// Without an incident the highlight is an empty list, never null.
	resp, _ = topologyRequest(t, &fakeIncidentAlerts{}, "/api/v1/topology", testViewerToken)
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(resp.Body.Bytes(), &raw)
	if string(raw["highlight"]) != "[]" {
		t.Fatalf("highlight = %s", raw["highlight"])
	}
}

func TestTopologyRejectsBadRequests(t *testing.T) {
	for name, test := range map[string]struct {
		alerts *fakeIncidentAlerts
		target string
		token  string
		want   int
	}{
		"machine token":     {&fakeIncidentAlerts{}, "/api/v1/topology", testMachineToken, http.StatusForbidden},
		"anonymous":         {&fakeIncidentAlerts{}, "/api/v1/topology", "", http.StatusUnauthorized},
		"bad incident":      {&fakeIncidentAlerts{}, "/api/v1/topology?incident=abc", testViewerToken, http.StatusBadRequest},
		"zero incident":     {&fakeIncidentAlerts{}, "/api/v1/topology?incident=0", testViewerToken, http.StatusBadRequest},
		"store unavailable": {&fakeIncidentAlerts{err: errors.New("db down")}, "/api/v1/topology?incident=1", testViewerToken, http.StatusServiceUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			if resp, _ := topologyRequest(t, test.alerts, test.target, test.token); resp.Code != test.want {
				t.Fatalf("status = %d, want %d: %s", resp.Code, test.want, resp.Body)
			}
		})
	}
}
