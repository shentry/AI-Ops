package diagnose

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"oncall-agent/internal/approval"
	"oncall-agent/internal/config"
	"oncall-agent/internal/incident"
	"oncall-agent/internal/llm"
	"oncall-agent/internal/memory"
	"oncall-agent/internal/store"
	"oncall-agent/internal/tools"
)

// These tests deliberately never use TEST_MYSQL_DSN: the worker consumes a
// schema-wide FIFO. Apply every migration to a dedicated oncall_diagnose*
// database. Missing env is the only skip path; configured DB failures must fail.
func openDiagnoseMySQL(t *testing.T) *store.DB {
	t.Helper()
	dsn := os.Getenv("TEST_DIAGNOSE_MYSQL_DSN")
	if dsn == "" {
		t.Skip("TEST_DIAGNOSE_MYSQL_DSN not set; requires dedicated migrated oncall_diagnose* MySQL")
	}
	db, err := store.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	var database string
	if err := db.Raw("SELECT DATABASE()").Scan(&database).Error; err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(database, "oncall_diagnose") {
		t.Fatalf("refusing non-diagnosis database %q", database)
	}
	db.DB = db.Session(&gorm.Session{Logger: logger.Default.LogMode(logger.Silent)})
	return db
}

// Test-only clock advances scheduling/window time, not the HTTP transport or
// MySQL. Atomic access permits the real httptest handler to model a late reply.
type diagnoseMySQLClock struct{ millis atomic.Int64 }

func (c *diagnoseMySQLClock) set(at time.Time) { c.millis.Store(at.UnixMilli()) }
func (c *diagnoseMySQLClock) now() time.Time   { return time.UnixMilli(c.millis.Load()).UTC() }

const diagnoseAlert = "Sub2APIDown"

// diagnoseMySQLCollector is the external evidence boundary: it reports an
// exited container, identified by Docker, that nothing restarts on its own.
type diagnoseMySQLCollector struct{ calls int }

func (*diagnoseMySQLCollector) Name() string { return "docker_inspect" }
func (c *diagnoseMySQLCollector) Collect(_ context.Context, target Target) EvidenceItem {
	c.calls++
	return EvidenceItem{Name: c.Name(), Source: "docker:inspect", Status: ItemOK, CollectedAt: time.Now().UTC(),
		Body:      fmt.Sprintf("incident %d: sub2api process exited", target.Incident.ID),
		Object:    &ObjectRef{Kind: "container", Name: "sub2api", ID: "c0ffee"},
		Container: &ContainerFacts{Status: "exited", ExitCode: 1, RestartPolicy: "no"}}
}

type fixtureBusinessMetrics struct{}

func (fixtureBusinessMetrics) Name() string { return "sub2api_metrics" }
func (fixtureBusinessMetrics) Collect(context.Context, Target) EvidenceItem {
	return EvidenceItem{Name: "sub2api_metrics", Source: "prometheus:query", Status: ItemOK,
		Business: &tools.BusinessTraffic{Requests: 100, Errors: 20, SampledAt: time.Now()}}
}

// fixtureRestart is the registered restart action: Prepare freezes the
// evidence-proven container and a health check; nothing ever executes it.
type fixtureRestart struct{ baseURL string }

func (fixtureRestart) Definition() tools.ActionDefinition {
	return tools.ActionDefinition{Name: tools.ActionDockerRestart, Version: 2, TargetKind: "container", Description: "fixture restart", Timeout: time.Second}
}

func (a fixtureRestart) Prepare(_ context.Context, req tools.PrepareRequest) (tools.Prepared, error) {
	check, _ := json.Marshal(tools.HealthCheck{BaseURL: a.baseURL})
	return tools.Prepared{Target: req.Target, Args: json.RawMessage(`{"target_kind":"container","target_name":"sub2api"}`),
		Revision: "started_at=fixture", PreState: json.RawMessage(`{"status":"exited"}`),
		Checks: []incident.Check{{Kind: incident.CheckHealth, Params: check}}}, nil
}

