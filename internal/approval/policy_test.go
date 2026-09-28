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

// fakeAction is a registered write whose behavior each test scripts.
type fakeAction struct {
	def        tools.ActionDefinition
	prepared   tools.Prepared
	prepareErr error
	requests   []tools.PrepareRequest
	execute    func(tools.Operation) (tools.Receipt, error)
	reconcile  func(tools.Operation) (tools.Outcome, error)
	executed   []tools.Operation
	reconciled []tools.Operation
}

func (a *fakeAction) Definition() tools.ActionDefinition { return a.def }

func (a *fakeAction) Prepare(_ context.Context, req tools.PrepareRequest) (tools.Prepared, error) {
	a.requests = append(a.requests, req)
	return a.prepared, a.prepareErr
}

func (a *fakeAction) Execute(_ context.Context, op tools.Operation) (tools.Receipt, error) {
	a.executed = append(a.executed, op)
	if a.execute == nil {
		return tools.Receipt{Written: true, Detail: "restarted"}, nil
	}
	return a.execute(op)
}

func (a *fakeAction) Reconcile(_ context.Context, op tools.Operation) (tools.Reconciliation, error) {
	a.reconciled = append(a.reconciled, op)
	if a.reconcile == nil {
		return tools.Reconciliation{Outcome: tools.OutcomeUnknown}, errors.New("not scripted")
	}
	outcome, err := a.reconcile(op)
	return tools.Reconciliation{Outcome: outcome}, err
}

var testTarget = incident.Object{Kind: "container", Name: "sub2api", ID: "abc123"}

func newRestartAction() *fakeAction {
	return &fakeAction{
		def: tools.ActionDefinition{Name: "docker_restart", Version: 2, TargetKind: "container", Description: "restart", Timeout: time.Second},
		prepared: tools.Prepared{Target: testTarget, Args: json.RawMessage(`{"target_kind":"container","target_name":"sub2api"}`),
			Revision: "started_at=2026-09-24T00:00:00Z", PreState: json.RawMessage(`{"status":"running"}`),
			Checks: []incident.Check{{Kind: incident.CheckHealth, Params: json.RawMessage(`{"base_url":"http://127.0.0.1:8080"}`)}}},
	}
}

func testRegistry(t *testing.T, actions ...*fakeAction) *tools.Registry {
	t.Helper()
	registry := tools.NewRegistry()
	for _, action := range actions {
		if err := registry.RegisterAction(action); err != nil {
			t.Fatal(err)
		}
	}
	undo := &fakeAction{def: tools.ActionDefinition{Name: "upstream_restore", Version: 1, TargetKind: "account", Description: "undo", Compensation: true, Timeout: time.Second}}
	if err := registry.RegisterAction(undo); err != nil {
		t.Fatal(err)
	}
	return registry
}

func testRemediation(mode string) config.RemediationConfig {
	return config.RemediationConfig{RulesVersion: "r1",
		Rules:        []config.RuleConfig{{ID: "restart", Action: "docker_restart", Mode: mode, Alerts: []string{"Sub2APIDown"}, MaxExecutions: 2, WindowMinutes: 60}},
		Verification: config.VerificationConfig{IntervalSeconds: 10, WindowSeconds: 300, TimeoutSeconds: 5, RequiredPasses: 3, WatchSeconds: 1800}}
}

var testService = config.ServiceConfig{Name: "sub2api", Env: "prod", Container: "sub2api"}

func testAuthority(t *testing.T, registry *tools.Registry, remediation config.RemediationConfig) *Authority {
	t.Helper()
	authority, err := NewAuthority(testService, remediation, registry)
	if err != nil {
		t.Fatal(err)
	}
	return authority
}

type fakeState struct {
	state   store.RemediationState
	err     error
	queries []store.RemediationQuery
}

func (f *fakeState) RemediationState(_ context.Context, q store.RemediationQuery) (store.RemediationState, error) {
	f.queries = append(f.queries, q)
	return f.state, f.err
}

var policyNow = time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

func testPolicy(t *testing.T, action *fakeAction, remediation config.RemediationConfig, state *fakeState) *Policy {
	t.Helper()
	registry := testRegistry(t, action)
	policy := NewPolicy(testAuthority(t, registry, remediation), registry, 30*time.Minute, state)
	policy.now = func() time.Time { return policyNow }
	return policy
}

func testInput() PolicyInput {
	return PolicyInput{IncidentID: 7, FaultAlert: "Sub2APIDown", Target: testTarget, EvidenceRefs: []string{"docker_inspect"}, ObservationOK: true,
		Members: []incident.ExecutionMember{{Fingerprint: "fp-down", Name: "Sub2APIDown", Status: "firing", Service: "sub2api"}, {Fingerprint: "fp-old", Name: "Sub2APIDown", Status: "resolved", Service: "sub2api"}}}
}

