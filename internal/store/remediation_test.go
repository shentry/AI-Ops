package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"oncall-agent/internal/incident"
)

// assertClaimRefused requires the claim to expire the approval with an
// audited reason containing want.
func assertClaimRefused(t *testing.T, db *DB, approval Approval, policy RemediationPolicy, at time.Time, want string) {
	t.Helper()
	row, claimed, err := db.ClaimApprovalExecution(context.Background(), approval.ID, at, policy)
	if err != nil || claimed || row.Status != "expired" {
		t.Fatalf("claim of %d: claimed=%v status=%s err=%v; want refusal %q", approval.ID, claimed, row.Status, err, want)
	}
	var event IncidentEvent
	if err := db.Where("approval_id = ? AND event_type = ?", approval.ID, "approval.expired").First(&event).Error; err != nil || !strings.Contains(event.Summary, want) {
		t.Fatalf("refusal of %d = %q err=%v; want %q", approval.ID, event.Summary, err, want)
	}
}

func assertClaimed(t *testing.T, db *DB, approval Approval, policy RemediationPolicy, at time.Time) {
	t.Helper()
	row, claimed, err := db.ClaimApprovalExecution(context.Background(), approval.ID, at, policy)
	if err != nil || !claimed {
		var event IncidentEvent
		db.Where("approval_id = ? AND event_type = ?", approval.ID, "approval.expired").First(&event)
		t.Fatalf("claim of %d refused: status=%s reason=%q err=%v", approval.ID, row.Status, event.Summary, err)
	}
}

// passVerification completes the approval's verify phase as passed at at.
func passVerification(t *testing.T, db *DB, approvalID uint64, at time.Time) VerificationFinalization {
	t.Helper()
	task, claimed, err := db.ClaimVerificationTask(context.Background(), approvalID, at)
	if err != nil || !claimed {
		t.Fatalf("verification claim=%v %v", claimed, err)
	}
	final, err := db.FinalizeVerification(context.Background(), VerificationCompletion{ApprovalID: approvalID, ClaimedAt: *task.ClaimedAt, CheckedAt: at, Status: "passed", Observation: "healthy", Detail: "HTTP 200"})
	if err != nil || !final.Applied {
		t.Fatalf("pass=%+v %v", final, err)
	}
	return final
}

func appendControl(t *testing.T, db *DB, event ControlEvent) ControlEvent {
	t.Helper()
	saved, err := db.AppendControlEvent(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Delete(&ControlEvent{}, saved.ID) })
	return saved
}

func TestEmergencyStopRefusesQueuedActions(t *testing.T) {
	db := openIntegrationDB(t)
	t.Cleanup(func() { db.Close() })
	now := time.Now().UTC().Truncate(time.Millisecond)
	queued, policy := executionFixture(t, db, now, "approved")
	appendControl(t, db, ControlEvent{Kind: ControlEmergencyStop, Actor: "ops", Reason: "drill", CreatedAt: now})
	assertClaimRefused(t, db, queued, policy, now, "emergency stop is active: drill")
	appendControl(t, db, ControlEvent{Kind: ControlEmergencyResume, Actor: "ops", Reason: "drill over", CreatedAt: now})
	next, policy := executionFixture(t, db, now.Add(time.Millisecond), "approved")
	assertClaimed(t, db, next, policy, now)
	if _, err := db.AppendControlEvent(context.Background(), ControlEvent{Kind: ControlRuleReset, Actor: "ops", Reason: "no rule", CreatedAt: now}); err == nil {
		t.Fatal("rule reset without a rule accepted")
	}
}

