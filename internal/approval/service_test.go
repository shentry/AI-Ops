package approval

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"oncall-agent/internal/config"
	"oncall-agent/internal/incident"
	"oncall-agent/internal/llm"
	"oncall-agent/internal/store"
	"oncall-agent/internal/tools"
)

func policyTestRegistry(t *testing.T, level tools.SafetyLevel) *tools.Registry {
	t.Helper()
	registry := tools.NewRegistry()
	for name, safety := range map[string]tools.SafetyLevel{
		incident.RestartAction: level, "read_metrics": tools.L1ReadOnly,
		"resize_pool": tools.L3Approval, "drop_database": tools.L4Forbidden,
	} {
		if err := registry.Register(tools.ToolSpec{Name: name, Level: safety, Description: name, Timeout: time.Second,
			Handler: func(context.Context, json.RawMessage) (string, error) { return "ok", nil }}); err != nil {
			t.Fatal(err)
		}
	}
	return registry
}

func planFor(action string) llm.Plan {
	return llm.Plan{Action: action, Target: llm.PlanTarget{Kind: "container", Name: "sub2api"}, Risk: "low"}
}

type fakeExecutionCounter struct {
	count        int
	err          error
	calls        int
	tool, target string
	since        time.Time
}

func (f *fakeExecutionCounter) CountRecentExecutions(_ context.Context, tool, target string, since time.Time) (int, error) {
	f.calls++
	f.tool, f.target, f.since = tool, target, since
	return f.count, f.err
}

func l2PolicyConfig() PolicyConfig {
	return PolicyConfig{AutoExecuteL2: true, AllowedTargets: []string{"sub2api"}, Container: "sub2api", HealthBaseURL: "http://127.0.0.1:8080",
		Verification: config.VerificationConfig{IntervalSeconds: 10, WindowSeconds: 120, TimeoutSeconds: 5}, RateWindow: time.Hour, MaxPerWindow: 1}
}

func l2Input() PolicyInput {
	return PolicyInput{Members: []incident.ExecutionMember{{Fingerprint: "fp1", Name: incident.SupportedAlert, Status: "firing", Container: "sub2api", Service: "sub2api"}}}
}

func TestPolicyDecisions(t *testing.T) {
	for _, test := range []struct {
		name, action string
		level        tools.SafetyLevel
		want         string
	}{
		{"none", "none", tools.L2LowRisk, DecisionNone},
		{"empty", "", tools.L2LowRisk, DecisionNone},
		{"unknown", "hack_tool", tools.L2LowRisk, DecisionDenied},
		{"readonly", "read_metrics", tools.L2LowRisk, DecisionAutoL1},
		{"L2", incident.RestartAction, tools.L2LowRisk, DecisionAutoL2},
		{"L3 supported", incident.RestartAction, tools.L3Approval, DecisionApproval},
		{"L3 unsupported", "resize_pool", tools.L2LowRisk, DecisionDenied},
		{"L4", incident.RestartAction, tools.L4Forbidden, DecisionDenied},
	} {
		t.Run(test.name, func(t *testing.T) {
			d := NewPolicy(policyTestRegistry(t, test.level), l2PolicyConfig(), &fakeExecutionCounter{}).Decide(context.Background(), planFor(test.action), l2Input())
			if d.Kind != test.want {
				t.Fatalf("decision = %+v, want %s", d, test.want)
			}
			if d.Kind == DecisionApproval || d.Kind == DecisionAutoL2 {
				snapshot, err := incident.ParseExecutionContext(d.ExecutionContext)
				if err != nil || snapshot.SafetyLevel != string(test.level) {
					t.Fatalf("snapshot = %+v, err=%v", snapshot, err)
				}
			}
		})
	}
	// Read-only tools do not require a mutation/verification binding.
	d := NewPolicy(policyTestRegistry(t, tools.L2LowRisk), PolicyConfig{}, nil).Decide(context.Background(), planFor("read_metrics"), PolicyInput{})
	if d.Kind != DecisionAutoL1 || len(d.ExecutionContext) != 0 || d.PlanHash != "" {
		t.Fatalf("readonly = %+v", d)
	}
}

