package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"oncall-agent/internal/incident"
)

// Faults are raised by real MySQL, after earlier writes in the same transaction.
func rejectInsert(t *testing.T, db *DB, table, condition string) func() {
	t.Helper()
	name := fmt.Sprintf("oncall_fault_%d", time.Now().UnixNano())
	sql := fmt.Sprintf("CREATE TRIGGER %s BEFORE INSERT ON %s FOR EACH ROW BEGIN IF %s THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'oncall injected failure'; END IF; END", name, table, condition)
	if err := db.Exec(sql).Error; err != nil {
		t.Fatal(err)
	}
	drop := func() {
		if err := db.Exec("DROP TRIGGER IF EXISTS " + name).Error; err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(drop)
	return drop
}

func TestExecutionResultAndVerifyTaskRollbackTogether(t *testing.T) {
	db := openIntegrationDB(t)
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	approval, policy := executionFixture(t, db, now, "approved")
	if _, claimed, err := db.ClaimApprovalExecution(ctx, approval.ID, now, policy); err != nil || !claimed {
		t.Fatalf("claim=%v %v", claimed, err)
	}
	before, err := db.ListIncidentEvents(ctx, approval.IncidentID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	drop := rejectInsert(t, db, "verify_task", fmt.Sprintf("NEW.approval_id = %d", approval.ID))
	completion := ExecutionCompletion{ApprovalID: approval.ID, Status: "executed", ResultJSON: []byte(`{"output":"real action already returned success"}`), FinishedAt: now}
	if err := db.FinishExecution(ctx, completion); err == nil || !strings.Contains(err.Error(), "oncall injected failure") {
		t.Fatalf("expected injected failure: %v", err)
	}
	got, err := db.GetApproval(ctx, approval.ID)
	if err != nil || got.Status != "executing" || got.ResultJSON != nil || got.Verification != nil {
		t.Fatalf("partial commit: %+v %v", got, err)
	}
	after, err := db.ListIncidentEvents(ctx, approval.IncidentID, 0, 100)
	if err != nil || len(after) != len(before) {
		t.Fatalf("partial audit commit: %d vs %d %v", len(after), len(before), err)
	}
	drop()
	if err := db.FinishExecution(ctx, completion); err != nil {
		t.Fatal(err)
	}
	got, err = db.GetApproval(ctx, approval.ID)
	if err != nil || got.Status != "executed" || got.Verification == nil {
		t.Fatalf("retry=%+v %v", got, err)
	}
}

func verificationFixture(t *testing.T, db *DB, now time.Time) (Approval, RemediationPolicy, VerifyTask) {
	t.Helper()
	approval, policy := executionFixture(t, db, now, "approved")
	if _, claimed, err := db.ClaimApprovalExecution(context.Background(), approval.ID, now, policy); err != nil || !claimed {
		t.Fatalf("claim=%v %v", claimed, err)
	}
	if err := db.FinishExecution(context.Background(), ExecutionCompletion{ApprovalID: approval.ID, Status: "executed", ResultJSON: []byte(`{"written":true,"detail":"restart returned success"}`), FinishedAt: now}); err != nil {
		t.Fatal(err)
	}
	approval, err := db.GetApproval(context.Background(), approval.ID)
	if err != nil || approval.Verification == nil {
		t.Fatalf("approval=%+v %v", approval, err)
	}
	return approval, policy, *approval.Verification
}

func TestVerificationStaleClaimCannotCompleteNewClaim(t *testing.T) {
	db := openIntegrationDB(t)
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	approval, _, original := verificationFixture(t, db, now)
	first, claimed, err := db.ClaimVerificationTask(ctx, approval.ID, now)
	if err != nil || !claimed {
		t.Fatalf("first=%v %v", claimed, err)
	}
	if n, err := db.RequeueStaleVerificationTasks(ctx, now.Add(time.Second)); err != nil || n != 1 {
		t.Fatalf("requeue=%d %v", n, err)
	}
	second, claimed, err := db.ClaimVerificationTask(ctx, approval.ID, now.Add(31*time.Second))
	if err != nil || !claimed || second.ClaimedAt.Equal(*first.ClaimedAt) || !second.DeadlineAt.Equal(original.DeadlineAt) {
		t.Fatalf("second=%+v %v", second, err)
	}
	completion := VerificationCompletion{ApprovalID: approval.ID, ClaimedAt: *first.ClaimedAt, CheckedAt: now.Add(32 * time.Second), Status: "passed", Observation: "healthy", Detail: "HTTP 200"}
	if result, err := db.FinalizeVerification(ctx, completion); err != nil || result.Applied {
		t.Fatalf("stale completion=%+v %v", result, err)
	}
	got, err := db.GetApproval(ctx, approval.ID)
	if err != nil || got.Verification.Status != "running" || !got.Verification.ClaimedAt.Equal(*second.ClaimedAt) {
		t.Fatalf("new claim overwritten: %+v %v", got, err)
	}
	completion.ClaimedAt = *second.ClaimedAt
	if result, err := db.FinalizeVerification(ctx, completion); err != nil || !result.Applied || result.Status != "passed" {
		t.Fatalf("new completion=%+v %v", result, err)
	}
}

func TestVerificationMemoryAndAuditRollbackTogether(t *testing.T) {
	db := openIntegrationDB(t)
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	approval, _, _ := verificationFixture(t, db, now)
	task, claimed, err := db.ClaimVerificationTask(ctx, approval.ID, now)
	if err != nil || !claimed {
		t.Fatalf("claim=%v %v", claimed, err)
	}
	parent, err := db.GetIncident(ctx, approval.IncidentID)
	if err != nil {
		t.Fatal(err)
	}
	run, err := db.GetAgentRun(ctx, approval.RunID)
	if err != nil || run.PlanJSON == nil {
		t.Fatalf("run=%+v %v", run, err)
	}
	fp := incident.FaultFingerprint(parent.GroupKey, testAlert)
	memory := FaultMemory{Fingerprint: fp, GroupKey: parent.GroupKey, AlertName: testAlert, RCAText: "recovered", PlanJSON: *run.PlanJSON, Confidence: "high", FirstSeen: now, LastSuccess: now, TTLSeconds: 3600}
	completion := VerificationCompletion{ApprovalID: approval.ID, ClaimedAt: *task.ClaimedAt, CheckedAt: now.Add(time.Second), Status: "passed", Observation: "healthy", Detail: "HTTP 200", Memory: &memory}
	before, err := db.ListIncidentEvents(ctx, approval.IncidentID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	steps, err := db.ListRunSteps(ctx, approval.RunID)
	if err != nil {
		t.Fatal(err)
	}
	drop := rejectInsert(t, db, "fault_memory", fmt.Sprintf("NEW.fingerprint = '%s'", fp))
	if _, err := db.FinalizeVerification(ctx, completion); err == nil || !strings.Contains(err.Error(), "oncall injected failure") {
		t.Fatalf("expected injected failure: %v", err)
	}
	got, err := db.GetApproval(ctx, approval.ID)
	if err != nil || got.Verification.Status != "running" || got.Verification.LastCheckedAt != nil {
		t.Fatalf("partial task commit: %+v %v", got, err)
	}
	if _, err := db.GetFaultMemory(ctx, fp); !errors.Is(err, ErrMemoryNotFound) {
		t.Fatalf("partial memory commit: %v", err)
	}
	after, err := db.ListIncidentEvents(ctx, approval.IncidentID, 0, 100)
	if err != nil || len(after) != len(before) {
		t.Fatalf("partial events=%d vs %d %v", len(after), len(before), err)
	}
	afterSteps, err := db.ListRunSteps(ctx, approval.RunID)
	if err != nil || len(afterSteps) != len(steps) {
		t.Fatalf("partial steps=%d vs %d %v", len(afterSteps), len(steps), err)
	}
	drop()
	if result, err := db.FinalizeVerification(ctx, completion); err != nil || !result.Applied || result.Status != "passed" {
		t.Fatalf("retry=%+v %v", result, err)
	}
	if entry, err := db.GetFaultMemory(ctx, fp); err != nil || entry.Confidence != "high" {
		t.Fatalf("memory=%+v %v", entry, err)
	}
	events, err := db.ListIncidentEvents(ctx, approval.IncidentID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if result, err := db.FinalizeVerification(ctx, completion); err != nil || result.Applied {
		t.Fatalf("duplicate=%+v %v", result, err)
	}
	after, err = db.ListIncidentEvents(ctx, approval.IncidentID, 0, 100)
	if err != nil || len(after) != len(events) {
		t.Fatalf("duplicate effects=%d vs %d %v", len(after), len(events), err)
	}
}