// One service has one disposition at a time, and the rule budget counts real
// executions of the rule in the current window.
func TestClaimRechecksServiceMutexAndBudget(t *testing.T) {
	db := openIntegrationDB(t)
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	scope := fixtureScope{Service: "svc-mutex-" + sha256Hex(now.String())[:8], Rule: "rule-mutex-" + sha256Hex(now.String())[:8]}
	fixture := func(offset int) Approval {
		approval, _ := executionFixtureFor(t, db, now.Add(time.Duration(offset)*time.Millisecond), "approved", scope)
		return approval
	}
	policy := testPolicy(scope, incident.ModeAuto)
	policy.Budgets[scope.Rule] = RuleBudget{Max: 1, Window: time.Hour}
	first := fixture(0)
	assertClaimed(t, db, first, policy, now)
	assertClaimRefused(t, db, fixture(1), policy, now, "busy with approval")
	if err := db.FinishExecution(ctx, ExecutionCompletion{ApprovalID: first.ID, Status: "executed", ResultJSON: []byte(`{"written":true}`), FinishedAt: now}); err != nil {
		t.Fatal(err)
	}
	// Verifying still holds the service: the fault's recovery is not yet known.
	assertClaimRefused(t, db, fixture(2), policy, now, "busy with approval")
	passVerification(t, db, first.ID, now.Add(time.Second))
	assertClaimRefused(t, db, fixture(3), policy, now.Add(2*time.Second), "budget exhausted")
	policy.Budgets[scope.Rule] = RuleBudget{Max: 2, Window: time.Hour}
	assertClaimed(t, db, fixture(4), policy, now.Add(2*time.Second))
}

func TestClaimRechecksCurrentRulesAndMaintenance(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	for name, test := range map[string]struct {
		change func(*RemediationPolicy, fixtureScope)
		want   string
	}{
		"rules release changed": {func(p *RemediationPolicy, _ fixtureScope) { p.Binding.RulesVersion = "next@111111111111" }, "rules changed"},
		"rule removed":          {func(p *RemediationPolicy, s fixtureScope) { delete(p.Binding.Rules, s.Rule) }, "rules changed"},
		"rule now observe": {func(p *RemediationPolicy, s fixtureScope) {
			rule := p.Binding.Rules[s.Rule]
			rule.Mode = incident.ModeObserve
			p.Binding.Rules[s.Rule] = rule
		}, "no longer authorizes"},
		"auto rule now manual": {func(p *RemediationPolicy, s fixtureScope) {
			rule := p.Binding.Rules[s.Rule]
			rule.Mode = incident.ModeManual
			p.Binding.Rules[s.Rule] = rule
		}, "no longer authorizes"},
		"action definition changed": {func(p *RemediationPolicy, _ fixtureScope) { p.Binding.Actions["docker_restart"] = 3 }, "no longer enabled"},
		"service changed":           {func(p *RemediationPolicy, _ fixtureScope) { p.Binding.Service = "other" }, "service configuration has changed"},
		"maintenance window":        {func(p *RemediationPolicy, _ fixtureScope) { p.Maintenance = "database upgrade" }, "maintenance window: database upgrade"},
		"no budget":                 {func(p *RemediationPolicy, _ fixtureScope) { p.Budgets = nil }, "budget exhausted"},
	} {
		t.Run(name, func(t *testing.T) {
			db := openIntegrationDB(t)
			t.Cleanup(func() { db.Close() })
			approval, policy := executionFixture(t, db, now, "approved")
			test.change(&policy, scopeOf(approval))
			assertClaimRefused(t, db, approval, policy, now, test.want)
		})
	}
	// The service recovered before the claim: the old plan is never executed.
	t.Run("fault resolved before claim", func(t *testing.T) {
		db := openIntegrationDB(t)
		t.Cleanup(func() { db.Close() })
		approval, policy := executionFixture(t, db, now, "approved")
		if err := db.Model(&Incident{}).Where("id = ?", approval.IncidentID).Update("status", incident.StatusResolved).Error; err != nil {
			t.Fatal(err)
		}
		assertClaimRefused(t, db, approval, policy, now, "no longer firing")
	})
	t.Run("person approved under manual rule", func(t *testing.T) {
		db := openIntegrationDB(t)
		t.Cleanup(func() { db.Close() })
		approval, _ := executionFixture(t, db, now, "pending")
		if _, err := db.DecideApproval(context.Background(), approval.ID, "approved", approval.PlanHash, "ops", "checked", "web", now); err != nil {
			t.Fatal(err)
		}
		assertClaimed(t, db, approval, testPolicy(scopeOf(approval), incident.ModeManual), now)
	})
}

