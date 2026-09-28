package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"oncall-agent/internal/incident"
)

// These tests deliberately keep MySQL's default REPEATABLE READ environment.
// They do not impose an isolation level on the operation: using READ COMMITTED
// for the production transaction is one valid way to make them pass.
func openScopeRaceDB(t *testing.T) *DB {
	t.Helper()
	db := openIntegrationDB(t)
	t.Cleanup(func() { db.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var version, defaultIsolation, sessionIsolation string
	if err := db.WithContext(ctx).Raw("SELECT VERSION(), @@global.transaction_isolation, @@session.transaction_isolation").Row().Scan(&version, &defaultIsolation, &sessionIsolation); err != nil {
		t.Fatal(err)
	}
	t.Logf("MySQL %s: default isolation=%s, connection isolation=%s", version, defaultIsolation, sessionIsolation)
	if defaultIsolation != "REPEATABLE-READ" {
		t.Fatalf("scope race requires MySQL default REPEATABLE-READ, got %s", defaultIsolation)
	}
	return db
}

type scopeRaceContextKey struct{}

// Commit a real ingest member only after the operation's ordinary identity
// SELECT has completed AND MySQL reports its parent SELECT FOR UPDATE in flight.
// The holder owns that exact Incident row, so the outstanding SELECT cannot
// finish until commit. Callbacks alone would prove query ordering, not that the
// locking SELECT actually reached MySQL; PROCESSLIST supplies that last proof.
// It only inspects this app user's own connection, requiring no PROCESS grant.
func commitScopeChangeWhileWaiting(t *testing.T, db *DB, incidentID uint64, service, identityTable string, operation func(context.Context) error) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.WithValue(context.Background(), scopeRaceContextKey{}, t.Name()), 20*time.Second)
	defer cancel()
	holder := db.WithContext(ctx).Begin()
	if holder.Error != nil {
		t.Fatal(holder.Error)
	}
	defer holder.Rollback()
	var parent Incident
	if err := holder.Clauses(clause.Locking{Strength: "UPDATE"}).First(&parent, incidentID).Error; err != nil {
		t.Fatalf("hold parent: %v", err)
	}

	type identityRead struct {
		connectionID uint64
		sql          string
	}
	identityDone := make(chan identityRead, 1)
	parentRequested := make(chan struct{}, 1)
	matches := func(q *gorm.DB) bool {
		return q.Statement.Context.Value(scopeRaceContextKey{}) == t.Name()
	}
	const afterName = "test:scope_race_identity_done"
	const beforeName = "test:scope_race_parent_requested"
	queries := db.Callback().Query()
	if err := queries.After("gorm:query").Register(afterName, func(q *gorm.DB) {
		if !matches(q) || q.Error != nil || q.Statement.Table != identityTable || q.RowsAffected != 1 {
			return
		}
		if _, locking := q.Statement.Clauses["FOR"]; locking {
			return
		}
		var connectionID uint64
		// This metadata SELECT uses the same connection and does not read an
		// InnoDB table or create/change a consistent-read snapshot.
		if err := q.Statement.ConnPool.QueryRowContext(q.Statement.Context, "SELECT CONNECTION_ID()").Scan(&connectionID); err != nil {
			q.AddError(err)
			return
		}
		select {
		case identityDone <- identityRead{connectionID, q.Statement.SQL.String()}:
		default:
		}
	}); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := queries.Remove(afterName); err != nil {
			t.Error(err)
		}
	}()
	if err := queries.Before("gorm:query").Register(beforeName, func(q *gorm.DB) {
		if !matches(q) || q.Statement.Table != "incident" {
			return
		}
		if _, locking := q.Statement.Clauses["FOR"]; !locking {
			return
		}
		select {
		case parentRequested <- struct{}{}:
		default:
		}
	}); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := queries.Remove(beforeName); err != nil {
			t.Error(err)
		}
	}()

	done := make(chan error, 1)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		done <- operation(ctx)
	}()
	// On any fatal assertion, cancel/join the waiter and release the real lock
	// before removing callbacks or letting fixture cleanup delete its records.
	defer func() {
		cancel()
		holder.Rollback()
		select {
		case <-stopped:
		case <-time.After(5 * time.Second):
			t.Error("scope race operation did not stop after cancellation")
		}
	}()

	var initial identityRead
	select {
	case initial = <-identityDone:
		t.Logf("identity SELECT completed on connection %d before scope commit: %s", initial.connectionID, initial.sql)
	case err := <-done:
		t.Fatalf("operation returned before identity SELECT: %v", err)
	case <-ctx.Done():
		t.Fatalf("waiting for identity SELECT: %v", ctx.Err())
	}
	select {
	case <-parentRequested:
	case err := <-done:
		t.Fatalf("operation returned before parent lock: %v", err)
	case <-ctx.Done():
		t.Fatalf("waiting for parent lock request: %v", ctx.Err())
	}
	for {
		var process struct {
			State string
			Info  string
		}
		if err := db.WithContext(ctx).Raw("SELECT STATE, INFO FROM information_schema.PROCESSLIST WHERE ID = ?", initial.connectionID).Scan(&process).Error; err != nil {
			t.Fatalf("observe own waiting connection: %v", err)
		}
		if strings.Contains(process.Info, "`incident`") && strings.Contains(process.Info, "FOR UPDATE") {
			t.Logf("MySQL parent lock wait observed: connection=%d state=%q query=%s", initial.connectionID, process.State, process.Info)
			break
		}
		select {
		case err := <-done:
			t.Fatalf("operation returned while parent lock was held: %v", err)
		case <-ctx.Done():
			t.Fatalf("parent lock never reached MySQL: %v", ctx.Err())
		default:
		}
	}

	at := time.Now().UTC().Truncate(time.Millisecond)
	fp := sha256Hex(parent.GroupKey + "/Sub2APISlow")
	alert := Alert{Fingerprint: fp, AlertHash: md5Hex(fp), Source: "alertmanager", Name: "Sub2APISlow", Severity: 5, Status: "firing", Labels: datatypes.JSON(`{"service":"` + service + `","container":"sub2api"}`), Annotations: datatypes.JSON(`{}`), StartsAt: at, ReceivedAt: at}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		cleanup := db.WithContext(cleanupCtx)
		for _, row := range []any{&IncidentAlert{}, &LastAlert{}, &Alert{}} {
			if err := cleanup.Where("fingerprint = ?", fp).Delete(row).Error; err != nil {
				t.Errorf("clean unsupported scope member: %v", err)
			}
		}
	})
	if err := holder.Create(&alert).Error; err != nil {
		t.Fatalf("ingest unsupported alert: %v", err)
	}
	for _, row := range []any{&LastAlert{Fingerprint: fp, AlertID: alert.ID, AlertHash: alert.AlertHash, Status: "firing", Severity: 5, FirstSeen: at, LastSeen: at, IncidentID: &incidentID}, &IncidentAlert{IncidentID: incidentID, Fingerprint: fp, LinkedAt: at}} {
		if err := holder.Create(row).Error; err != nil {
			t.Fatalf("ingest unsupported member: %v", err)
		}
	}
	if err := holder.Model(&Incident{}).Where("id = ?", incidentID).Updates(map[string]any{"alerts_count": gorm.Expr("alerts_count + 1"), "last_seen_at": at}).Error; err != nil {
		t.Fatal(err)
	}
	if err := holder.Commit().Error; err != nil {
		t.Fatalf("commit new scope: %v", err)
	}
	t.Logf("holder committed real firing Sub2APISlow: incident=%d alert=%d", incidentID, alert.ID)

	var operationErr error
	select {
	case operationErr = <-done:
	case <-ctx.Done():
		t.Fatalf("operation did not finish after holder commit: %v", ctx.Err())
	}
	members, err := db.ListIncidentExecutionMembers(ctx, incidentID)
	if err != nil || len(members) != 2 {
		t.Fatalf("committed scope=%+v err=%v; want original Down plus new Slow", members, err)
	}
	if _, err := incident.FiringFingerprints(members, service, []string{testAlert}); err == nil {
		t.Fatal("committed scope must be outside the approved rule")
	}
	return operationErr
}