func TestPolicyUnsupportedScopeDeniedEvenForManualApproval(t *testing.T) {
	for _, level := range []tools.SafetyLevel{tools.L2LowRisk, tools.L3Approval} {
		for _, test := range []struct {
			name   string
			change func(*PolicyConfig, *PolicyInput, *llm.Plan)
		}{
			{"no members", func(_ *PolicyConfig, in *PolicyInput, _ *llm.Plan) { in.Members = nil }},
			{"resolved", func(_ *PolicyConfig, in *PolicyInput, _ *llm.Plan) { in.Members[0].Status = "resolved" }},
			{"unsupported fault", func(_ *PolicyConfig, in *PolicyInput, _ *llm.Plan) { in.Members[0].Name = "Sub2APISlow" }},
			{"mixed faults", func(_ *PolicyConfig, in *PolicyInput, _ *llm.Plan) {
				in.Members = append(in.Members, incident.ExecutionMember{Fingerprint: "fp2", Name: "RedisDown", Status: "firing"})
			}},
			{"other member target", func(_ *PolicyConfig, in *PolicyInput, _ *llm.Plan) { in.Members[0].Container = "other" }},
			{"missing provenance", func(_ *PolicyConfig, in *PolicyInput, _ *llm.Plan) { in.Members[0].Service = "" }},
			{"missing allowlist", func(cfg *PolicyConfig, _ *PolicyInput, _ *llm.Plan) { cfg.AllowedTargets = nil }},
			{"missing container binding", func(cfg *PolicyConfig, _ *PolicyInput, _ *llm.Plan) { cfg.Container = "" }},
			{"missing URL binding", func(cfg *PolicyConfig, _ *PolicyInput, _ *llm.Plan) { cfg.HealthBaseURL = "" }},
			{"credential URL", func(cfg *PolicyConfig, _ *PolicyInput, _ *llm.Plan) { cfg.HealthBaseURL = "http://user:secret@host" }},
			{"invalid timing", func(cfg *PolicyConfig, _ *PolicyInput, _ *llm.Plan) { cfg.Verification.TimeoutSeconds = 10 }},
			{"broad target", func(_ *PolicyConfig, _ *PolicyInput, plan *llm.Plan) { plan.Target.Kind = "cluster" }},
			{"invented target", func(_ *PolicyConfig, _ *PolicyInput, plan *llm.Plan) { plan.Target.Name = "other" }},
		} {
			t.Run(string(level)+"/"+test.name, func(t *testing.T) {
				cfg, input, plan := l2PolicyConfig(), l2Input(), planFor(incident.RestartAction)
				cfg.AutoExecuteL2 = false // Disabling automatic execution never bypasses scope validation.
				test.change(&cfg, &input, &plan)
				d := NewPolicy(policyTestRegistry(t, level), cfg, nil).Decide(context.Background(), plan, input)
				if d.Kind != DecisionDenied || d.PlanHash != "" {
					t.Fatalf("decision = %+v", d)
				}
			})
		}
	}
}

func TestPolicyL2AutomaticConditionsDegradeToApproval(t *testing.T) {
	for _, test := range []struct {
		name    string
		change  func(*PolicyConfig)
		counter *fakeExecutionCounter
		reason  string
	}{
		{"switch off", func(c *PolicyConfig) { c.AutoExecuteL2 = false }, &fakeExecutionCounter{}, "auto_execute_l2 disabled"},
		{"dry run", func(c *PolicyConfig) { c.DryRun = true }, &fakeExecutionCounter{}, "dry_run enabled"},
		{"rate exceeded", func(*PolicyConfig) {}, &fakeExecutionCounter{count: 1}, "rate limit reached"},
		{"rate error", func(*PolicyConfig) {}, &fakeExecutionCounter{err: errors.New("db down")}, "rate limit check failed"},
		{"rate unconfigured", func(c *PolicyConfig) { c.MaxPerWindow = 0 }, &fakeExecutionCounter{}, "rate limit is not configured"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := l2PolicyConfig()
			test.change(&cfg)
			d := NewPolicy(policyTestRegistry(t, tools.L2LowRisk), cfg, test.counter).Decide(context.Background(), planFor(incident.RestartAction), l2Input())
			if d.Kind != DecisionApproval || !strings.Contains(d.Reason, test.reason) {
				t.Fatalf("decision = %+v", d)
			}
			if _, err := incident.PlanHash(d.ToolName, d.Args, d.ExecutionContext); err != nil {
				t.Fatal(err)
			}
		})
	}
	d := NewPolicy(policyTestRegistry(t, tools.L2LowRisk), l2PolicyConfig(), nil).Decide(context.Background(), planFor(incident.RestartAction), l2Input())
	if d.Kind != DecisionApproval {
		t.Fatalf("missing counter = %+v", d)
	}
}

