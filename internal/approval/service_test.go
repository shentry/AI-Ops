package approval

import (
	"context"
	"encoding/json"
	"errors"
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

func TestPolicyDecisions(t *testing.T) {
	registry := policyTestRegistry(t)
	tests := []struct {
		name string
		cfg  PolicyConfig
		plan llm.Plan
		want string
	}{
		{"none action", PolicyConfig{}, llm.Plan{Action: "none"}, DecisionNone},
		{"empty action", PolicyConfig{}, llm.Plan{}, DecisionNone},
		{"unregistered denied", PolicyConfig{}, planFor("hack_tool"), DecisionDenied},
		{"L1 auto", PolicyConfig{}, planFor("read_metrics"), DecisionAutoL1},
		{"L2 with guardrails", PolicyConfig{AutoExecuteL2: true}, planFor("restart_container"), DecisionAutoL2},
		{"L2 dry run degrades", PolicyConfig{AutoExecuteL2: true, DryRun: true}, planFor("restart_container"), DecisionApproval},
		{"L2 switch off degrades", PolicyConfig{}, planFor("restart_container"), DecisionApproval},
		{"L3 approval", PolicyConfig{AutoExecuteL2: true}, planFor("resize_pool"), DecisionApproval},
		{"L4 forbidden", PolicyConfig{AutoExecuteL2: true}, planFor("drop_database"), DecisionDenied},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision := NewPolicy(registry, test.cfg).Decide(test.plan)
			if decision.Kind != test.want {
				t.Fatalf("Decide() = %q, want %q", decision.Kind, test.want)
			}
		})
	}
}

func TestPlanHashBindsContent(t *testing.T) {
	registry := policyTestRegistry(t)
	policy := NewPolicy(registry, PolicyConfig{})
	a := policy.Decide(planFor("resize_pool"))
	b := policy.Decide(planFor("resize_pool"))
	if a.PlanHash == "" || a.PlanHash != b.PlanHash {
		t.Fatalf("plan hash not stable: %q vs %q", a.PlanHash, b.PlanHash)
	}
	// 换 target 后 hash 必须变 —— 审批绑定内容。
	changed := planFor("resize_pool")
	changed.Target.Name = "other"
	if policy.Decide(changed).PlanHash == a.PlanHash {
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

func (f *fakeApprovalStore) DecideApproval(_ context.Context, id uint64, status, decidedBy string, now time.Time) error {
	a, ok := f.approvals[id]
	if !ok {
		return store.ErrApprovalNotFound
	}
	if a.Status != "pending" || !now.Before(a.ExpiresAt) {
		return store.ErrApprovalConflict
	}
	a.Status = status
	a.DecidedBy = &decidedBy
	f.approvals[id] = a
	return nil
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
	if err := svc.Decide(ctx, created.ID, true, "ops"); err != nil {
		t.Fatal(err)
	}
	if err := svc.Decide(ctx, created.ID, true, "ops"); !errors.Is(err, store.ErrApprovalConflict) {
		t.Fatalf("re-approve error = %v, want conflict", err)
	}
	// deny 已决单同样冲突。
	if err := svc.Decide(ctx, created.ID, false, "ops"); !errors.Is(err, store.ErrApprovalConflict) {
		t.Fatalf("deny after approve error = %v, want conflict", err)
	}
	// 不存在 404。
	if err := svc.Decide(ctx, 999, true, "ops"); !errors.Is(err, store.ErrApprovalNotFound) {
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
	if err := svc.Decide(ctx, created.ID, true, "ops"); err != nil {
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
