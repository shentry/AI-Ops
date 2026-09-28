package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"gorm.io/datatypes"
	"oncall-agent/internal/incident"
)

const (
	testAlert       = "Sub2APIDown"
	testRuleVersion = "test@000000000000"
)

// fixtureScope names the service and rule a fixture authorizes.
type fixtureScope struct {
	Service string
	Rule    string
}

// executionFixture publishes a restart snapshot through a real diagnosis
// completion. Each fixture has its own service and rule, so the service
// mutex, budgets and rule blocks of one test never leak into another.
// status approved means an auto rule approved it; pending means manual.
func executionFixture(t *testing.T, db *DB, now time.Time, status string) (Approval, RemediationPolicy) {
	t.Helper()
	suffix := sha256Hex(t.Name() + now.Format(time.RFC3339Nano))[:10]
	return executionFixtureFor(t, db, now, status, fixtureScope{Service: "svc-" + suffix, Rule: "rule-" + suffix})
}

func executionFixtureFor(t *testing.T, db *DB, now time.Time, status string, scope fixtureScope) (Approval, RemediationPolicy) {
	t.Helper()
	parent := insertTestIncident(t, db, now, "execution-trust")
	fp := sha256Hex(parent.GroupKey)
	labels, _ := json.Marshal(map[string]string{"service": scope.Service, "container": "sub2api"})
	alert := Alert{Fingerprint: fp, AlertHash: md5Hex(fp), Source: "alertmanager", Name: testAlert, Severity: 5, Status: "firing", Labels: labels, Annotations: datatypes.JSON(`{}`), StartsAt: now, ReceivedAt: now}
	if err := db.Create(&alert).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&LastAlert{Fingerprint: fp, AlertID: alert.ID, AlertHash: alert.AlertHash, Status: "firing", Severity: 5, FirstSeen: now, LastSeen: now, IncidentID: &parent.ID}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&IncidentAlert{IncidentID: parent.ID, Fingerprint: fp, LinkedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Exec("DELETE FROM verify_task WHERE approval_id IN (SELECT id FROM approval WHERE incident_id = ?)", parent.ID)
		db.Exec("DELETE FROM agent_run_step WHERE run_id IN (SELECT id FROM agent_run WHERE incident_id = ?)", parent.ID)
		db.Exec("DELETE FROM fault_cmd_history WHERE approval_id IN (SELECT id FROM approval WHERE incident_id = ?)", parent.ID)
		db.Where("incident_id = ?", parent.ID).Delete(&Review{})
		db.Where("incident_id = ?", parent.ID).Delete(&Approval{})
		db.Where("incident_id = ?", parent.ID).Delete(&AgentRun{})
		db.Where("incident_id = ?", parent.ID).Delete(&IncidentAlert{})
		db.Where("fingerprint = ?", fp).Delete(&LastAlert{})
		db.Where("id = ?", alert.ID).Delete(&Alert{})
		db.Where("fingerprint = ?", incident.FaultFingerprint(parent.GroupKey, testAlert)).Delete(&FaultMemory{})
		db.Where("service = ?", scope.Service).Delete(&ChangeEvent{})
		db.Where("service = ?", scope.Service).Delete(&ServiceLock{})
		db.Where("rule_id = ?", scope.Rule).Delete(&ControlEvent{})
	})
	run, _, err := db.RequestRun(context.Background(), RunRequest{IncidentID: parent.ID, Mode: "full", Trigger: RunTriggerAlert, RequestedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	mode := incident.ModeManual
	if status == "approved" {
		mode = incident.ModeAuto
	}
	expires := now.Add(time.Hour)
	snapshot := incident.ExecutionContext{
		Version: incident.ExecutionContextVersion, Kind: incident.KindPrimary, Service: scope.Service,
		Rule:          incident.RuleRef{ID: scope.Rule, Version: testRuleVersion, Mode: mode, Alerts: []string{testAlert}},
		ActionVersion: 2, Target: incident.Object{Kind: "container", Name: "sub2api", ID: "c0ffee"},
		Revision: "started_at=" + now.Format(time.RFC3339Nano), PreState: json.RawMessage(`{"status":"running"}`),
		EvidenceRefs: []string{"docker_inspect"}, Members: []string{fp}, FaultAlert: testAlert,
		Verification: incident.VerificationSpec{Checks: []incident.Check{{Kind: incident.CheckHealth, Params: json.RawMessage(`{"base_url":"http://127.0.0.1:8080"}`)}},
			IntervalSeconds: 10, WindowSeconds: 120, TimeoutSeconds: 5, RequiredPasses: 1},
		ExpiresAt: expires,
	}
	raw, _ := json.Marshal(snapshot)
	args := datatypes.JSON(`{"target_kind":"container","target_name":"sub2api"}`)
	hash, err := incident.PlanHash("docker_restart", args, raw)
	if err != nil {
		t.Fatal(err)
	}
	service, rule := scope.Service, scope.Rule
	draft := Approval{IncidentID: parent.ID, RunID: run.ID, Service: &service, RuleID: &rule, ToolName: "docker_restart", ArgsJSON: args, ExecutionContext: datatypes.JSON(raw), PlanHash: hash, Reason: "test approval snapshot", Status: status, ExpiresAt: expires, CreatedAt: now}
	if status == "approved" {
		actor, source := "system:rule:"+rule, "rule"
		draft.DecidedBy, draft.DecisionSource, draft.DecidedAt = &actor, &source, &now
	}
	guard := datatypes.JSON(`"decision=allow overridden=false reason="`)
	completion := RunCompletion{RunID: run.ID, Status: "succeeded", RCA: "supported outage", PlanJSON: []byte(`{"action":"docker_restart","confidence":"high"}`), FinishedAt: now, Approval: &draft, Steps: []AgentRunStep{{RunID: run.ID, Seq: 4, Kind: "guard", Name: "rules", OutputJSON: &guard, StartedAt: now, FinishedAt: &now}}}
	if err := db.CompleteRun(context.Background(), completion); err != nil {
		t.Fatal(err)
	}
	if draft.ID == 0 {
		t.Fatal("diagnosis completion did not publish its approval")
	}
	return draft, testPolicy(scope, incident.ModeAuto)
}

// testPolicy is the current configuration authorizing a fixture's rule.
func testPolicy(scope fixtureScope, mode string) RemediationPolicy {
	return RemediationPolicy{
		Binding: incident.ExecutionBinding{Service: scope.Service, RulesVersion: testRuleVersion,
			Rules:   map[string]incident.RuleRef{scope.Rule: {ID: scope.Rule, Version: testRuleVersion, Mode: mode, Alerts: []string{testAlert}}},
			Actions: map[string]int{"docker_restart": 2, "upstream_restore": 1}},
		Budgets: map[string]RuleBudget{scope.Rule: {Max: 10, Window: time.Hour}},
	}
}

func scopeOf(approval Approval) fixtureScope {
	return fixtureScope{Service: *approval.Service, Rule: *approval.RuleID}
}

// withSnapshot returns approval with its snapshot changed and its hash and
// expiry kept consistent, as if the policy had frozen different content.
func withSnapshot(t *testing.T, approval Approval, mutate func(*incident.ExecutionContext)) Approval {
	t.Helper()
	snapshot, err := incident.ParseExecutionContext(approval.ExecutionContext)
	if err != nil {
		t.Fatal(err)
	}
	mutate(&snapshot)
	raw, _ := json.Marshal(snapshot)
	hash, err := incident.PlanHash(approval.ToolName, approval.ArgsJSON, raw)
	if err != nil {
		t.Fatal(err)
	}
	approval.ExecutionContext, approval.PlanHash, approval.ExpiresAt = datatypes.JSON(raw), hash, snapshot.ExpiresAt
	return approval
}

// resnapshot applies withSnapshot to a stored approval.
func resnapshot(t *testing.T, db *DB, approval *Approval, mutate func(*incident.ExecutionContext)) {
	t.Helper()
	*approval = withSnapshot(t, *approval, mutate)
	if err := db.Model(&Approval{}).Where("id = ?", approval.ID).Updates(map[string]any{"execution_context": approval.ExecutionContext, "plan_hash": approval.PlanHash, "expires_at": approval.ExpiresAt}).Error; err != nil {
		t.Fatal(err)
	}
}

// executeFixture claims and completes an approval as executed.
func executeFixture(t *testing.T, db *DB, approval Approval, policy RemediationPolicy, at time.Time) {
	t.Helper()
	if row, claimed, err := db.ClaimApprovalExecution(context.Background(), approval.ID, at, policy); err != nil || !claimed {
		t.Fatalf("claim=%v row=%+v err=%v", claimed, row, err)
	}
	if err := db.FinishExecution(context.Background(), ExecutionCompletion{ApprovalID: approval.ID, Status: "executed", ResultJSON: []byte(`{"written":true,"detail":"restarted"}`), FinishedAt: at}); err != nil {
		t.Fatal(err)
	}
}

func TestDiagnosisPublishesApprovalWithResult(t *testing.T) {
	db := openIntegrationDB(t)
	t.Cleanup(func() { db.Close() })
	draft, _ := executionFixture(t, db, time.Now().UTC().Truncate(time.Millisecond), "approved")
	approval, err := db.GetApproval(context.Background(), draft.ID)
	if err != nil || approval.Status != "approved" || approval.PlanHash != draft.PlanHash || *approval.RuleID != *draft.RuleID || *approval.Service != *draft.Service {
		t.Fatalf("approval=%+v err=%v", approval, err)
	}
	run, err := db.GetAgentRun(context.Background(), draft.RunID)
	if err != nil || run.Status != "succeeded" || run.PlanJSON == nil {
		t.Fatalf("run=%+v err=%v", run, err)
	}
}

// Only an auto rule approves a primary action without a person, and the
// indexed columns must mirror the hashed snapshot.
func TestPublicationRequiresRuleDecisionAndMirroredColumns(t *testing.T) {
	db := openIntegrationDB(t)
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	previous, _ := executionFixture(t, db, now, "pending")
	if _, err := db.DecideApproval(ctx, previous.ID, "denied", previous.PlanHash, "ops", "redo", "web", now); err != nil {
		t.Fatal(err)
	}
	run, _, err := db.RequestRun(ctx, RunRequest{IncidentID: previous.IncidentID, Mode: "full", Trigger: RunTriggerAlert, RequestedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Approval){
		"manual rule auto-approved": func(a *Approval) {
			actor, source := "system:rule:"+*a.RuleID, "rule"
			a.Status, a.DecidedBy, a.DecisionSource, a.DecidedAt = "approved", &actor, &source, &now
		},
		"rule column differs": func(a *Approval) { other := "other"; a.RuleID = &other },
		"service missing":     func(a *Approval) { a.Service = nil },
		"expiry differs":      func(a *Approval) { a.ExpiresAt = a.ExpiresAt.Add(time.Minute) },
		"parent on primary":   func(a *Approval) { a.ParentApprovalID = &previous.ID },
	} {
		draft := previous
		draft.ID, draft.RunID, draft.Status, draft.DecidedBy, draft.DecidedAt, draft.DecisionSource, draft.DecisionReason = 0, run.ID, "pending", nil, nil, nil, nil
		mutate(&draft)
		if err := db.CompleteRun(ctx, RunCompletion{RunID: run.ID, Status: "succeeded", FinishedAt: now, Approval: &draft}); err == nil {
			t.Fatalf("%s: invalid approval published", name)
		}
	}
	assertScopeRaceCount(t, db, &Approval{}, "run_id = ?", []any{run.ID}, 0)
}

func TestFinishExecutionAtomicallyQueuesVerificationAndIsIdempotent(t *testing.T) {
	db := openIntegrationDB(t)
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	approval, policy := executionFixture(t, db, now, "approved")
	row, claimed, err := db.ClaimApprovalExecution(ctx, approval.ID, now, policy)
	if err != nil || !claimed || row.OperationID == nil || *row.OperationID == "" || row.OperationStartedAt == nil {
		t.Fatalf("claim=%v row=%+v err=%v", claimed, row, err)
	}
	result, _ := json.Marshal(map[string]any{"written": true, "detail": "ok"})
	completion := ExecutionCompletion{ApprovalID: approval.ID, Status: "executed", ResultJSON: result, FinishedAt: now}
	if err := db.FinishExecution(ctx, completion); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetApproval(ctx, approval.ID)
	if err != nil || got.Status != "executed" || got.Verification == nil || got.Verification.Phase != "verify" {
		t.Fatalf("approval=%+v err=%v", got, err)
	}
	before, _ := db.ListIncidentEvents(ctx, approval.IncidentID, 0, 100)
	if err := db.FinishExecution(ctx, completion); err != nil {
		t.Fatal(err)
	}
	after, _ := db.ListIncidentEvents(ctx, approval.IncidentID, 0, 100)
	if len(before) != len(after) {
		t.Fatal("duplicate completion wrote more events")
	}
	completion.ResultJSON = []byte(`{"written":true,"detail":"different"}`)
	if err := db.FinishExecution(ctx, completion); err == nil {
		t.Fatal("conflicting completion accepted")
	}
}

// A refusal before writing is not an execution: no verification, no command
// history, no budget use, and a visible problem.
func TestAbortedExecutionIsNotAnAction(t *testing.T) {
	db := openIntegrationDB(t)
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	approval, policy := executionFixture(t, db, now, "approved")
	if _, claimed, err := db.ClaimApprovalExecution(ctx, approval.ID, now, policy); err != nil || !claimed {
		t.Fatalf("claim=%v %v", claimed, err)
	}
	if err := db.FinishExecution(ctx, ExecutionCompletion{ApprovalID: approval.ID, Status: "aborted", ResultJSON: []byte(`{"written":false,"error":"container identity changed"}`), FinishedAt: now}); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetApproval(ctx, approval.ID)
	if err != nil || got.Status != "aborted" || got.Verification != nil {
		t.Fatalf("approval=%+v err=%v", got, err)
	}
	assertScopeRaceCount(t, db, &FaultCmdHistory{}, "approval_id = ?", []any{approval.ID}, 0)
	assertScopeRaceCount(t, db, &IncidentProblem{}, "incident_id = ? AND code = ? AND status = ?", []any{approval.IncidentID, "execution_aborted", "open"}, 1)
	state, err := db.RemediationState(ctx, RemediationQuery{Service: *approval.Service, RuleID: *approval.RuleID, IncidentID: approval.IncidentID, Since: now.Add(-time.Hour)})
	if err != nil || state.Executions != 0 || state.IncidentActions != 0 || state.BusyWith != 0 || state.Blocked != "" {
		t.Fatalf("aborted execution counted: %+v %v", state, err)
	}
}

func TestVerificationQueueProgressAndTerminalReplay(t *testing.T) {
	db := openIntegrationDB(t)
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	approval, policy := executionFixture(t, db, now, "approved")
	executeFixture(t, db, approval, policy, now)
	task, found, err := db.NextVerificationTask(ctx, now)
	if err != nil || !found {
		t.Fatalf("task=%+v found=%v err=%v", task, found, err)
	}
	task, claimed, err := db.ClaimVerificationTask(ctx, approval.ID, now)
	if err != nil || !claimed || task.ClaimedAt == nil {
		t.Fatalf("claim=%+v %v %v", task, claimed, err)
	}
	completion := VerificationCompletion{ApprovalID: approval.ID, ClaimedAt: *task.ClaimedAt, CheckedAt: now, Status: "pending", NextCheckAt: now.Add(10 * time.Second), Observation: "unhealthy", Detail: "HTTP 503"}
	if _, err := db.FinalizeVerification(ctx, completion); err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := db.ClaimVerificationTask(ctx, approval.ID, now); err != nil || claimed {
		t.Fatalf("not due: claimed=%v err=%v", claimed, err)
	}
	task, claimed, err = db.ClaimVerificationTask(ctx, approval.ID, now.Add(10*time.Second))
	if err != nil || !claimed {
		t.Fatalf("claim=%v %v", claimed, err)
	}
	completion.ClaimedAt = *task.ClaimedAt
	completion.CheckedAt = now.Add(10 * time.Second)
	completion.Status = "passed"
	completion.Observation = "healthy"
	completion.Detail = "HTTP 200"
	result, err := db.FinalizeVerification(ctx, completion)
	if err != nil || !result.Applied || result.Status != "passed" || result.Phase != "verify" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if result, err := db.FinalizeVerification(ctx, completion); err != nil || result.Applied {
		t.Fatalf("duplicate=%+v err=%v", result, err)
	}
	got, err := db.GetApproval(ctx, approval.ID)
	if err != nil || got.Status != "executed" || got.Verification.Status != "passed" {
		t.Fatalf("approval=%+v err=%v", got, err)
	}
}
