package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"oncall-agent/internal/incident"
)

// The store, not only the worker, refuses a "passed" verdict until the frozen
// snapshot's consecutive healthy observations are durably recorded; any
// non-healthy observation resets the streak.
func TestVerificationPassRequiresDurableConsecutiveStreak(t *testing.T) {
	db := openIntegrationDB(t)
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	approval, policy := executionFixture(t, db, now, "approved")
	resnapshot(t, db, &approval, func(s *incident.ExecutionContext) { s.Verification.RequiredPasses = 3 })
	if _, claimed, err := db.ClaimApprovalExecution(ctx, approval.ID, now, policy); err != nil || !claimed {
		t.Fatalf("claim=%v %v", claimed, err)
	}
	if err := db.FinishExecution(ctx, ExecutionCompletion{ApprovalID: approval.ID, Status: "executed", ResultJSON: []byte(`{"output":"restarted"}`), FinishedAt: now}); err != nil {
		t.Fatal(err)
	}

	at := now
	observe := func(observation, status string) (VerificationFinalization, error) {
		t.Helper()
		task, claimed, err := db.ClaimVerificationTask(ctx, approval.ID, at)
		if err != nil || !claimed {
			t.Fatalf("claim at %s = %v %v", at, claimed, err)
		}
		completion := VerificationCompletion{ApprovalID: approval.ID, ClaimedAt: *task.ClaimedAt, CheckedAt: at, Status: status, Observation: observation, Detail: observation}
		if status == "pending" {
			completion.NextCheckAt = at.Add(10 * time.Second)
		}
		result, err := db.FinalizeVerification(ctx, completion)
		if err != nil {
			// A rejected verdict leaves the claim; release it as an ordinary pending observation.
			if _, err := db.RequeueStaleVerificationTasks(ctx, at.Add(time.Millisecond)); err != nil {
				t.Fatal(err)
			}
		}
		at = at.Add(10 * time.Second)
		return result, err
	}
	streak := func() int {
		t.Helper()
		got, err := db.GetApproval(ctx, approval.ID)
		if err != nil || got.Verification == nil {
			t.Fatalf("approval=%+v %v", got, err)
		}
		return got.Verification.ConsecutivePasses
	}

	if _, err := observe("healthy", "pending"); err != nil || streak() != 1 {
		t.Fatalf("first healthy: streak=%d %v", streak(), err)
	}
	if _, err := observe("unavailable", "pending"); err != nil || streak() != 0 {
		t.Fatalf("unavailable must reset: streak=%d %v", streak(), err)
	}
	if _, err := observe("healthy", "pending"); err != nil || streak() != 1 {
		t.Fatalf("restart streak: streak=%d %v", streak(), err)
	}
	if _, err := observe("healthy", "passed"); err == nil || !strings.Contains(err.Error(), "consecutive") || streak() != 1 {
		t.Fatalf("premature pass accepted: streak=%d %v", streak(), err)
	}
	if _, err := observe("healthy", "pending"); err != nil || streak() != 2 {
		t.Fatalf("second healthy: streak=%d %v", streak(), err)
	}
	result, err := observe("healthy", "passed")
	if err != nil || !result.Applied || result.Status != "passed" || streak() != 3 {
		t.Fatalf("pass=%+v streak=%d %v", result, streak(), err)
	}
}
