package store

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// An interrupted execution stays executing through TTL sweeps until the
// executor reconciles it with the target; an unknown outcome is recorded as
// failed with a manual check, blocks the rule and is never retried.
func TestInterruptedExecutionAwaitsReconciliation(t *testing.T) {
	db := openIntegrationDB(t)
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	stuck, policy := executionFixture(t, db, now, "approved")
	if _, claimed, err := db.ClaimApprovalExecution(ctx, stuck.ID, now, policy); err != nil || !claimed {
		t.Fatalf("claim=%v %v", claimed, err)
	}
	if _, err := db.ExpireApprovals(ctx, now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	rows, err := db.ListExecutingApprovals(ctx)
	if err != nil || !containsApproval(rows, stuck.ID) {
		t.Fatalf("executing=%v err=%v", rows, err)
	}
	t.Cleanup(func() {
		db.Where("actor = ? AND reason = ?", "system:execution", fmt.Sprintf("approval %d outcome is unknown; reconcile before resuming", stuck.ID)).Delete(&ControlEvent{})
	})
	completion := ExecutionCompletion{ApprovalID: stuck.ID, Status: "failed", ManualCheck: true, ResultJSON: []byte(`{"outcome":"unknown","error":"target state could not be read"}`), FinishedAt: now.Add(time.Minute)}
	if err := db.FinishExecution(ctx, completion); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetApproval(ctx, stuck.ID)
	if err != nil || got.Status != "failed" || got.Verification != nil {
		t.Fatalf("approval=%+v err=%v", got, err)
	}
	for _, code := range []string{"execution_failed", "manual_check"} {
		assertScopeRaceCount(t, db, &IncidentProblem{}, "incident_id = ? AND code = ? AND status = ?", []any{stuck.IncidentID, code, "open"}, 1)
	}
	if rows, err := db.ListExecutingApprovals(ctx); err != nil || containsApproval(rows, stuck.ID) {
		t.Fatalf("reconciled execution still listed: %v", err)
	}
	state, err := db.RemediationState(ctx, RemediationQuery{Service: *stuck.Service, RuleID: *stuck.RuleID, IncidentID: stuck.IncidentID, Since: now.Add(-time.Hour)})
	if err != nil || !state.Stopped || state.Executions != 1 || state.IncidentActions != 1 || state.Blocked == "" || state.BusyWith != 0 {
		t.Fatalf("state after unknown outcome=%+v err=%v", state, err)
	}
}

func containsApproval(rows []Approval, id uint64) bool {
	for _, row := range rows {
		if row.ID == id {
			return true
		}
	}
	return false
}
