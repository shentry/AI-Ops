package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"oncall-agent/internal/store"
)

type fakeControlRoomStore struct {
	incident  store.Incident
	events    []store.IncidentEvent
	approvals []store.Approval
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
func (f fakeControlRoomStore) ListIncidentApprovals(_ context.Context, _ uint64, status string, limit int) ([]store.Approval, error) {
	var rows []store.Approval
	for _, row := range f.approvals {
		if status == "" || row.Status == status {
			rows = append(rows, row)
		}
		if len(rows) == limit {
			break
		}
	}
	return rows, nil
}
func (f fakeControlRoomStore) ListIncidentRunSteps(context.Context, uint64, uint64, uint64, int) ([]store.AgentRunStep, error) {
	return nil, nil
}
func (f fakeControlRoomStore) ListLatestIncidents(context.Context, string, int) ([]store.Incident, error) {
	return []store.Incident{f.incident}, nil
}
func (f fakeControlRoomStore) GetApproval(context.Context, uint64) (store.Approval, error) {
	return store.Approval{}, store.ErrApprovalNotFound
}

func TestControlRoomRejectsAnonymousRequests(t *testing.T) {
	api := NewControlRoomAPI(fakeControlRoomStore{incident: store.Incident{ID: 1, Status: "open"}}, testAuth(t))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/incidents/1/control-room", nil)
	req.Header.Set("X-Operator", "admin")
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
	}, testAuth(t))
	req := withBearer(httptest.NewRequest(http.MethodGet, "/api/v1/incidents/8/control-room", nil), testViewerToken)
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

func TestControlRoomAllowsViewerSession(t *testing.T) {
	auth := testAuth(t)
	api := NewControlRoomAPI(fakeControlRoomStore{incident: store.Incident{ID: 9, Status: "open"}}, auth)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/incidents/9/control-room", nil)
	req.AddCookie(login(t, auth, testViewerToken))
	resp := httptest.NewRecorder()
	api.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("viewer session = %d body=%s", resp.Code, resp.Body.String())
	}
}

func TestControlRoomLatestActionIndependentOfPending(t *testing.T) {
	for _, pending := range []bool{false, true} {
		latest := completeApproval(t)
		latest.ID = 12
		latest.Status = "executed"
		latest.Verification = &store.VerifyTask{ApprovalID: latest.ID, Status: "running"}
		rows := []store.Approval{latest}
		if pending {
			rows = append(rows, completeApproval(t))
		}
		api := NewControlRoomAPI(fakeControlRoomStore{incident: store.Incident{ID: 11, Status: "firing"}, approvals: rows}, testAuth(t))
		resp := httptest.NewRecorder()
		api.ServeHTTP(resp, withBearer(httptest.NewRequest(http.MethodGet, "/api/v1/incidents/11/control-room", nil), testViewerToken))
		var body ControlRoomDTO
		if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if resp.Code != http.StatusOK || body.LatestAction == nil || body.LatestAction.ID != 12 || body.LatestAction.Verification.Status != "running" {
			t.Fatalf("latest action: %d %s", resp.Code, resp.Body.String())
		}
		if pending != (body.PendingApproval != nil) {
			t.Fatalf("pending conflated with latest: %s", resp.Body.String())
		}
	}
}

func TestControlRoomFlowUsesTerminalSnapshotWhenEventsAreAbsent(t *testing.T) {
	for _, status := range []string{"aborted", "denied", "expired", "failed", "executed"} {
		t.Run(status, func(t *testing.T) {
			row := completeApproval(t)
			row.Status = status
			var events []store.IncidentEvent
			wantVerification := "not_applicable"
			if status == "executed" {
				row.Verification = &store.VerifyTask{ApprovalID: row.ID, Status: "passed"}
				events = []store.IncidentEvent{{EventType: "verify.passed", Status: "passed"}}
				wantVerification = "passed" // Use the task verdict, not generic event completion.
			}
			handler := NewControlRoomAPI(fakeControlRoomStore{incident: store.Incident{ID: 11, Status: "firing"}, approvals: []store.Approval{row}, events: events}, testAuth(t))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, withBearer(httptest.NewRequest(http.MethodGet, "/api/v1/incidents/11/control-room", nil), testViewerToken))
			var body ControlRoomDTO
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || response.Code != http.StatusOK {
				t.Fatalf("response=%s err=%v", response.Body.String(), err)
			}
			statuses := make(map[string]string)
			for _, node := range body.FlowNodes {
				statuses[node.Kind] = node.Status
			}
			if statuses["execute"] != status || statuses["verify"] != wantVerification {
				t.Fatalf("flow=%v want execution=%s verify=%s", statuses, status, wantVerification)
			}
		})
	}
}