func TestPolicyCountsTargetAcrossDifferentSnapshotHashes(t *testing.T) {
	counter := &fakeExecutionCounter{}
	p := NewPolicy(policyTestRegistry(t, tools.L2LowRisk), l2PolicyConfig(), counter)
	now := time.Now().UTC()
	p.now = func() time.Time { return now }
	a := p.Decide(context.Background(), planFor(incident.RestartAction), l2Input())
	counter.count = 1
	input := l2Input()
	input.Members[0].Fingerprint = "new-fault"
	b := p.Decide(context.Background(), planFor(incident.RestartAction), input)
	if a.Kind != DecisionAutoL2 || b.Kind != DecisionApproval || a.PlanHash == b.PlanHash {
		t.Fatalf("a=%+v b=%+v", a, b)
	}
	if counter.calls != 2 || counter.tool != incident.RestartAction || counter.target != "sub2api" || !counter.since.Equal(now.Add(-time.Hour)) {
		t.Fatalf("counter = %+v", counter)
	}
}

func TestPolicySnapshotRetainsOriginalDryRunAndVerification(t *testing.T) {
	cfg := l2PolicyConfig()
	cfg.DryRun = true
	p := NewPolicy(policyTestRegistry(t, tools.L2LowRisk), cfg, &fakeExecutionCounter{})
	first := p.Decide(context.Background(), planFor(incident.RestartAction), l2Input())
	p.cfg.DryRun = false
	p.cfg.Verification.WindowSeconds = 240
	second := p.Decide(context.Background(), planFor(incident.RestartAction), l2Input())
	original, err := incident.ParseExecutionContext(first.ExecutionContext)
	if err != nil || !original.DryRun || original.Verification.WindowSeconds != 120 || first.PlanHash == second.PlanHash {
		t.Fatalf("original=%+v err=%v first=%+v second=%+v", original, err, first, second)
	}
}

func preparedDecision(t *testing.T, kind string) Decision {
	t.Helper()
	cfg := l2PolicyConfig()
	cfg.DryRun = kind == DecisionApproval
	d := NewPolicy(policyTestRegistry(t, tools.L2LowRisk), cfg, &fakeExecutionCounter{}).Decide(context.Background(), planFor(incident.RestartAction), l2Input())
	if d.Kind != kind {
		t.Fatalf("decision = %+v", d)
	}
	return d
}

func TestServicePrepareValidatesAndDoesNotWrite(t *testing.T) {
	// No store is needed: Prepare must only construct a draft for CompleteRun.
	svc := NewService(nil, 30)
	for _, kind := range []string{DecisionApproval, DecisionAutoL2} {
		d := preparedDecision(t, kind)
		a, err := svc.Prepare(7, 11, d, "safe reason")
		if err != nil {
			t.Fatal(err)
		}
		if a.ID != 0 || a.IncidentID != 7 || a.RunID != 11 || a.PlanHash != d.PlanHash || !a.ExpiresAt.Equal(a.CreatedAt.Add(30*time.Minute)) {
			t.Fatalf("draft = %+v", a)
		}
		if kind == DecisionApproval {
			if a.Status != "pending" || a.DecidedBy != nil {
				t.Fatalf("manual draft = %+v", a)
			}
		} else if a.Status != "approved" || a.DecidedBy == nil || *a.DecidedBy != "system:auto_l2" || a.DecisionSource == nil || *a.DecisionSource != "system" || a.DecidedAt == nil || a.DecisionReason == nil || *a.DecisionReason != "safe reason" {
			t.Fatalf("automatic draft = %+v", a)
		}
		// The draft owns its bytes: later caller/config changes cannot upgrade dry-run.
		d.Args[0] = 'x'
		d.ExecutionContext[0] = 'x'
		snapshot, err := incident.ParseExecutionContext(a.ExecutionContext)
		if err != nil || snapshot.DryRun != (kind == DecisionApproval) {
			t.Fatalf("immutable snapshot = %+v, err=%v", snapshot, err)
		}
		if hash, err := incident.PlanHash(a.ToolName, a.ArgsJSON, a.ExecutionContext); err != nil || hash != a.PlanHash {
			t.Fatalf("draft hash = %s, %v", hash, err)
		}
	}
}

