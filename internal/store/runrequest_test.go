package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRequestRunHasOneAdmissionPath(t *testing.T) {
	db := openIntegrationDB(t)
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	parent := insertTestIncident(t, db, now, "admission")
	t.Cleanup(func() { db.Where("incident_id = ?", parent.ID).Delete(&AgentRun{}) })
	req := RunRequest{IncidentID: parent.ID, Mode: "full", Trigger: RunTriggerManual, RequestedAt: now}
	run, created, err := db.RequestRun(ctx, req)
	if err != nil || !created {
		t.Fatalf("enqueue: %v %v", created, err)
	}
	_, _, err = db.RequestRun(ctx, req)
	var refusal *RunAdmissionError
	if !errors.As(err, &refusal) || refusal.Code != "active_processing" || refusal.RunID != run.ID {
		t.Fatalf("duplicate: %v", err)
	}
	if err := db.CompleteRun(ctx, RunCompletion{RunID: run.ID, Status: "succeeded", FinishedAt: now.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	req.RequestedAt = now.Add(2 * time.Second)
	_, _, err = db.RequestRun(ctx, req)
	if !errors.As(err, &refusal) || refusal.Code != "cooldown" || refusal.RetryAfterSeconds != 58 {
		t.Fatalf("cooldown: %#v %v", refusal, err)
	}
	req.RequestedAt = now.Add(time.Minute)
	if _, created, err := db.RequestRun(ctx, req); err != nil || !created {
		t.Fatalf("after cooldown: %v %v", created, err)
	}
}

func TestConcurrentManualAndRetryAdmissionCreatesOneCycle(t *testing.T) {
	db := openIntegrationDB(t)
	t.Cleanup(func() { db.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	now := time.Now().UTC().Truncate(time.Millisecond)
	parent := insertTestIncident(t, db, now.Add(-2*time.Minute), "concurrent-admission")
	t.Cleanup(func() { db.Where("incident_id = ?", parent.ID).Delete(&AgentRun{}) })
	root, _, err := db.RequestRun(ctx, RunRequest{IncidentID: parent.ID, Mode: "full", Trigger: RunTriggerAlert, RequestedAt: now.Add(-2 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteRun(ctx, RunCompletion{RunID: root.ID, Status: "failed", FinishedAt: now.Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		created bool
		err     error
	}
	results := make(chan outcome, 20)
	start := make(chan struct{})
	for i := range 20 {
		go func(index int) {
			<-start
			request := RunRequest{IncidentID: parent.ID, Mode: "full", Trigger: RunTriggerManual, RequestedAt: now}
			if index%2 == 0 {
				request.Trigger, request.RetryOf = RunTriggerRetry, &root.ID
			}
			_, created, err := db.RequestRun(ctx, request)
			results <- outcome{created, err}
		}(i)
	}
	close(start)
	winners := 0
	for range 20 {
		result := <-results
		if result.created {
			winners++
		}
		if result.err != nil {
			var refusal *RunAdmissionError
			if !errors.As(result.err, &refusal) || refusal.Code != "active_processing" {
				t.Errorf("unexpected refusal: %v", result.err)
			}
		}
	}
	var count int64
	if err := db.Model(&AgentRun{}).Where("incident_id = ?", parent.ID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if winners != 1 || count != 2 {
		t.Fatalf("winners=%d total runs=%d, want one new cycle", winners, count)
	}
}

func TestAdmissionExcludesApprovalAndVerification(t *testing.T) {
	db := openIntegrationDB(t)
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	approval, policy := executionFixture(t, db, now, "pending")
	request := RunRequest{IncidentID: approval.IncidentID, Mode: "full", Trigger: RunTriggerManual, RequestedAt: now.Add(time.Minute)}
	assertBlocked := func() {
		t.Helper()
		_, created, err := db.RequestRun(ctx, request)
		var refusal *RunAdmissionError
		if created || !errors.As(err, &refusal) || refusal.Code != "active_processing" || refusal.ApprovalID != approval.ID {
			t.Fatalf("blocked=%v %+v %v", created, refusal, err)
		}
	}
	assertBlocked()
	if _, err := db.DecideApproval(ctx, approval.ID, "approved", approval.PlanHash, "ops", "approved", "web", now); err != nil {
		t.Fatal(err)
	}
	assertBlocked()
	if _, claimed, err := db.ClaimApprovalExecution(ctx, approval.ID, now, policy); err != nil || !claimed {
		t.Fatalf("claim=%v %v", claimed, err)
	}
	assertBlocked()
	if err := db.FinishExecution(ctx, ExecutionCompletion{ApprovalID: approval.ID, Status: "executed", ResultJSON: []byte(`{"output":"ok"}`), FinishedAt: now}); err != nil {
		t.Fatal(err)
	}
	assertBlocked()
	task, claimed, err := db.ClaimVerificationTask(ctx, approval.ID, now)
	if err != nil || !claimed {
		t.Fatalf("verify claim=%v %v", claimed, err)
	}
	assertBlocked()
	if _, err := db.FinalizeVerification(ctx, VerificationCompletion{ApprovalID: approval.ID, ClaimedAt: *task.ClaimedAt, CheckedAt: now.Add(time.Second), Status: "passed", Observation: "healthy"}); err != nil {
		t.Fatal(err)
	}
	if _, created, err := db.RequestRun(ctx, request); err != nil || !created {
		t.Fatalf("terminal did not release admission: %v %v", created, err)
	}
}

func TestRetryMissingParentDoesNotStartNewBudget(t *testing.T) {
	db := openIntegrationDB(t)
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	parent := insertTestIncident(t, db, now, "missing-chain")
	missing := uint64(1 << 62)
	_, created, err := db.RequestRun(ctx, RunRequest{IncidentID: parent.ID, Mode: "full", Trigger: RunTriggerRetry, RetryOf: &missing, RequestedAt: now})
	var refusal *RunAdmissionError
	if created || !errors.As(err, &refusal) || refusal.Code != "retry_chain" {
		t.Fatalf("broken chain=%v %+v %v", created, refusal, err)
	}
	var count int64
	if err := db.Model(&AgentRun{}).Where("incident_id = ?", parent.ID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("broken chain wrote %d runs", count)
	}
}
