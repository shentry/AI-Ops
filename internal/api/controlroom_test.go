package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"oncall-agent/internal/auth"
	"oncall-agent/internal/store"
)

type fakeSessionAuth struct {
	unauth bool
}

func (f fakeSessionAuth) Authenticate(context.Context, *http.Request) (auth.Actor, store.WebSession, error) {
	if f.unauth {
		return auth.Actor{}, store.WebSession{}, auth.ErrUnauthenticated
	}
	return auth.Actor{ID: "ou_1", Name: "ops"}, store.WebSession{ID: "sess"}, nil
}

type fakeControlRoomStore struct {
	incident store.Incident
	events   []store.IncidentEvent
}

func (f fakeControlRoomStore) GetIncident(context.Context, uint64) (store.Incident, error) {
	return f.incident, nil
}
func (f fakeControlRoomStore) ListIncidentMembers(context.Context, uint64) ([]store.IncidentMember, error) {
	return []store.IncidentMember{{Fingerprint: "fp", Name: "alert", Status: "firing"}}, nil
}
func (f fakeControlRoomStore) ListIncidentEvents(context.Context, uint64, uint64, int) ([]store.IncidentEvent, error) {
	return f.events, nil
}
func (f fakeControlRoomStore) ListLatestIncidentEvents(context.Context, uint64, int) ([]store.IncidentEvent, error) {
	return f.events, nil
}
func (f fakeControlRoomStore) ListIncidentProblems(context.Context, uint64, string, int) ([]store.IncidentProblem, error) {
	return nil, nil
}
func (f fakeControlRoomStore) ListAgentRuns(context.Context, uint64, uint64, int) ([]store.AgentRun, error) {
	return nil, nil
}
func (f fakeControlRoomStore) ListIncidentApprovals(context.Context, uint64, string, int) ([]store.Approval, error) {
	return nil, nil
}

func TestControlRoomRequiresSession(t *testing.T) {
	api := NewControlRoomAPI(fakeControlRoomStore{incident: store.Incident{ID: 1, Status: "open"}}, fakeSessionAuth{unauth: true})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/incidents/1/control-room", nil)
	resp := httptest.NewRecorder()
	api.ServeHTTP(resp, req)
	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d", resp.Code)
	}
}

func TestControlRoomReturnsMembersAndLatestEvents(t *testing.T) {
	api := NewControlRoomAPI(fakeControlRoomStore{
		incident: store.Incident{ID: 8, Status: "open", Title: "cpu"},
		events:   []store.IncidentEvent{{ID: 12, IncidentID: 8, EventType: "run.started", Phase: "diagnose", Status: "running", Summary: "started"}},
	}, fakeSessionAuth{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/incidents/8/control-room", nil)
	resp := httptest.NewRecorder()
	api.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("code = %d body=%s", resp.Code, resp.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatalf("json: %v", err)
	}
	members, _ := body["members"].([]any)
	events, _ := body["recent_events"].([]any)
	if len(members) != 1 || len(events) != 1 {
		t.Fatalf("body = %s", resp.Body.String())
	}
}

func TestControlRoomAllowsAnonymousConsole(t *testing.T) {
	api := NewControlRoomAPI(
		fakeControlRoomStore{incident: store.Incident{ID: 9, Status: "open"}},
		NewAnonymousConsoleAuthenticator(),
	)
	resp := httptest.NewRecorder()
	api.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/api/v1/incidents/9/control-room", nil))
	if resp.Code != http.StatusOK {
		t.Fatalf("anonymous console = %d body=%s", resp.Code, resp.Body.String())
	}
}
