package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"oncall-agent/internal/store"
)

type flowFactsStore struct {
	fakeControlRoomStore
	runs  []store.AgentRun
	steps []store.AgentRunStep
}

func (f flowFactsStore) ListAgentRuns(context.Context, uint64, uint64, int) ([]store.AgentRun, error) {
	return f.runs, nil
}
func (f flowFactsStore) ListIncidentRunSteps(context.Context, uint64, uint64, uint64, int) ([]store.AgentRunStep, error) {
	return f.steps, nil
}

func readFlowFacts(t *testing.T, db flowFactsStore) ControlRoomDTO {
	t.Helper()
	response := httptest.NewRecorder()
	NewControlRoomAPI(db, testAuth(t)).ServeHTTP(response, withBearer(httptest.NewRequest(http.MethodGet, "/api/v1/incidents/11/control-room", nil), testViewerToken))
	var body ControlRoomDTO
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || response.Code != 200 {
		t.Fatalf("response=%s err=%v", response.Body.String(), err)
	}
	return body
}

func TestControlRoomVerificationOutcomeSurvivesEventWindowEviction(t *testing.T) {
	row := completeApproval(t)
	row.Status = "executed"
	row.Verification = &store.VerifyTask{ApprovalID: row.ID, Status: "inconclusive"}
	now := time.Now().UTC()
	var events []store.IncidentEvent
	for i := range 20 {
		events = append(events, store.IncidentEvent{ID: uint64(i + 100), RunID: &row.RunID, EventType: "conversation.answered", Status: "succeeded"})
	}
	body := readFlowFacts(t, flowFactsStore{
		fakeControlRoomStore: fakeControlRoomStore{incident: store.Incident{ID: 11, Status: "firing"}, approvals: []store.Approval{row}, events: events},
		runs:                 []store.AgentRun{{ID: row.RunID, IncidentID: 11, Status: "succeeded"}},
		steps:                []store.AgentRunStep{{ID: 8, RunID: row.RunID, Seq: 90, Kind: "verify", Name: "sub2api_http_health", StartedAt: now, FinishedAt: &now}},
	})
	if body.LatestAction == nil || body.LatestAction.Verification.Status != "inconclusive" {
		t.Fatal("fixture lost authoritative verification")
	}
	for _, node := range body.FlowNodes {
		if node.Kind == "verify" && node.Status != "inconclusive" {
			t.Fatalf("step completion invented successful recovery: %+v", node)
		}
	}
}

func TestControlRoomFlowDoesNotReusePreviousRunAction(t *testing.T) {
	old := completeApproval(t)
	old.Status = "executed"
	old.Verification = &store.VerifyTask{ApprovalID: old.ID, Status: "passed"}
	body := readFlowFacts(t, flowFactsStore{
		fakeControlRoomStore: fakeControlRoomStore{incident: store.Incident{ID: 11, Status: "firing"}, approvals: []store.Approval{old}, events: []store.IncidentEvent{
			{RunID: &old.RunID, EventType: "execution.completed", Status: "executed"},
			{RunID: &old.RunID, EventType: "verify.passed", Status: "passed"},
		}},
		runs: []store.AgentRun{{ID: old.RunID + 1, IncidentID: 11, Status: "running"}},
	})
	if body.LatestAction == nil || body.LatestAction.Status != "executed" {
		t.Fatal("history panel must retain previous action")
	}
	for _, node := range body.FlowNodes {
		if (node.Kind == "execute" || node.Kind == "verify") && node.Status != "not_started" {
			t.Fatalf("new Run inherited an old action: %+v", node)
		}
	}
}