func (fixtureRestart) Execute(context.Context, tools.Operation) (tools.Receipt, error) {
	return tools.Receipt{}, fmt.Errorf("test must not invoke a mutation")
}

func (fixtureRestart) Reconcile(context.Context, tools.Operation) (tools.Reconciliation, error) {
	return tools.Reconciliation{Outcome: tools.OutcomeUnknown}, fmt.Errorf("test must not reconcile a mutation")
}

type diagnoseMySQLFixture struct {
	db           *store.DB
	service      string
	parent       store.Incident
	run          store.AgentRun
	approval     store.Approval
	authority    *approval.Authority
	registry     *tools.Registry
	pipeline     *Pipeline
	collector    *diagnoseMySQLCollector
	reasoner     *fakeReasoner // deterministic LLM boundary only; never a SQL/policy mock
	reporter     *fakeReporter
	fingerprints []string
	clock        diagnoseMySQLClock
}

// newDiagnoseMySQLFixture runs under its own service and auto rule, so the
// service mutex, budgets and rule blocks of one fixture never affect another.
func newDiagnoseMySQLFixture(t *testing.T, baseURL string, memoryHit bool) *diagnoseMySQLFixture {
	t.Helper()
	db := openDiagnoseMySQL(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	f := &diagnoseMySQLFixture{db: db, service: "sub2api-" + suffix}
	f.parent = store.Incident{GroupKey: "diagnose-mysql-" + suffix,
		Status: "firing", Severity: 5, AlertsCount: 1, Title: "isolated diagnosis integration",
		StartedAt: now, LastSeenAt: now}
	if err := db.Create(&f.parent).Error; err != nil {
		t.Fatal(err)
	}
	// Registered before any child rows. Every deletion is scoped, even after
	// partial setup, and errors fail the test. No TRUNCATE or global queue reset.
	t.Cleanup(func() {
		for _, deletion := range []struct {
			query string
			arg   any
		}{
			{"DELETE FROM verify_task WHERE approval_id IN (SELECT id FROM approval WHERE incident_id = ?)", f.parent.ID},
			{"DELETE FROM fault_cmd_history WHERE approval_id IN (SELECT id FROM approval WHERE incident_id = ?)", f.parent.ID},
			{"DELETE FROM incident_event WHERE incident_id = ?", f.parent.ID},
			{"DELETE FROM incident_problem WHERE incident_id = ?", f.parent.ID},
			{"DELETE FROM agent_run_step WHERE run_id IN (SELECT id FROM agent_run WHERE incident_id = ?)", f.parent.ID},
			{"DELETE FROM approval WHERE incident_id = ?", f.parent.ID},
			{"DELETE FROM agent_run WHERE incident_id = ?", f.parent.ID},
			{"DELETE FROM incident_alert WHERE incident_id = ?", f.parent.ID},
			{"DELETE FROM fault_memory WHERE fingerprint = ?", f.memoryFingerprint()},
			{"DELETE FROM last_alert WHERE fingerprint IN ?", f.fingerprints},
			{"DELETE FROM alert WHERE fingerprint IN ?", f.fingerprints},
			{"DELETE FROM service_lock WHERE service = ?", f.service},
			{"DELETE FROM incident WHERE id = ?", f.parent.ID},
		} {
			if err := db.Exec(deletion.query, deletion.arg).Error; err != nil {
				t.Errorf("cleanup %s: %v", deletion.query, err)
			}
		}
	})
	f.addMember(t, diagnoseAlert, f.service)
	var created bool
	var err error
	f.run, created, err = db.RequestRun(context.Background(), store.RunRequest{IncidentID: f.parent.ID,
		Mode: "full", Trigger: store.RunTriggerAlert, RequestedAt: now})
	if err != nil || !created {
		t.Fatalf("RequestRun created=%v err=%v", created, err)
	}
	if claimed, err := db.ClaimAgentRun(context.Background(), f.run.ID, now); err != nil || !claimed {
		t.Fatalf("ClaimAgentRun claimed=%v err=%v", claimed, err)
	}
	f.run = f.loadRun(t)
	plan := llm.Plan{Action: tools.ActionDockerRestart, Target: llm.PlanTarget{Kind: "container", Name: "sub2api"},
		Confidence: "high", Reason: "restart exited process", Expected: "health recovers"}
	if memoryHit {
		entry := store.FaultMemory{Fingerprint: f.memoryFingerprint(), GroupKey: f.parent.GroupKey,
			AlertName: diagnoseAlert, RCAText: "sub2api process exited", PlanJSON: diagnoseMySQLJSON(t, plan),
			Confidence: "high", FirstSeen: now, LastSuccess: now, TTLSeconds: 3600}
		if err := db.UpsertFaultMemory(context.Background(), entry); err != nil {
			t.Fatal(err)
		}
	}
	f.registry = tools.NewRegistry()
	if err := f.registry.RegisterAction(fixtureRestart{baseURL: baseURL}); err != nil {
		t.Fatal(err)
	}
	f.authority, err = approval.NewAuthority(config.ServiceConfig{Name: f.service, Env: "test", Container: "sub2api", BaseURL: baseURL},
		config.RemediationConfig{RulesVersion: "fixture", Rules: []config.RuleConfig{{ID: "restart-" + suffix, Action: tools.ActionDockerRestart,
			Mode: incident.ModeAuto, Alerts: []string{diagnoseAlert}, MaxExecutions: 100, WindowMinutes: 60}},
			Verification: config.VerificationConfig{IntervalSeconds: 10, WindowSeconds: 120, TimeoutSeconds: 5, RequiredPasses: 1}}, f.registry)
	if err != nil {
		t.Fatal(err)
	}
	f.collector = &diagnoseMySQLCollector{}
	f.reasoner = &fakeReasoner{result: &llm.DiagnoseResult{RCA: "sub2api process exited", Confidence: "high", Plan: plan,
		TokensIn: 21, TokensOut: 13, Steps: []llm.StepLog{{Name: "docker_logs", Input: `{"target_name":"sub2api"}`, Output: "process exited"}}}}
	f.reporter = &fakeReporter{}
	f.pipeline = NewPipeline(db, NewEvidenceBuilder(db, []Collector{f.collector, fixtureBusinessMetrics{}}), f.reasoner, approval.NewPolicy(f.authority, f.registry, 30*time.Minute, db),
		approval.NewService(db), f.reporter, memory.NewStore(db, 3600, true), 0)
	return f
}

func diagnoseMySQLJSON(t *testing.T, value any) datatypes.JSON {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return datatypes.JSON(raw)
}

func (f *diagnoseMySQLFixture) memoryFingerprint() string {
	return incident.FaultFingerprint(f.parent.GroupKey, diagnoseAlert)
}

func (f *diagnoseMySQLFixture) addMember(t *testing.T, name, service string) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Millisecond)
	fp := fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s-%d", f.parent.GroupKey, len(f.fingerprints)))))
	f.fingerprints = append(f.fingerprints, fp)
	alert := store.Alert{Fingerprint: fp, AlertHash: fp[:32], Source: "alertmanager", Name: name, Status: "firing", Severity: 5,
		Labels: diagnoseMySQLJSON(t, map[string]string{"service": service}), Annotations: datatypes.JSON(`{}`),
		StartsAt: now, ReceivedAt: now}
	if err := f.db.Create(&alert).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.Create(&store.LastAlert{Fingerprint: fp, AlertID: alert.ID, AlertHash: alert.AlertHash, Status: "firing",
		Severity: 5, FirstSeen: now, LastSeen: now, IncidentID: &f.parent.ID}).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.Create(&store.IncidentAlert{IncidentID: f.parent.ID, Fingerprint: fp, LinkedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
}

func (f *diagnoseMySQLFixture) loadRun(t *testing.T) store.AgentRun {
	t.Helper()
	run, err := f.db.GetAgentRun(context.Background(), f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func (f *diagnoseMySQLFixture) diagnose(t *testing.T) {
	t.Helper()
	if err := f.pipeline.Run(context.Background(), f.run); err != nil {
		t.Fatal(err)
	}
	f.run = f.loadRun(t)
	var approvals []store.Approval
	if err := f.db.Where("run_id = ?", f.run.ID).Find(&approvals).Error; err != nil {
		t.Fatal(err)
	}
	if f.run.Status != "succeeded" || f.run.RCAText == nil || f.run.PlanJSON == nil || len(approvals) != 1 || approvals[0].Status != "approved" {
		t.Fatalf("invalid clean diagnosis run=%+v approvals=%+v", f.run, approvals)
	}
	f.approval = approvals[0]
	hash, err := incident.PlanHash(f.approval.ToolName, f.approval.ArgsJSON, f.approval.ExecutionContext)
	if err != nil || hash != f.approval.PlanHash {
		t.Fatalf("invalid immutable approval hash=%s err=%v", hash, err)
	}
	steps, err := f.db.ListRunSteps(context.Background(), f.run.ID)
	if err != nil || !hasExplicitUnchangedGuard(steps) {
		t.Fatalf("Pipeline did not emit eligible Guard audit: %+v err=%v", steps, err)
	}
}

func (f *diagnoseMySQLFixture) execute(t *testing.T) *VerificationWorker {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Millisecond)
	if _, claimed, err := f.db.ClaimApprovalExecution(context.Background(), f.approval.ID, now, f.authority.Policy(now)); err != nil || !claimed {
		t.Fatalf("ClaimApprovalExecution claimed=%v err=%v", claimed, err)
	}
	// Deliberately synthetic action result: tests cover diagnosis/verification,
	// not the parent's physical restart acceptance. No Executor/tool is invoked.
	if err := f.db.FinishExecution(context.Background(), store.ExecutionCompletion{ApprovalID: f.approval.ID,
		Status: "executed", ResultJSON: []byte(`{"written":true,"detail":"test-only external action completed"}`), FinishedAt: now}); err != nil {
		t.Fatal(err)
	}
	f.clock.set(now)
	worker := NewVerificationWorker(f.db, NewVerifier(f.registry, nil, nil), 3600, nil, log.New(io.Discard, "", 0), f.authority.Binding())
	worker.now = f.clock.now
	return worker
}

func (f *diagnoseMySQLFixture) task(t *testing.T) store.VerifyTask {
	t.Helper()
	var task store.VerifyTask
	if err := f.db.First(&task, "approval_id = ?", f.approval.ID).Error; err != nil {
		t.Fatal(err)
	}
	return task
}

func (f *diagnoseMySQLFixture) count(t *testing.T, table, condition string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := f.db.Table(table).Where(condition, args...).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	return n
}

func (f *diagnoseMySQLFixture) eventCount(t *testing.T, event string) int64 {
	t.Helper()
	return f.count(t, "incident_event", "incident_id = ? AND event_type = ?", f.parent.ID, event)
}

// Condition includes this fixture's run/incident/fingerprint. The trigger never
// rejects another fixture's rows. It is removed before row cleanup, even on fail.
func (f *diagnoseMySQLFixture) inject(t *testing.T, table, condition string) func() {
	t.Helper()
	name := fmt.Sprintf("diagnose_gate_%d_%d", f.parent.ID, time.Now().UnixNano())
	query := fmt.Sprintf("CREATE TRIGGER `%s` BEFORE INSERT ON `%s` FOR EACH ROW BEGIN IF %s THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'diagnose integration injection'; END IF; END", name, table, condition)
	if err := f.db.Exec(query).Error; err != nil {
		t.Fatal(err)
	}
	removed := false
	remove := func() {
		if !removed {
			if err := f.db.Exec("DROP TRIGGER `" + name + "`").Error; err != nil {
				t.Error(err)
				return
			}
			removed = true
		}
	}
	t.Cleanup(remove)
	return remove
}
