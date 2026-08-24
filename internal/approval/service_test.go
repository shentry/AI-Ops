package approval

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"oncall-agent/internal/llm"
	"oncall-agent/internal/store"
	"oncall-agent/internal/tools"
)

func policyTestRegistry(t *testing.T) *tools.Registry {
	t.Helper()
	registry := tools.NewRegistry()
	noop := func(context.Context, json.RawMessage) (string, error) { return "ok", nil }
	for _, spec := range []tools.ToolSpec{
		{Name: "read_metrics", Level: tools.L1ReadOnly, Description: "read", Timeout: time.Second, Handler: noop},
		{Name: "restart_container", Level: tools.L2LowRisk, Description: "restart", Timeout: time.Second, Handler: noop},
		{Name: "resize_pool", Level: tools.L3Approval, Description: "resize", Timeout: time.Second, Handler: noop},
		{Name: "drop_database", Level: tools.L4Forbidden, Description: "drop", Timeout: time.Second, Handler: noop},
	} {
		if err := registry.Register(spec); err != nil {
			t.Fatal(err)
		}
	}
	return registry
}

func planFor(action string) llm.Plan {
	return llm.Plan{Action: action, Target: llm.PlanTarget{Kind: "container", Name: "sub2api"}}
}

// fakeExecutionCounter 是限频护栏的假数据源：count 是窗口内已执行次数。
type fakeExecutionCounter struct {
	count int
	err   error
	calls int
}

func (f *fakeExecutionCounter) CountRecentExecutions(context.Context, string, time.Time) (int, error) {
	f.calls++
	return f.count, f.err
}

// l2PolicyConfig 是"护栏全满足"的基线配置，各用例只改要测的那一条。
func l2PolicyConfig() PolicyConfig {
	return PolicyConfig{
		AutoExecuteL2:  true,
		AllowedTargets: []string{"sub2api"},
		RateWindow:     time.Hour,
		MaxPerWindow:   1,
	}
}

// l2Input 是"来源可信 + 可验证"的基线输入。
func l2Input() PolicyInput {
	return PolicyInput{KnownTargets: []string{"sub2api", "10.0.0.1:9100"}, Verifiable: true}
}

func TestPolicyDecisions(t *testing.T) {
	registry := policyTestRegistry(t)
	tests := []struct {
		name  string
		cfg   PolicyConfig
		input PolicyInput
		plan  llm.Plan
		want  string
	}{
		{"none action", PolicyConfig{}, l2Input(), llm.Plan{Action: "none"}, DecisionNone},
		{"empty action", PolicyConfig{}, l2Input(), llm.Plan{}, DecisionNone},
		{"unregistered denied", PolicyConfig{}, l2Input(), planFor("hack_tool"), DecisionDenied},
		{"L1 auto", PolicyConfig{}, l2Input(), planFor("read_metrics"), DecisionAutoL1},
		{"L2 with all guardrails", l2PolicyConfig(), l2Input(), planFor("restart_container"), DecisionAutoL2},
		{"L3 approval", l2PolicyConfig(), l2Input(), planFor("resize_pool"), DecisionApproval},
		{"L4 forbidden", l2PolicyConfig(), l2Input(), planFor("drop_database"), DecisionDenied},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision := NewPolicy(registry, test.cfg, &fakeExecutionCounter{}).Decide(context.Background(), test.plan, test.input)
			if decision.Kind != test.want {
				t.Fatalf("Decide() = %q (%s), want %q", decision.Kind, decision.Reason, test.want)
			}
		})
	}
}

