package store

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestFailedPublicationCanRecoverSameRunWithRepeatedStepSequence(t *testing.T) {
	db := openIntegrationDB(t)
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	oldTime := now.Add(-10 * time.Minute)
	previous, _ := executionFixture(t, db, oldTime, false, "pending")
	if _, err := db.DecideApproval(ctx, previous.ID, "denied", previous.PlanHash, "ops", "new diagnosis", "web", oldTime); err != nil {
		t.Fatal(err)
	}
	run, _, err := db.RequestRun(ctx, RunRequest{IncidentID: previous.IncidentID, Mode: "full", Trigger: RunTriggerAlert, RequestedAt: oldTime})
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := db.ClaimAgentRun(ctx, run.ID, oldTime); err != nil || !claimed {
		t.Fatalf("claim=%v %v", claimed, err)
	}
	step := AgentRunStep{RunID: run.ID, Seq: 4, Kind: "guard", Name: "rules", StartedAt: oldTime, FinishedAt: &oldTime}
	if err := db.AppendRunStepRecord(ctx, RunStepRecord{Step: step}); err != nil {
		t.Fatal(err)
	}
	draft := previous
	draft.ID, draft.RunID, draft.CreatedAt, draft.ExpiresAt = 0, run.ID, now, now.Add(time.Hour)
	completion := RunCompletion{RunID: run.ID, Status: "succeeded", RCA: "new diagnosis", FinishedAt: now, Approval: &draft}
	drop := rejectInsert(t, db, "approval", fmt.Sprintf("NEW.run_id = %d", run.ID))
	if err := db.CompleteRun(ctx, completion); err == nil {
		t.Fatal("publication fault was ignored")
	}
	got, err := db.GetAgentRun(ctx, run.ID)
	if err != nil || got.Status != "running" || draft.ID != 0 {
		t.Fatalf("partial publication: run=%+v approval=%d err=%v", got, draft.ID, err)
	}
	drop()
	if n, err := db.RequeueStaleAgentRuns(ctx, now.Add(-5*time.Minute)); err != nil || n != 1 {
		t.Fatalf("recovery=%d %v", n, err)
	}
	if claimed, err := db.ClaimAgentRun(ctx, run.ID, now); err != nil || !claimed {
		t.Fatalf("reclaim=%v %v", claimed, err)
	}
	step.StartedAt, step.FinishedAt = now, &now
	if err := db.AppendRunStepRecord(ctx, RunStepRecord{Step: step}); err != nil {
		t.Fatalf("repeated phase sequence prevented recovery: %v", err)
	}
	if err := db.CompleteRun(ctx, completion); err != nil {
		t.Fatal(err)
	}
	got, err = db.GetAgentRun(ctx, run.ID)
	if err != nil || got.Status != "succeeded" || draft.ID == 0 {
		t.Fatalf("recovered publication: %+v approval=%d err=%v", got, draft.ID, err)
	}
	var count int64
	if err := db.Model(&AgentRunStep{}).Where("run_id = ? AND seq = ?", run.ID, 4).Count(&count).Error; err != nil || count != 2 {
		t.Fatalf("phase history=%d %v", count, err)
	}
}

func TestExpiredApprovalIsRejectedByCommittedClaim(t *testing.T) {
	db := openIntegrationDB(t)
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	approval, binding := executionFixture(t, db, now, false, "approved")
	row, claimed, err := db.ClaimApprovalExecution(ctx, approval.ID, approval.ExpiresAt, binding)
	if err != nil || claimed || row.Status != "expired" {
		t.Fatalf("expired claim=%+v %v %v", row, claimed, err)
	}
	var started, expired int64
	if err := db.Model(&IncidentEvent{}).Where("approval_id = ? AND event_type = ?", row.ID, "execution.started").Count(&started).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&IncidentEvent{}).Where("approval_id = ? AND event_type = ?", row.ID, "approval.expired").Count(&expired).Error; err != nil {
		t.Fatal(err)
	}
	if started != 0 || expired != 1 {
		t.Fatalf("claim facts: started=%d expired=%d", started, expired)
	}
}