func TestPolicyDecisions(t *testing.T) {
	for name, test := range map[string]struct {
		mode   string
		plan   string
		input  func(*PolicyInput)
		action func(*fakeAction)
		state  store.RemediationState
		want   string
		reason string
	}{
		"no action":            {mode: incident.ModeAuto, plan: "none", want: DecisionNone},
		"unknown action":       {mode: incident.ModeAuto, plan: "docker_exec", want: DecisionDenied, reason: "not an enabled action"},
		"compensation planned": {mode: incident.ModeAuto, plan: "upstream_restore", want: DecisionDenied, reason: "not an enabled action"},
		"alert outside rule": {mode: incident.ModeAuto, plan: "docker_restart", want: DecisionDenied, reason: "no remediation rule",
			input: func(in *PolicyInput) {
				in.Members = append(in.Members, incident.ExecutionMember{Fingerprint: "fp-slow", Name: "Sub2APISlow", Status: "firing", Service: "sub2api"})
			}},
		"other service": {mode: incident.ModeAuto, plan: "docker_restart", want: DecisionDenied, reason: "no remediation rule",
			input: func(in *PolicyInput) { in.Members[0].Service = "other" }},
		"observe would act":    {mode: incident.ModeObserve, plan: "docker_restart", want: DecisionObserve, reason: "would docker_restart container/sub2api"},
		"observe would refuse": {mode: incident.ModeObserve, plan: "docker_restart", want: DecisionObserve, reason: "would refuse", action: func(a *fakeAction) { a.prepareErr = errors.New("container is restarting") }},
		"manual":               {mode: incident.ModeManual, plan: "docker_restart", want: DecisionApproval, reason: "requires a person"},
		"auto":                 {mode: incident.ModeAuto, plan: "docker_restart", want: DecisionAuto, reason: "authorizes automatic execution"},
		"prepare refused": {mode: incident.ModeAuto, plan: "docker_restart", want: DecisionDenied, reason: "identity changed", action: func(a *fakeAction) {
			a.prepareErr = errors.Join(tools.ErrActionRefused, errors.New("identity changed"))
		}},
		"emergency stop":   {mode: incident.ModeAuto, plan: "docker_restart", want: DecisionDenied, reason: "emergency stop is active: drill", state: store.RemediationState{Stopped: true, StopReason: "drill"}},
		"service busy":     {mode: incident.ModeManual, plan: "docker_restart", want: DecisionDenied, reason: "busy with approval 9", state: store.RemediationState{BusyWith: 9}},
		"budget exhausted": {mode: incident.ModeManual, plan: "docker_restart", want: DecisionDenied, reason: "budget exhausted", state: store.RemediationState{Executions: 2}},
	} {
		t.Run(name, func(t *testing.T) {
			action := newRestartAction()
			if test.action != nil {
				test.action(action)
			}
			input := testInput()
			if test.input != nil {
				test.input(&input)
			}
			state := &fakeState{state: test.state}
			decision := testPolicy(t, action, testRemediation(test.mode), state).Decide(context.Background(), llm.Plan{Action: test.plan}, input)
			if decision.Kind != test.want || !strings.Contains(decision.Reason, test.reason) {
				t.Fatalf("decision=%s reason=%q; want %s containing %q", decision.Kind, decision.Reason, test.want, test.reason)
			}
			executable := decision.Kind == DecisionAuto || decision.Kind == DecisionApproval
			if executable != (decision.PlanHash != "") || (decision.Kind == DecisionObserve && len(decision.ExecutionContext) != 0) {
				t.Fatalf("only an executable decision carries a snapshot: %+v", decision)
			}
		})
	}
}

func TestPolicyFreezesCompleteSnapshot(t *testing.T) {
	action := newRestartAction()
	state := &fakeState{}
	plan := llm.Plan{Action: "docker_restart", Params: json.RawMessage(`{"reason":"oom"}`)}
	decision := testPolicy(t, action, testRemediation(incident.ModeManual), state).Decide(context.Background(), plan, testInput())
	if decision.Kind != DecisionApproval || decision.RuleID != "restart" || decision.Service != "sub2api" || decision.Demoted {
		t.Fatalf("decision=%+v", decision)
	}
	if len(action.requests) != 1 || action.requests[0].Target != testTarget || string(action.requests[0].Params) != `{"reason":"oom"}` || action.requests[0].Rule.ID != "restart" {
		t.Fatalf("prepare request=%+v", action.requests)
	}
	snapshot, err := incident.ParseExecutionContext(decision.ExecutionContext)
	if err != nil {
		t.Fatal(err)
	}
	release := testRemediation(incident.ModeManual).Release(testService)
	if snapshot.Kind != incident.KindPrimary || snapshot.Service != "sub2api" || snapshot.Rule.Version != release || snapshot.Rule.Mode != incident.ModeManual ||
		snapshot.ActionVersion != 2 || snapshot.Target != testTarget || snapshot.Revision != action.prepared.Revision || snapshot.FaultAlert != "Sub2APIDown" ||
		strings.Join(snapshot.Members, ",") != "fp-down" || strings.Join(snapshot.EvidenceRefs, ",") != "docker_inspect" {
		t.Fatalf("snapshot=%+v", snapshot)
	}
	v := snapshot.Verification
	if v.IntervalSeconds != 10 || v.WindowSeconds != 300 || v.TimeoutSeconds != 5 || v.RequiredPasses != 3 || v.WatchSeconds != 1800 || len(v.Checks) != 1 {
		t.Fatalf("verification=%+v", v)
	}
	if want := policyNow.Add(30 * time.Minute); !snapshot.ExpiresAt.Equal(want) || !decision.ExpiresAt.Equal(want) {
		t.Fatalf("expiry snapshot=%s decision=%s", snapshot.ExpiresAt, decision.ExpiresAt)
	}
	if hash, err := incident.PlanHash(decision.ToolName, decision.Args, decision.ExecutionContext); err != nil || hash != decision.PlanHash {
		t.Fatalf("hash=%s err=%v; want %s", hash, err, decision.PlanHash)
	}
	if q := state.queries[0]; q.Service != "sub2api" || q.RuleID != "restart" || q.IncidentID != 7 || !q.Since.Equal(policyNow.Add(-time.Hour)) {
		t.Fatalf("state query=%+v", q)
	}
}