// L2 的每一条护栏单独失守都必须降级审批（设计 6.1：白名单、真实 target、
// 影响范围受限、限频、全局开关、非 dry-run、可验证，缺一不可）。
func TestPolicyL2GuardrailsEachDegradeToApproval(t *testing.T) {
	registry := policyTestRegistry(t)
	wildcard := planFor("restart_container")
	wildcard.Target.Name = "sub2api-*"
	broad := planFor("restart_container")
	broad.Target.Kind = "cluster"
	noTarget := planFor("restart_container")
	noTarget.Target.Name = ""
	offAllowlist := planFor("restart_container")
	offAllowlist.Target.Name = "postgres"

	tests := []struct {
		name    string
		cfg     PolicyConfig
		input   PolicyInput
		plan    llm.Plan
		counter *fakeExecutionCounter
		reason  string
	}{
		{
			name: "global switch off", cfg: PolicyConfig{AllowedTargets: []string{"sub2api"}, RateWindow: time.Hour, MaxPerWindow: 1},
			input: l2Input(), plan: planFor("restart_container"), counter: &fakeExecutionCounter{}, reason: "auto_execute_l2 disabled",
		},
		{
			name: "dry run", cfg: withDryRun(l2PolicyConfig()),
			input: l2Input(), plan: planFor("restart_container"), counter: &fakeExecutionCounter{}, reason: "dry_run enabled",
		},
		{
			name: "incomplete target", cfg: l2PolicyConfig(),
			input: l2Input(), plan: noTarget, counter: &fakeExecutionCounter{}, reason: "target is incomplete",
		},
		{
			name: "wildcard target", cfg: l2PolicyConfig(),
			input: l2Input(), plan: wildcard, counter: &fakeExecutionCounter{}, reason: "single concrete object",
		},
		{
			name: "broad blast radius", cfg: l2PolicyConfig(),
			input: l2Input(), plan: broad, counter: &fakeExecutionCounter{}, reason: "unbounded blast radius",
		},
		{
			name: "target not allowlisted", cfg: l2PolicyConfig(),
			input: l2Input(), plan: offAllowlist, counter: &fakeExecutionCounter{}, reason: "auto-execute allowlist",
		},
		{
			name: "target not from alert labels", cfg: l2PolicyConfig(),
			input: PolicyInput{KnownTargets: []string{"other-service"}, Verifiable: true},
			plan:  planFor("restart_container"), counter: &fakeExecutionCounter{}, reason: "does not come from alert labels",
		},
		{
			name: "outcome not verifiable", cfg: l2PolicyConfig(),
			input: PolicyInput{KnownTargets: []string{"sub2api"}},
			plan:  planFor("restart_container"), counter: &fakeExecutionCounter{}, reason: "not verifiable",
		},
		{
			name: "rate limit reached", cfg: l2PolicyConfig(),
			input: l2Input(), plan: planFor("restart_container"), counter: &fakeExecutionCounter{count: 1}, reason: "rate limit reached",
		},
		{
			name: "rate limit unreadable fails closed", cfg: l2PolicyConfig(),
			input: l2Input(), plan: planFor("restart_container"),
			counter: &fakeExecutionCounter{err: errors.New("db down")}, reason: "rate limit check failed",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision := NewPolicy(registry, test.cfg, test.counter).Decide(context.Background(), test.plan, test.input)
			if decision.Kind != DecisionApproval {
				t.Fatalf("Decide() = %q (%s), want approval", decision.Kind, decision.Reason)
			}
			if !strings.Contains(decision.Reason, test.reason) {
				t.Fatalf("reason = %q, want it to mention %q", decision.Reason, test.reason)
			}
		})
	}
}

// 没有限频数据源时 L2 自动路径不许放行（fail closed）。
func TestPolicyL2WithoutRateCounterDegrades(t *testing.T) {
	decision := NewPolicy(policyTestRegistry(t), l2PolicyConfig(), nil).
		Decide(context.Background(), planFor("restart_container"), l2Input())
	if decision.Kind != DecisionApproval || !strings.Contains(decision.Reason, "rate limit is not configured") {
		t.Fatalf("decision = %q (%s), want approval on missing rate counter", decision.Kind, decision.Reason)
	}
}