// After a primary action in an incident, a further action there needs a person.
func TestSecondAutomaticActionInIncidentIsRefused(t *testing.T) {
	db := openIntegrationDB(t)
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	first, policy := executionFixture(t, db, now, "approved")
	executeFixture(t, db, first, policy, now)
	passVerification(t, db, first.ID, now.Add(time.Second))
	publish := func(status string, at time.Time) Approval {
		t.Helper()
		run, _, err := db.RequestRun(ctx, RunRequest{IncidentID: first.IncidentID, Mode: "full", Trigger: RunTriggerAlert, RequestedAt: at})
		if err != nil {
			t.Fatal(err)
		}
		draft := first
		draft.ID, draft.RunID, draft.Status, draft.CreatedAt, draft.ResultJSON, draft.OperationID, draft.OperationStartedAt, draft.Verification = 0, run.ID, status, at, nil, nil, nil, nil
		if status == "pending" {
			draft = withSnapshot(t, draft, func(s *incident.ExecutionContext) { s.Rule.Mode = incident.ModeManual })
			draft.DecidedBy, draft.DecidedAt, draft.DecisionSource, draft.DecisionReason = nil, nil, nil, nil
		}
		if err := db.CompleteRun(ctx, RunCompletion{RunID: run.ID, Status: "succeeded", FinishedAt: at, Approval: &draft}); err != nil {
			t.Fatal(err)
		}
		return draft
	}
	at := now.Add(2 * time.Second)
	assertClaimRefused(t, db, publish("approved", at), policy, at, "already ran in this incident")
	manual := publish("pending", at.Add(time.Millisecond))
	if _, err := db.DecideApproval(ctx, manual.ID, "approved", manual.PlanHash, "ops", "second action", "web", at); err != nil {
		t.Fatal(err)
	}
	assertClaimed(t, db, manual, policy, at)
}

// A failed recovery blocks the rule's automatic primary actions until an
// operator resets it; a person may still approve one.
func TestFailedRecoveryBlocksRuleUntilReset(t *testing.T) {
	db := openIntegrationDB(t)
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	scope := fixtureScope{Service: "svc-block-" + sha256Hex(now.String())[:8], Rule: "rule-block-" + sha256Hex(now.String())[:8]}
	policy := testPolicy(scope, incident.ModeAuto)
	failed, _ := executionFixtureFor(t, db, now, "approved", scope)
	executeFixture(t, db, failed, policy, now)
	completion := failedVerificationCompletion(t, db, failed.ID)
	completion.Retry = false
	if final, err := db.FinalizeVerification(ctx, completion); err != nil || final.Status != "failed" {
		t.Fatalf("failed verification=%+v %v", final, err)
	}
	after := completion.CheckedAt.Add(time.Second)
	state, err := db.RemediationState(ctx, RemediationQuery{Service: scope.Service, RuleID: scope.Rule, Since: now.Add(-time.Hour)})
	if err != nil || !strings.Contains(state.Blocked, "verify.failed") || state.BusyWith != 0 || state.Executions != 1 {
		t.Fatalf("state=%+v err=%v", state, err)
	}
	blocked, _ := executionFixtureFor(t, db, now.Add(time.Millisecond), "approved", scope)
	assertClaimRefused(t, db, blocked, policy, after, "is blocked: verify.failed")

	manual, _ := executionFixtureFor(t, db, now.Add(2*time.Millisecond), "pending", scope)
	if _, err := db.DecideApproval(ctx, manual.ID, "approved", manual.PlanHash, "ops", "cause understood", "web", after); err != nil {
		t.Fatal(err)
	}
	executeFixture(t, db, manual, policy, after)
	passVerification(t, db, manual.ID, after.Add(time.Second))

	rule := scope.Rule
	appendControl(t, db, ControlEvent{Kind: ControlRuleReset, RuleID: &rule, Actor: "ops", Reason: "bad upstream replaced", CreatedAt: after.Add(2 * time.Second)})
	if state, err := db.RemediationState(ctx, RemediationQuery{RuleID: scope.Rule, Since: now.Add(-time.Hour)}); err != nil || state.Blocked != "" {
		t.Fatalf("reset state=%+v err=%v", state, err)
	}
	reset, _ := executionFixtureFor(t, db, now.Add(3*time.Millisecond), "approved", scope)
	assertClaimed(t, db, reset, policy, after.Add(3*time.Second))
}

