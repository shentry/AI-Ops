package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"oncall-agent/internal/incident"
)

// Persist a fresh unhealthy probe before expiry, then claim the expiry decision.
func failedVerificationCompletion(t *testing.T, db *DB, approvalID uint64) VerificationCompletion {
	t.Helper()
	ctx := context.Background()
	approval, err := db.GetApproval(ctx, approvalID)
	if err != nil || approval.Verification == nil {
		t.Fatalf("approval=%+v %v", approval, err)
	}
	deadline := approval.Verification.DeadlineAt
	lastCheck := deadline.Add(-time.Second)
	task, claimed, err := db.ClaimVerificationTask(ctx, approvalID, lastCheck)
	if err != nil || !claimed {
		t.Fatalf("probe claim=%v %v", claimed, err)
	}
	pending := VerificationCompletion{ApprovalID: approvalID, ClaimedAt: *task.ClaimedAt, CheckedAt: lastCheck, Status: "pending", NextCheckAt: deadline, Observation: "unhealthy", Detail: "HTTP 503"}
	if result, err := db.FinalizeVerification(ctx, pending); err != nil || !result.Applied || result.Status != "pending" {
		t.Fatalf("probe=%+v %v", result, err)
	}
	task, claimed, err = db.ClaimVerificationTask(ctx, approvalID, deadline)
	if err != nil || !claimed {
		t.Fatalf("expiry claim=%v %v", claimed, err)
	}
	return VerificationCompletion{ApprovalID: approvalID, ClaimedAt: *task.ClaimedAt, CheckedAt: deadline, Status: "failed", Observation: "unhealthy", Detail: "HTTP 503", Retry: true}
}