func TestClaimApprovalExecutionScopeRace(t *testing.T) {
	db := openScopeRaceDB(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	approval, policy := executionFixture(t, db, now, "approved")
	var row Approval
	var claimed bool
	err := commitScopeChangeWhileWaiting(t, db, approval.IncidentID, *approval.Service, "approval", func(ctx context.Context) error {
		var err error
		row, claimed, err = db.ClaimApprovalExecution(ctx, approval.ID, now, policy)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := db.GetApproval(context.Background(), approval.ID)
	if err != nil {
		t.Fatal(err)
	}
	if claimed || row.Status != "expired" || stored.Status != "expired" {
		t.Errorf("stale scope authorized execution: claimed=%v returned=%s persisted=%s; want false/expired/expired", claimed, row.Status, stored.Status)
	}
	assertScopeRaceCount(t, db, &IncidentEvent{}, "incident_id = ? AND event_type = ?", []any{approval.IncidentID, "execution.started"}, 0)
	assertScopeRaceCount(t, db, &IncidentEvent{}, "incident_id = ? AND event_type = ?", []any{approval.IncidentID, "approval.expired"}, 1)
}

func TestFinalizeVerificationScopeRace(t *testing.T) {
	for _, outcome := range []string{"passed", "failed"} {
		t.Run(outcome, func(t *testing.T) {
			db := openScopeRaceDB(t)
			ctx := context.Background()
			now := time.Now().UTC().Truncate(time.Millisecond)
			approval, _, _ := verificationFixture(t, db, now)
			parent, err := db.GetIncident(ctx, approval.IncidentID)
			if err != nil {
				t.Fatal(err)
			}
			run, err := db.GetAgentRun(ctx, approval.RunID)
			if err != nil || run.PlanJSON == nil {
				t.Fatalf("run=%+v err=%v", run, err)
			}
			memory := FaultMemory{Fingerprint: incident.FaultFingerprint(parent.GroupKey, testAlert), GroupKey: parent.GroupKey, AlertName: testAlert, PlanJSON: *run.PlanJSON, Confidence: "high", FirstSeen: now, LastSuccess: now, TTLSeconds: 3600}
			var completion VerificationCompletion
			if outcome == "passed" {
				task, claimed, err := db.ClaimVerificationTask(ctx, approval.ID, now)
				if err != nil || !claimed || task.ClaimedAt == nil {
					t.Fatalf("verification claim=%v task=%+v err=%v", claimed, task, err)
				}
				completion = VerificationCompletion{ApprovalID: approval.ID, ClaimedAt: *task.ClaimedAt, CheckedAt: now.Add(time.Second), Status: "passed", Observation: "healthy", Detail: "HTTP 200", Memory: &memory}
			} else {
				if err := db.UpsertFaultMemory(ctx, memory); err != nil {
					t.Fatal(err)
				}
				completion = failedVerificationCompletion(t, db, approval.ID)
				completion.DemoteFingerprint = memory.Fingerprint
			}
			var final VerificationFinalization
			err = commitScopeChangeWhileWaiting(t, db, approval.IncidentID, *approval.Service, "approval", func(ctx context.Context) error {
				var err error
				final, err = db.FinalizeVerification(ctx, completion)
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			if !final.Applied || final.Status != "inconclusive" || final.RetryRunID != 0 || final.Escalated {
				t.Errorf("stale scope finalized %s: %+v; want applied inconclusive without retry/escalation", outcome, final)
			}
			stored, err := db.GetApproval(ctx, approval.ID)
			if err != nil || stored.Status != "executed" || stored.Verification == nil {
				t.Fatalf("approval=%+v err=%v", stored, err)
			}
			if stored.Verification.Status != "inconclusive" {
				t.Errorf("persisted verification=%s; want inconclusive", stored.Verification.Status)
			}
			assertScopeRaceCount(t, db, &AgentRun{}, "retry_of = ?", []any{approval.RunID}, 0)
			if outcome == "passed" {
				assertScopeRaceCount(t, db, &FaultMemory{}, "fingerprint = ?", []any{memory.Fingerprint}, 0)
			} else {
				got, err := db.GetFaultMemory(ctx, memory.Fingerprint)
				if err != nil || got.Confidence != "high" {
					t.Errorf("unsupported scope demoted memory: confidence=%s err=%v; want high", got.Confidence, err)
				}
			}
			assertScopeRaceCount(t, db, &IncidentEvent{}, "incident_id = ? AND event_type IN ?", []any{parent.ID, []string{"verify.passed", "verify.failed", "retry.scheduled"}}, 0)
			assertScopeRaceCount(t, db, &IncidentEvent{}, "incident_id = ? AND event_type = ?", []any{parent.ID, "verify.inconclusive"}, 1)
			assertScopeRaceCount(t, db, &IncidentProblem{}, "incident_id = ? AND code = ? AND status = ?", []any{parent.ID, "verify_inconclusive", "open"}, 1)
		})
	}
}

func TestCompleteRunScopeRace(t *testing.T) {
	db := openScopeRaceDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	previous, _ := executionFixture(t, db, now, "pending")
	// Reuse the real fixture's scope/snapshot, then start a legitimate new run
	// after denying its old plan rather than rewinding a completed run in SQL.
	if _, err := db.DecideApproval(ctx, previous.ID, "denied", previous.PlanHash, "ops", "new diagnosis", "web", now); err != nil {
		t.Fatal(err)
	}
	run, created, err := db.RequestRun(ctx, RunRequest{IncidentID: previous.IncidentID, Mode: "full", Trigger: RunTriggerAlert, RequestedAt: now.Add(time.Second)})
	if err != nil || !created {
		t.Fatalf("new run=%+v created=%v err=%v", run, created, err)
	}
	draft := Approval{IncidentID: run.IncidentID, RunID: run.ID, Service: previous.Service, RuleID: previous.RuleID, ToolName: previous.ToolName, ArgsJSON: previous.ArgsJSON, ExecutionContext: previous.ExecutionContext, PlanHash: previous.PlanHash, Reason: "scope race publication", Status: "pending", CreatedAt: now, ExpiresAt: previous.ExpiresAt}
	completion := RunCompletion{RunID: run.ID, Status: "succeeded", RCA: "old supported scope", PlanJSON: []byte(`{"action":"docker_restart","confidence":"high"}`), FinishedAt: now.Add(2 * time.Second), Approval: &draft, Steps: []AgentRunStep{{Seq: 4, Kind: "guard", Name: "rules", StartedAt: now}}}
	err = commitScopeChangeWhileWaiting(t, db, run.IncidentID, *previous.Service, "agent_run", func(ctx context.Context) error {
		return db.CompleteRun(ctx, completion)
	})
	if err == nil {
		t.Errorf("stale scope published approval ID=%d; CompleteRun must reject unsupported scope", draft.ID)
	} else {
		t.Logf("scope publication rejected: %v", err)
	}
	stored, err := db.GetAgentRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if draft.ID != 0 || stored.Status != "pending" || stored.PlanJSON != nil {
		t.Errorf("publication was not rolled back: approval=%d run=%s plan=%v; want no approval and original pending run", draft.ID, stored.Status, stored.PlanJSON)
	}
	assertScopeRaceCount(t, db, &Approval{}, "run_id = ?", []any{run.ID}, 0)
	assertScopeRaceCount(t, db, &AgentRunStep{}, "run_id = ?", []any{run.ID}, 0)
	assertScopeRaceCount(t, db, &IncidentEvent{}, "run_id = ? AND event_type = ?", []any{run.ID, "approval.created"}, 0)
}

func assertScopeRaceCount(t *testing.T, db *DB, model any, where string, args []any, want int64) {
	t.Helper()
	var count int64
	if err := db.Model(model).Where(where, args...).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != want {
		t.Errorf("%T where %s %v: count=%d, want %d", model, where, args, count, want)
	}
}