// A failed recovery with a frozen compensation queues the undo in the same
// transaction; it runs under the parent's authority even while the rule is
// blocked or a maintenance window is open, and it holds the service.
func TestCompensationQueuedWithFailedVerification(t *testing.T) {
	db := openIntegrationDB(t)
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	approval, policy := executionFixture(t, db, now, "approved")
	resnapshot(t, db, &approval, func(s *incident.ExecutionContext) {
		s.Compensation = &incident.Compensation{Action: "upstream_restore", ActionVersion: 1, Args: json.RawMessage(`{"account_id":7}`), Revision: "schedulable=false",
			Checks: []incident.Check{{Kind: incident.CheckAccount, Params: json.RawMessage(`{"account_id":7,"schedulable":true}`)}}}
	})
	executeFixture(t, db, approval, policy, now)
	completion := failedVerificationCompletion(t, db, approval.ID)
	completion.Retry = false
	final, err := db.FinalizeVerification(ctx, completion)
	if err != nil || final.Status != "failed" || final.CompensationID == 0 {
		t.Fatalf("final=%+v err=%v", final, err)
	}
	undo, err := db.GetApproval(ctx, final.CompensationID)
	if err != nil || undo.Status != "approved" || undo.ParentApprovalID == nil || *undo.ParentApprovalID != approval.ID || undo.ToolName != "upstream_restore" || *undo.DecidedBy != "system:compensation" {
		t.Fatalf("compensation=%+v err=%v", undo, err)
	}
	snapshot, err := incident.ParseExecutionContext(undo.ExecutionContext)
	if err != nil || snapshot.Kind != incident.KindCompensation || snapshot.Compensation != nil || !undo.ExpiresAt.Equal(completion.CheckedAt.Add(incident.CompensationTTL)) {
		t.Fatalf("compensation snapshot=%+v err=%v", snapshot, err)
	}
	state, err := db.RemediationState(ctx, RemediationQuery{Service: *approval.Service})
	if err != nil || state.BusyWith != undo.ID {
		t.Fatalf("queued compensation must hold the service: %+v %v", state, err)
	}
	at := completion.CheckedAt.Add(time.Second)
	policy.Maintenance = "maintenance"
	assertClaimed(t, db, undo, policy, at)
	if err := db.FinishExecution(ctx, ExecutionCompletion{ApprovalID: undo.ID, Status: "executed", ResultJSON: []byte(`{"written":true}`), FinishedAt: at}); err != nil {
		t.Fatal(err)
	}
	passVerification(t, db, undo.ID, at.Add(time.Second))
	assertScopeRaceCount(t, db, &AgentRunStep{}, "run_id = ? AND seq = ?", []any{undo.RunID, 92}, 1)
	assertScopeRaceCount(t, db, &FaultCmdHistory{}, "approval_id = ?", []any{undo.ID}, 0)
	assertScopeRaceCount(t, db, &IncidentEvent{}, "approval_id = ? AND event_type = ?", []any{approval.ID, "compensation.queued"}, 1)
}