func TestVerificationRetryAndMemoryDemotionAreOneTransaction(t *testing.T) {
	db := openIntegrationDB(t)
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	approval, _, _ := verificationFixture(t, db, now)
	// Seed the recorded source as an existing successful memory-hit diagnosis.
	if err := db.Model(&AgentRun{}).Where("id = ?", approval.RunID).Update("mode", "memory_hit").Error; err != nil {
		t.Fatal(err)
	}
	parent, err := db.GetIncident(ctx, approval.IncidentID)
	if err != nil {
		t.Fatal(err)
	}
	run, err := db.GetAgentRun(ctx, approval.RunID)
	if err != nil {
		t.Fatal(err)
	}
	fp := incident.FaultFingerprint(parent.GroupKey, testAlert)
	if err := db.UpsertFaultMemory(ctx, FaultMemory{Fingerprint: fp, GroupKey: parent.GroupKey, AlertName: testAlert, PlanJSON: *run.PlanJSON, Confidence: "high", FirstSeen: now, LastSuccess: now, TTLSeconds: 3600}); err != nil {
		t.Fatal(err)
	}
	completion := failedVerificationCompletion(t, db, approval.ID)
	completion.DemoteFingerprint = fp
	before, err := db.ListIncidentEvents(ctx, approval.IncidentID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	drop := rejectInsert(t, db, "agent_run", fmt.Sprintf("NEW.incident_id = %d AND NEW.retry_of = %d", approval.IncidentID, approval.RunID))
	if _, err := db.FinalizeVerification(ctx, completion); err == nil || !strings.Contains(err.Error(), "oncall injected failure") {
		t.Fatalf("expected retry insert failure: %v", err)
	}
	got, err := db.GetApproval(ctx, approval.ID)
	if err != nil || got.Verification.Status != "running" {
		t.Fatalf("partial task=%+v %v", got, err)
	}
	entry, err := db.GetFaultMemory(ctx, fp)
	if err != nil || entry.Confidence != "high" {
		t.Fatalf("partial memory=%+v %v", entry, err)
	}
	var retries int64
	if err := db.Model(&AgentRun{}).Where("retry_of = ?", approval.RunID).Count(&retries).Error; err != nil || retries != 0 {
		t.Fatalf("partial retry=%d %v", retries, err)
	}
	after, err := db.ListIncidentEvents(ctx, approval.IncidentID, 0, 100)
	if err != nil || len(after) != len(before) {
		t.Fatalf("partial audit=%d vs %d %v", len(after), len(before), err)
	}
	var problems int64
	if err := db.Model(&IncidentProblem{}).Where("incident_id = ?", approval.IncidentID).Count(&problems).Error; err != nil || problems != 0 {
		t.Fatalf("partial problem=%d %v", problems, err)
	}
	drop()
	result, err := db.FinalizeVerification(ctx, completion)
	if err != nil || !result.Applied || result.RetryRunID == 0 || result.Escalated {
		t.Fatalf("retry=%+v %v", result, err)
	}
	entry, err = db.GetFaultMemory(ctx, fp)
	if err != nil || entry.Confidence != "low" {
		t.Fatalf("demotion=%+v %v", entry, err)
	}
	child, err := db.GetAgentRun(ctx, result.RetryRunID)
	if err != nil || child.RetryOf == nil || *child.RetryOf != approval.RunID || child.Status != "pending" {
		t.Fatalf("child=%+v %v", child, err)
	}
	if again, err := db.FinalizeVerification(ctx, completion); err != nil || again.Applied {
		t.Fatalf("duplicate=%+v %v", again, err)
	}
	if err := db.Model(&AgentRun{}).Where("retry_of = ?", approval.RunID).Count(&retries).Error; err != nil || retries != 1 {
		t.Fatalf("duplicate retry=%d %v", retries, err)
	}
}

func TestVerificationRetryBudgetEndsInDurableEscalation(t *testing.T) {
	db := openIntegrationDB(t)
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	current, policy, _ := verificationFixture(t, db, now)
	incidentID := current.IncidentID
	for attempt := range 3 {
		completion := failedVerificationCompletion(t, db, current.ID)
		result, err := db.FinalizeVerification(ctx, completion)
		if err != nil || !result.Applied || result.Status != "failed" {
			t.Fatalf("attempt %d=%+v %v", attempt, result, err)
		}
		if attempt == 2 {
			if result.RetryRunID != 0 || !result.Escalated {
				t.Fatalf("budget not enforced: %+v", result)
			}
			break
		}
		if result.RetryRunID == 0 || result.Escalated {
			t.Fatalf("retry missing: %+v", result)
		}
		run, err := db.GetAgentRun(ctx, result.RetryRunID)
		if err != nil || run.RetryOf == nil || *run.RetryOf != current.RunID {
			t.Fatalf("chain=%+v %v", run, err)
		}
		at := completion.CheckedAt.Add(time.Second)
		// The failed verification blocked the rule's automatic actions, so the
		// retry's action waits for a person.
		draft := withSnapshot(t, Approval{IncidentID: incidentID, RunID: run.ID, Service: current.Service, RuleID: current.RuleID, ToolName: current.ToolName, ArgsJSON: current.ArgsJSON, ExecutionContext: current.ExecutionContext, Reason: "manual retry approval", Status: "pending", CreatedAt: at},
			func(s *incident.ExecutionContext) { s.Rule.Mode, s.ExpiresAt = incident.ModeManual, at.Add(time.Hour) })
		if err := db.CompleteRun(ctx, RunCompletion{RunID: run.ID, Status: "succeeded", PlanJSON: []byte(`{"action":"docker_restart","confidence":"high"}`), FinishedAt: at, Approval: &draft}); err != nil {
			t.Fatal(err)
		}
		if _, err := db.DecideApproval(ctx, draft.ID, "approved", draft.PlanHash, "ops", "retry", "web", at); err != nil {
			t.Fatal(err)
		}
		if _, claimed, err := db.ClaimApprovalExecution(ctx, draft.ID, at, policy); err != nil || !claimed {
			t.Fatalf("execution claim=%v %v", claimed, err)
		}
		if err := db.FinishExecution(ctx, ExecutionCompletion{ApprovalID: draft.ID, Status: "executed", ResultJSON: []byte(`{"output":"ok"}`), FinishedAt: at}); err != nil {
			t.Fatal(err)
		}
		current = draft
	}
	var runCount int64
	if err := db.Model(&AgentRun{}).Where("incident_id = ?", incidentID).Count(&runCount).Error; err != nil || runCount != 3 {
		t.Fatalf("runs=%d %v", runCount, err)
	}
	var problem IncidentProblem
	if err := db.Where("incident_id = ? AND code = ? AND status = ?", incidentID, "manual_check", "open").First(&problem).Error; err != nil || !strings.Contains(problem.Summary, "retry_budget") {
		t.Fatalf("manual problem=%+v %v", problem, err)
	}
	var escalations int64
	if err := db.Model(&IncidentEvent{}).Where("incident_id = ? AND event_type = ?", incidentID, "escalation.required").Count(&escalations).Error; err != nil || escalations != 1 {
		t.Fatalf("escalations=%d %v", escalations, err)
	}
}
