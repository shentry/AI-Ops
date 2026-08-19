package diagnose

import (
	"context"
	"strings"
	"testing"
	"time"

	"oncall-agent/internal/store"
)

type fakeVerifyStore struct {
	members []store.IncidentMember
	steps   []store.AgentRunStep
}

func (f *fakeVerifyStore) ListIncidentMembers(context.Context, uint64) ([]store.IncidentMember, error) {
	return f.members, nil
}

func (f *fakeVerifyStore) AppendRunStep(_ context.Context, step store.AgentRunStep) error {
	f.steps = append(f.steps, step)
	return nil
}

func TestVerifyPassesWhenAllResolved(t *testing.T) {
	db := &fakeVerifyStore{members: []store.IncidentMember{
		{Fingerprint: "a", Status: "resolved"},
		{Fingerprint: "b", Status: "resolved"},
	}}
	verifier := NewVerifier(db)
	result := verifier.VerifyAfterExecution(context.Background(), 11, 7, 0)
	if !result.Passed {
		t.Fatalf("result = %+v, want passed", result)
	}
	if len(db.steps) != 1 || db.steps[0].Kind != "verify" || db.steps[0].RunID != 11 {
		t.Fatalf("steps = %+v", db.steps)
	}
	if !strings.Contains(string(*db.steps[0].OutputJSON), "passed=true") {
		t.Fatalf("step output = %v", *db.steps[0].OutputJSON)
	}
}

func TestVerifyFailsWhileFiring(t *testing.T) {
	db := &fakeVerifyStore{members: []store.IncidentMember{
		{Fingerprint: "a", Status: "resolved"},
		{Fingerprint: "b", Status: "firing"},
	}}
	result := NewVerifier(db).VerifyAfterExecution(context.Background(), 11, 7, 0)
	if result.Passed || !strings.Contains(result.Detail, "1/2") {
		t.Fatalf("result = %+v, want failed", result)
	}
}

func TestVerifyDelayRespectsContext(t *testing.T) {
	db := &fakeVerifyStore{}
	verifier := NewVerifier(db)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := verifier.VerifyAfterExecution(ctx, 11, 7, time.Hour)
	if result.Passed || !strings.Contains(result.Detail, "canceled") {
		t.Fatalf("result = %+v, want canceled", result)
	}
	// 取消的路径也落 step。
	if len(db.steps) != 1 {
		t.Fatalf("steps = %d, want 1", len(db.steps))
	}
}