// instance 标签常见的 host:port 形态也算可信来源。
func TestPolicyAcceptsHostPortProvenance(t *testing.T) {
	plan := planFor("restart_container")
	plan.Target.Name = "gateway-1"
	cfg := l2PolicyConfig()
	cfg.AllowedTargets = []string{"gateway-1"}
	decision := NewPolicy(policyTestRegistry(t), cfg, &fakeExecutionCounter{}).
		Decide(context.Background(), plan, PolicyInput{KnownTargets: []string{"gateway-1:9100"}, Verifiable: true})
	if decision.Kind != DecisionAutoL2 {
		t.Fatalf("decision = %q (%s), want auto_l2", decision.Kind, decision.Reason)
	}
}

func withDryRun(cfg PolicyConfig) PolicyConfig {
	cfg.DryRun = true
	return cfg
}

func TestPlanHashBindsContent(t *testing.T) {
	registry := policyTestRegistry(t)
	policy := NewPolicy(registry, PolicyConfig{}, &fakeExecutionCounter{})
	ctx := context.Background()
	a := policy.Decide(ctx, planFor("resize_pool"), l2Input())
	b := policy.Decide(ctx, planFor("resize_pool"), l2Input())
	if a.PlanHash == "" || a.PlanHash != b.PlanHash {
		t.Fatalf("plan hash not stable: %q vs %q", a.PlanHash, b.PlanHash)
	}
	// 换 target 后 hash 必须变 —— 审批绑定内容。
	changed := planFor("resize_pool")
	changed.Target.Name = "other"
	if policy.Decide(ctx, changed, l2Input()).PlanHash == a.PlanHash {
		t.Fatal("plan hash did not change with target")
	}
}

// fakeApprovalStore 记录审批单。
type fakeApprovalStore struct {
	approvals map[uint64]store.Approval
	nextID    uint64
}

func newFakeApprovalStore() *fakeApprovalStore {
	return &fakeApprovalStore{approvals: map[uint64]store.Approval{}, nextID: 1}
}

func (f *fakeApprovalStore) CreateApproval(_ context.Context, a store.Approval) (store.Approval, error) {
	a.ID = f.nextID
	f.nextID++
	a.Status = "pending"
	f.approvals[a.ID] = a
	return a, nil
}

func (f *fakeApprovalStore) GetApproval(_ context.Context, id uint64) (store.Approval, error) {
	if a, ok := f.approvals[id]; ok {
		return a, nil
	}
	return store.Approval{}, store.ErrApprovalNotFound
}

func (f *fakeApprovalStore) DecideApproval(_ context.Context, id uint64, status, decidedBy, decisionReason, decisionSource string, now time.Time) (store.Approval, error) {
	a, ok := f.approvals[id]
	if !ok {
		return store.Approval{}, store.ErrApprovalNotFound
	}
	if a.Status != "pending" || !now.Before(a.ExpiresAt) {
		return store.Approval{}, store.ErrApprovalConflict
	}
	a.Status = status
	a.DecidedBy = &decidedBy
	a.DecidedAt = &now
	a.DecisionReason = &decisionReason
	a.DecisionSource = &decisionSource
	f.approvals[id] = a
	return a, nil
}

func (f *fakeApprovalStore) CreateSystemApprovedApproval(ctx context.Context, a store.Approval, decidedBy, decisionReason, decisionSource string, now time.Time) (store.Approval, error) {
	created, err := f.CreateApproval(ctx, a)
	if err != nil {
		return store.Approval{}, err
	}
	return f.DecideApproval(ctx, created.ID, "approved", decidedBy, decisionReason, decisionSource, now)
}

func (f *fakeApprovalStore) ListApprovals(_ context.Context, status string) ([]store.Approval, error) {
	out := make([]store.Approval, 0)
	for _, a := range f.approvals {
		if status == "" || a.Status == status {
			out = append(out, a)
		}
	}
	return out, nil
}