// A recovery is reusable memory only after the watch window ends healthy; the
// same number of consecutive unhealthy observations is a recurrence.
func TestWatchPhaseDecidesStability(t *testing.T) {
	for _, outcome := range []string{"stable", "recurred"} {
		t.Run(outcome, func(t *testing.T) {
			db := openIntegrationDB(t)
			t.Cleanup(func() { db.Close() })
			ctx := context.Background()
			now := time.Now().UTC().Truncate(time.Millisecond)
			approval, policy := executionFixture(t, db, now, "approved")
			resnapshot(t, db, &approval, func(s *incident.ExecutionContext) { s.Verification.WatchSeconds = 60 })
			executeFixture(t, db, approval, policy, now)
			parent, err := db.GetIncident(ctx, approval.IncidentID)
			if err != nil {
				t.Fatal(err)
			}
			fp := incident.FaultFingerprint(parent.GroupKey, testAlert)
			memory := FaultMemory{Fingerprint: fp, GroupKey: parent.GroupKey, AlertName: testAlert, PlanJSON: []byte(`{"action":"docker_restart"}`), Confidence: "high", TTLSeconds: 3600}

			passedAt := now.Add(time.Second)
			task, _, err := db.ClaimVerificationTask(ctx, approval.ID, passedAt)
			if err != nil {
				t.Fatal(err)
			}
			final, err := db.FinalizeVerification(ctx, VerificationCompletion{ApprovalID: approval.ID, ClaimedAt: *task.ClaimedAt, CheckedAt: passedAt, Status: "passed", Observation: "healthy", Detail: "HTTP 200", Memory: &memory})
			if err != nil || final.Status != "passed" || final.Phase != "watch" {
				t.Fatalf("passed=%+v err=%v", final, err)
			}
			if _, err := db.GetFaultMemory(ctx, fp); !errors.Is(err, ErrMemoryNotFound) {
				t.Fatalf("memory written before the watch ended: %v", err)
			}
			if state, err := db.RemediationState(ctx, RemediationQuery{Service: *approval.Service}); err != nil || state.BusyWith != 0 {
				t.Fatalf("a watched recovery must not hold the service: %+v %v", state, err)
			}
			got, err := db.GetApproval(ctx, approval.ID)
			if err != nil || got.Verification.Phase != "watch" || got.Verification.Status != "pending" || !got.Verification.DeadlineAt.Equal(passedAt.Add(time.Minute)) {
				t.Fatalf("watch task=%+v err=%v", got.Verification, err)
			}

			if _, created, err := db.RequestRun(ctx, RunRequest{IncidentID: approval.IncidentID, Mode: "full", Trigger: RunTriggerAlert, RequestedAt: passedAt}); err != nil || !created {
				t.Fatalf("the passive watch must not block diagnosis: created=%v err=%v", created, err)
			}
			watchAt := passedAt.Add(10 * time.Second)
			task, _, err = db.ClaimVerificationTask(ctx, approval.ID, watchAt)
			if err != nil {
				t.Fatal(err)
			}
			if outcome == "recurred" {
				final, err = db.FinalizeVerification(ctx, VerificationCompletion{ApprovalID: approval.ID, ClaimedAt: *task.ClaimedAt, CheckedAt: watchAt, Status: "recurred", Observation: "unhealthy", Detail: "HTTP 503"})
				if err != nil || final.Status != "recurred" || !final.Escalated {
					t.Fatalf("recurred=%+v err=%v", final, err)
				}
				assertScopeRaceCount(t, db, &IncidentProblem{}, "incident_id = ? AND code = ? AND status = ?", []any{approval.IncidentID, "recurred", "open"}, 1)
				state, err := db.RemediationState(ctx, RemediationQuery{RuleID: *approval.RuleID, Since: now.Add(-time.Hour)})
				if err != nil || !strings.Contains(state.Blocked, "verify.recurred") {
					t.Fatalf("recurrence must block the rule: %+v %v", state, err)
				}
				if _, err := db.GetFaultMemory(ctx, fp); !errors.Is(err, ErrMemoryNotFound) {
					t.Fatalf("recurred fix became memory: %v", err)
				}
				return
			}
			if final, err = db.FinalizeVerification(ctx, VerificationCompletion{ApprovalID: approval.ID, ClaimedAt: *task.ClaimedAt, CheckedAt: watchAt, Status: "stable", Observation: "healthy", Detail: "HTTP 200"}); err == nil {
				t.Fatalf("stable before the watch ended: %+v", final)
			}
			for watchAt.Before(passedAt.Add(time.Minute)) {
				next := watchAt.Add(10 * time.Second)
				if final, err = db.FinalizeVerification(ctx, VerificationCompletion{ApprovalID: approval.ID, ClaimedAt: *task.ClaimedAt, CheckedAt: watchAt, Status: "pending", NextCheckAt: next, Observation: "healthy", Detail: "HTTP 200"}); err != nil || final.Status != "pending" {
					t.Fatalf("watch pending=%+v err=%v", final, err)
				}
				watchAt = next
				if watchAt.Before(passedAt.Add(time.Minute)) {
					task, _, err = db.ClaimVerificationTask(ctx, approval.ID, watchAt)
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			endAt := passedAt.Add(time.Minute)
			task, _, err = db.ClaimVerificationTask(ctx, approval.ID, endAt)
			if err != nil {
				t.Fatal(err)
			}
			final, err = db.FinalizeVerification(ctx, VerificationCompletion{ApprovalID: approval.ID, ClaimedAt: *task.ClaimedAt, CheckedAt: endAt, Status: "stable", Observation: "healthy", Detail: "watch ended", Memory: &memory})
			if err != nil || final.Status != "stable" || final.Phase != "watch" {
				t.Fatalf("stable=%+v err=%v", final, err)
			}
			if entry, err := db.GetFaultMemory(ctx, fp); err != nil || entry.Confidence != "high" {
				t.Fatalf("stable memory=%+v err=%v", entry, err)
			}
			assertScopeRaceCount(t, db, &AgentRunStep{}, "run_id = ? AND seq = ?", []any{approval.RunID, 91}, 1)
		})
	}
}

func TestExecutorLockIsExclusive(t *testing.T) {
	db := openIntegrationDB(t)
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	name := "oncall-test-" + sha256Hex(t.Name() + time.Now().String())[:12]
	release, err := db.AcquireExecutorLock(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AcquireExecutorLock(ctx, name); err == nil {
		t.Fatal("second executor acquired the lock")
	}
	release()
	release, err = db.AcquireExecutorLock(ctx, name)
	if err != nil {
		t.Fatalf("lock not released: %v", err)
	}
	release()
}

func TestRecordRulesReleaseOncePerRelease(t *testing.T) {
	db := openIntegrationDB(t)
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	first, second := "test-a@"+sha256Hex(now.String())[:12], "test-b@"+sha256Hex(now.String())[:12]
	t.Cleanup(func() {
		db.Where("kind = ? AND reason IN ?", ControlRulesLoaded, []string{first, second}).Delete(&ControlEvent{})
	})
	for _, release := range []string{first, first, second} {
		if err := db.RecordRulesRelease(ctx, release, []string{"rule"}, now); err != nil {
			t.Fatal(err)
		}
	}
	var count int64
	if err := db.Model(&ControlEvent{}).Where("kind = ? AND reason IN ?", ControlRulesLoaded, []string{first, second}).Count(&count).Error; err != nil || count != 2 {
		t.Fatalf("rules releases recorded=%d err=%v; want one per distinct release", count, err)
	}
}