// Facts that need a person turn an auto rule's decision into a manual one;
// the snapshot records the mode actually granted.
func TestPolicyDemotesAutoWhenAPersonIsNeeded(t *testing.T) {
	for name, test := range map[string]struct {
		state       store.RemediationState
		input       func(*PolicyInput)
		maintenance bool
		reason      string
	}{
		"rule blocked":        {state: store.RemediationState{Blocked: "verify.failed on approval 3"}, reason: "rule blocked by verify.failed"},
		"second action":       {state: store.RemediationState{IncidentActions: 1}, reason: "already ran in this incident"},
		"monitoring degraded": {input: func(in *PolicyInput) { in.ObservationOK = false }, reason: "monitoring data is unavailable"},
		"maintenance":         {maintenance: true, reason: "maintenance window upgrade"},
	} {
		t.Run(name, func(t *testing.T) {
			remediation := testRemediation(incident.ModeAuto)
			if test.maintenance {
				remediation.Maintenance = []config.MaintenanceWindow{{Start: policyNow.Add(-time.Minute), End: policyNow.Add(time.Hour), Reason: "upgrade"}}
			}
			input := testInput()
			if test.input != nil {
				test.input(&input)
			}
			decision := testPolicy(t, newRestartAction(), remediation, &fakeState{state: test.state}).Decide(context.Background(), llm.Plan{Action: "docker_restart"}, input)
			if decision.Kind != DecisionApproval || !decision.Demoted || !strings.Contains(decision.Reason, test.reason) {
				t.Fatalf("decision=%s demoted=%v reason=%q", decision.Kind, decision.Demoted, decision.Reason)
			}
			snapshot, err := incident.ParseExecutionContext(decision.ExecutionContext)
			if err != nil || snapshot.Rule.Mode != incident.ModeManual {
				t.Fatalf("snapshot mode=%s err=%v", snapshot.Rule.Mode, err)
			}
		})
	}
}

func TestPolicyStateErrorDenies(t *testing.T) {
	decision := testPolicy(t, newRestartAction(), testRemediation(incident.ModeAuto), &fakeState{err: errors.New("db down")}).Decide(context.Background(), llm.Plan{Action: "docker_restart"}, testInput())
	if decision.Kind != DecisionDenied || !strings.Contains(decision.Reason, "remediation state unavailable") {
		t.Fatalf("decision=%+v", decision)
	}
}

func TestAuthorityBindsRulesToEnabledActions(t *testing.T) {
	registry := testRegistry(t, newRestartAction())
	for name, rule := range map[string]config.RuleConfig{
		"disabled action":     {ID: "rollback", Action: "deployment_rollback", Mode: incident.ModeAuto, Alerts: []string{"A"}},
		"compensation action": {ID: "undo", Action: "upstream_restore", Mode: incident.ModeAuto, Alerts: []string{"A"}},
	} {
		remediation := testRemediation(incident.ModeAuto)
		remediation.Rules = append(remediation.Rules, rule)
		if _, err := NewAuthority(testService, remediation, registry); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	remediation := testRemediation(incident.ModeAuto)
	remediation.Maintenance = []config.MaintenanceWindow{{Start: policyNow, End: policyNow.Add(time.Hour)}}
	authority := testAuthority(t, registry, remediation)
	binding := authority.Binding()
	if binding.Service != "sub2api" || binding.RulesVersion != remediation.Release(testService) || binding.Rules["restart"].Mode != incident.ModeAuto ||
		binding.Actions["docker_restart"] != 2 || binding.Actions["upstream_restore"] != 1 {
		t.Fatalf("binding=%+v", binding)
	}
	if p := authority.Policy(policyNow); p.Maintenance != "maintenance window" || p.Budgets["restart"] != (store.RuleBudget{Max: 2, Window: time.Hour}) {
		t.Fatalf("policy=%+v", p)
	}
	if p := authority.Policy(policyNow.Add(time.Hour)); p.Maintenance != "" {
		t.Fatalf("maintenance after the window: %q", p.Maintenance)
	}
}