func TestServicePrepareRejectsMalformedOrMismatchedSnapshot(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*Decision)
	}{
		{"missing snapshot", func(d *Decision) { d.ExecutionContext = nil }},
		{"malformed snapshot", func(d *Decision) { d.ExecutionContext = []byte(`{"dry_run":false}`) }},
		{"missing mode", func(d *Decision) {
			d.ExecutionContext = []byte(strings.Replace(string(d.ExecutionContext), `"dry_run":true,`, "", 1))
		}},
		{"changed mode", func(d *Decision) {
			d.ExecutionContext = []byte(strings.Replace(string(d.ExecutionContext), `"dry_run":true`, `"dry_run":false`, 1))
		}},
		{"changed target", func(d *Decision) { d.Args = []byte(`{"target_kind":"container","target_name":"other"}`) }},
		{"bad hash", func(d *Decision) { d.PlanHash = "wrong" }},
		{"readonly", func(d *Decision) { d.Kind = DecisionAutoL1 }},
		{"denied", func(d *Decision) { d.Kind = DecisionDenied }},
		{"dry automatic", func(d *Decision) { d.Kind = DecisionAutoL2 }},
		{"L3 automatic", func(d *Decision) {
			d.Kind = DecisionAutoL2
			d.ExecutionContext = []byte(strings.Replace(strings.Replace(string(d.ExecutionContext), `"L2"`, `"L3"`, 1), `"dry_run":true`, `"dry_run":false`, 1))
			d.PlanHash, _ = incident.PlanHash(d.ToolName, d.Args, d.ExecutionContext)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			d := preparedDecision(t, DecisionApproval)
			test.change(&d)
			if _, err := NewService(nil, 30).Prepare(7, 11, d, "reason"); err == nil {
				t.Fatal("unsafe approval draft accepted")
			}
		})
	}
	for _, test := range []struct {
		incidentID, runID uint64
		ttl               int
		reason            string
	}{{0, 11, 30, "r"}, {7, 0, 30, "r"}, {7, 11, 0, "r"}, {7, 11, 30, " "}} {
		if _, err := NewService(nil, test.ttl).Prepare(test.incidentID, test.runID, preparedDecision(t, DecisionApproval), test.reason); err == nil {
			t.Fatalf("invalid draft accepted: %+v", test)
		}
	}
}

type fakeApprovalStore struct {
	row                                 store.Approval
	id                                  uint64
	status, hash, actor, reason, source string
	err                                 error
}

func (f *fakeApprovalStore) GetApproval(context.Context, uint64) (store.Approval, error) {
	return f.row, f.err
}
func (f *fakeApprovalStore) ListApprovals(_ context.Context, status string) ([]store.Approval, error) {
	f.status = status
	return []store.Approval{f.row}, f.err
}
func (f *fakeApprovalStore) DecideApproval(_ context.Context, id uint64, status, hash, actor, reason, source string, _ time.Time) (store.Approval, error) {
	f.id, f.status, f.hash, f.actor, f.reason, f.source = id, status, hash, actor, reason, source
	return f.row, f.err
}

func TestServiceForwardsExpectedHashToAtomicDecision(t *testing.T) {
	for _, approve := range []bool{true, false} {
		for _, dbErr := range []error{nil, store.ErrApprovalConflict, store.ErrApprovalNotFound} {
			db := &fakeApprovalStore{row: store.Approval{ID: 42}, err: dbErr}
			svc := NewService(db, 30)
			row, err := svc.Decide(context.Background(), 42, approve, "expected-hash", "anonymous", "operator note", "web")
			if !errors.Is(err, dbErr) || row.ID != 42 || db.id != 42 || db.hash != "expected-hash" || db.actor != "anonymous" || db.reason != "operator note" || db.source != "web" {
				t.Fatalf("row=%+v err=%v db=%+v", row, err, db)
			}
			want := "denied"
			if approve {
				want = "approved"
			}
			if db.status != want {
				t.Fatal(db.status)
			}
		}
	}
}

func TestServiceGetAndList(t *testing.T) {
	db := &fakeApprovalStore{row: store.Approval{ID: 42}}
	svc := NewService(db, 30)
	if row, err := svc.Get(context.Background(), 42); err != nil || row.ID != 42 {
		t.Fatalf("Get = %+v, %v", row, err)
	}
	if rows, err := svc.List(context.Background(), "pending"); err != nil || len(rows) != 1 || db.status != "pending" {
		t.Fatalf("List = %+v, %v", rows, err)
	}
}