func TestServiceLifecycle(t *testing.T) {
	svc := NewService(newFakeApprovalStore(), 30)
	ctx := context.Background()
	decision := Decision{Kind: DecisionApproval, ToolName: "resize_pool", Args: []byte(`{"target_name":"sub2api"}`), PlanHash: "hash1"}

	created, err := svc.Create(ctx, 7, 11, decision, "L3 requires approval")
	if err != nil {
		t.Fatal(err)
	}
	if created.Status != "pending" || created.ExpiresAt.Before(time.Now()) {
		t.Fatalf("created = %+v", created)
	}

	// approve → 幂等冲突：第二次 409。
	if _, err := svc.Decide(ctx, created.ID, true, "ops", "", "api"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Decide(ctx, created.ID, true, "ops", "", "api"); !errors.Is(err, store.ErrApprovalConflict) {
		t.Fatalf("re-approve error = %v, want conflict", err)
	}
	// deny 已决单同样冲突。
	if _, err := svc.Decide(ctx, created.ID, false, "ops", "", "api"); !errors.Is(err, store.ErrApprovalConflict) {
		t.Fatalf("deny after approve error = %v, want conflict", err)
	}
	// 不存在 404。
	if _, err := svc.Decide(ctx, 999, true, "ops", "", "api"); !errors.Is(err, store.ErrApprovalNotFound) {
		t.Fatalf("missing error = %v, want not found", err)
	}
}

func TestValidateExecution(t *testing.T) {
	svc := NewService(newFakeApprovalStore(), 30)
	ctx := context.Background()
	decision := Decision{Kind: DecisionApproval, ToolName: "resize_pool", Args: []byte(`{"target_name":"sub2api"}`), PlanHash: "hash1"}
	created, _ := svc.Create(ctx, 7, 11, decision, "reason")

	// 未批准不能执行。
	if err := svc.ValidateExecution(ctx, created.ID, decision); err == nil {
		t.Fatal("pending approval passed execution validation")
	}
	if _, err := svc.Decide(ctx, created.ID, true, "ops", "", "api"); err != nil {
		t.Fatal(err)
	}
	if err := svc.ValidateExecution(ctx, created.ID, decision); err != nil {
		t.Fatalf("approved validation error = %v", err)
	}
	// 篡改 hash/args/tool 全部被拒。
	for _, tampered := range []Decision{
		{ToolName: "resize_pool", Args: decision.Args, PlanHash: "other"},
		{ToolName: "resize_pool", Args: []byte(`{"target_name":"other"}`), PlanHash: "hash1"},
		{ToolName: "drop_database", Args: decision.Args, PlanHash: "hash1"},
	} {
		if err := svc.ValidateExecution(ctx, created.ID, tampered); err == nil {
			t.Fatalf("tampered decision %+v passed validation", tampered)
		}
	}
}

func TestValidateExecutionExpired(t *testing.T) {
	fake := newFakeApprovalStore()
	svc := NewService(fake, 30)
	ctx := context.Background()
	decision := Decision{Kind: DecisionApproval, ToolName: "x", Args: []byte(`{}`), PlanHash: "h"}
	created, _ := svc.Create(ctx, 1, 1, decision, "r")
	// 手工把审批单改成已批准但已过期。
	a := fake.approvals[created.ID]
	a.Status = "approved"
	a.ExpiresAt = time.Now().Add(-time.Minute)
	fake.approvals[created.ID] = a
	if err := svc.ValidateExecution(ctx, created.ID, decision); err == nil {
		t.Fatal("expired approval passed execution validation")
	}
}

func TestPlanHashSurvivesMySQLNormalization(t *testing.T) {
	// MySQL JSON 列会重排键序和空白；入库读出后重算必须仍命中。
	args := []byte(`{"target_name":"sub2api","target_kind":"container"}`)
	base := PlanHash("docker_restart", args)
	reformatted := []byte(`{ "target_kind" : "container", "target_name" : "sub2api" }`)
	if got := PlanHash("docker_restart", reformatted); got != base {
		t.Fatalf("hash changed after reformat: %q vs %q", base, got)
	}
	// 值变了必须失配。
	changed := []byte(`{"target_kind":"container","target_name":"other"}`)
	if got := PlanHash("docker_restart", changed); got == base {
		t.Fatal("hash did not change with value")
	}
}
