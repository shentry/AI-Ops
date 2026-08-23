package approval

import (
	"context"
	"encoding/json"

	"errors"
	"gorm.io/datatypes"
	"sync"
	"testing"
	"time"

	"oncall-agent/internal/store"
	"oncall-agent/internal/tools"
)

// fakeExecStore 模拟执行队列。
type fakeExecStore struct {
	mu        sync.Mutex
	approvals map[uint64]*store.Approval
	order     []uint64
	finished  map[uint64]string
	history   []store.FaultCmdHistory
	incident  store.Incident
	members   []store.IncidentMember
	runs      map[uint64]store.AgentRun
	steps     []store.AgentRunStep
}

func newFakeExecStore() *fakeExecStore {
	return &fakeExecStore{
		approvals: map[uint64]*store.Approval{},
		finished:  map[uint64]string{},
		incident:  store.Incident{ID: 1, GroupKey: "payments"},
		members:   []store.IncidentMember{{Name: "HighCPU", Status: "resolved"}},
		runs:      map[uint64]store.AgentRun{},
	}
}

func (f *fakeExecStore) add(a store.Approval) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.approvals[a.ID] = &a
	f.order = append(f.order, a.ID)
}

func (f *fakeExecStore) NextApprovedApproval(_ context.Context, now time.Time) (store.Approval, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range f.order {
		a := f.approvals[id]
		if a.Status == "approved" && now.Before(a.ExpiresAt) {
			return *a, true, nil
		}
	}
	return store.Approval{}, false, nil
}

func (f *fakeExecStore) ClaimApprovalExecution(_ context.Context, id uint64, now time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.approvals[id]
	if !ok || a.Status != "approved" || !now.Before(a.ExpiresAt) {
		return false, nil
	}
	a.Status = "executing"
	return true, nil
}

func (f *fakeExecStore) FinishApprovalExecution(_ context.Context, id uint64, status string, resultJSON []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.approvals[id]
	if !ok || a.Status != "executing" {
		return errors.New("not executing")
	}
	a.Status = status
	if len(resultJSON) > 0 {
		j := datatypes.JSON(append([]byte(nil), resultJSON...))
		a.ResultJSON = &j
	}
	f.finished[id] = status
	return nil
}

func (f *fakeExecStore) RecoverExecutingApprovals(_ context.Context, _ time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var recovered int64
	for _, a := range f.approvals {
		if a.Status == "executing" {
			a.Status = "failed"
			recovered++
		}
	}
	return recovered, nil
}

func (f *fakeExecStore) InsertFaultCmdHistory(_ context.Context, row store.FaultCmdHistory) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.history = append(f.history, row)
	return nil
}

func (f *fakeExecStore) GetIncident(context.Context, uint64) (store.Incident, error) {
	return f.incident, nil
}

func (f *fakeExecStore) ListIncidentMembers(context.Context, uint64) ([]store.IncidentMember, error) {
	return f.members, nil
}

func (f *fakeExecStore) GetAgentRun(_ context.Context, id uint64) (store.AgentRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runs[id], nil
}

func (f *fakeExecStore) ListRunSteps(_ context.Context, runID uint64) ([]store.AgentRunStep, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]store.AgentRunStep, 0)
	for _, step := range f.steps {
		if step.RunID == runID {
			out = append(out, step)
		}
	}
	return out, nil
}

type fakeVerifier struct {
	mu    sync.Mutex
	calls []uint64
}

func (f *fakeVerifier) VerifyAfterExecution(_ context.Context, runID, _ uint64, _ time.Duration) VerifyOutcome {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, runID)
	return VerifyOutcome{Passed: true, Detail: "ok"}
}

func executorTestRegistry(t *testing.T, executions *int) *tools.Registry {
	t.Helper()
	registry := tools.NewRegistry()
	if err := registry.Register(tools.ToolSpec{
		Name: "docker_restart", Description: "restart", Level: tools.L2LowRisk,
		Timeout: 5 * time.Second, MaxOutput: 512,
		Handler: func(context.Context, json.RawMessage) (string, error) {
			*executions++
			return `{"restarted":"sub2api"}`, nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	return registry
}

func approvedApproval(id uint64, planHash string) store.Approval {
	args := []byte(`{"target_name":"sub2api"}`)
	if planHash == "" {
		planHash = PlanHash("docker_restart", args)
	}
	return store.Approval{
		ID: id, IncidentID: 1, RunID: 2, ToolName: "docker_restart", ArgsJSON: args,
		PlanHash: planHash, Status: "approved",
		ExpiresAt: time.Now().Add(30 * time.Minute), CreatedAt: time.Now(),
	}
}

func TestExecutorRunsApprovedApproval(t *testing.T) {
	executions := 0
	fake := newFakeExecStore()
	fake.add(approvedApproval(1, ""))
	verifier := &fakeVerifier{}
	executor := NewExecutor(fake, executorTestRegistry(t, &executions), false, 0, verifier, nil, nil, nil)

	executor.drain(context.Background())
	if executions != 1 {
		t.Fatalf("executions = %d, want 1", executions)
	}
	if fake.finished[1] != "executed" {
		t.Fatalf("status = %q", fake.finished[1])
	}
	if len(fake.history) != 1 || fake.history[0].ToolName != "docker_restart" || fake.history[0].ApprovalID == nil {
		t.Fatalf("history = %+v", fake.history)
	}
	if len(verifier.calls) != 1 {
		t.Fatalf("verify calls = %d", len(verifier.calls))
	}
	// 幂等：再 drain 一次不重复执行。
	executor.drain(context.Background())
	if executions != 1 {
		t.Fatalf("second drain executed %d times", executions)
	}
}

func TestExecutorRejectsHashMismatch(t *testing.T) {
	executions := 0
	fake := newFakeExecStore()
	fake.add(approvedApproval(1, "tampered-hash"))
	executor := NewExecutor(fake, executorTestRegistry(t, &executions), false, 0, &fakeVerifier{}, nil, nil, nil)
	executor.drain(context.Background())
	if executions != 0 {
		t.Fatal("tampered approval was executed")
	}
	if fake.finished[1] != "failed" {
		t.Fatalf("status = %q, want failed", fake.finished[1])
	}
}

func TestExecutorRejectsUnregisteredTool(t *testing.T) {
	fake := newFakeExecStore()
	args := []byte(`{}`)
	fake.add(store.Approval{
		ID: 1, IncidentID: 1, RunID: 2, ToolName: "ghost_tool", ArgsJSON: args,
		PlanHash: PlanHash("ghost_tool", args), Status: "approved",
		ExpiresAt: time.Now().Add(time.Hour), CreatedAt: time.Now(),
	})
	executor := NewExecutor(fake, tools.NewRegistry(), false, 0, &fakeVerifier{}, nil, nil, nil)
	executor.drain(context.Background())
	if fake.finished[1] != "failed" {
		t.Fatalf("status = %q, want failed", fake.finished[1])
	}
}

func TestExecutorDryRunSkipsExecutionAndVerify(t *testing.T) {
	executions := 0
	fake := newFakeExecStore()
	fake.add(approvedApproval(1, ""))
	verifier := &fakeVerifier{}
	executor := NewExecutor(fake, executorTestRegistry(t, &executions), true, 0, verifier, nil, nil, nil)
	executor.drain(context.Background())
	if executions != 0 {
		t.Fatal("dry-run executed the tool")
	}
	if fake.finished[1] != "executed" {
		t.Fatalf("status = %q", fake.finished[1])
	}
	if len(verifier.calls) != 0 {
		t.Fatalf("verify called %d times in dry-run", len(verifier.calls))
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(*fake.approvals[1].ResultJSON), &result); err != nil {
		t.Fatal(err)
	}
	if result["dry_run"] != true {
		t.Fatalf("result = %v", result)
	}
}

func TestExecutorSkipsExpired(t *testing.T) {
	executions := 0
	fake := newFakeExecStore()
	expired := approvedApproval(1, "")
	expired.ExpiresAt = time.Now().Add(-time.Minute)
	fake.add(expired)
	executor := NewExecutor(fake, executorTestRegistry(t, &executions), false, 0, &fakeVerifier{}, nil, nil, nil)
	executor.drain(context.Background())
	if executions != 0 || len(fake.finished) != 0 {
		t.Fatalf("expired approval executed: executions=%d finished=%v", executions, fake.finished)
	}
}

// fakeRetry 记录重诊调度调用。
type fakeRetry struct {
	calls   []uint64
	created bool
}

func (f *fakeRetry) ScheduleRetry(_ context.Context, incidentID, _ uint64, _ string) (bool, error) {
	f.calls = append(f.calls, incidentID)
	return f.created, nil
}

func TestExecutorVerifyFailureSchedulesRetry(t *testing.T) {
	executions := 0
	fake := newFakeExecStore()
	fake.add(approvedApproval(1, ""))
	retry := &fakeRetry{created: true}
	// verify 失败：members 里还有 firing。
	fake.members = []store.IncidentMember{{Name: "HighCPU", Status: "firing"}}
	executor := NewExecutor(fake, executorTestRegistry(t, &executions), false, 0, &failingVerifier{}, retry, nil, nil)
	executor.drain(context.Background())
	if executions != 1 {
		t.Fatalf("executions = %d", executions)
	}
	if fake.finished[1] != "executed" {
		t.Fatalf("status = %q", fake.finished[1])
	}
	if len(retry.calls) != 1 || retry.calls[0] != 1 {
		t.Fatalf("retry calls = %v", retry.calls)
	}
}

func TestExecutorVerifyPassSkipsRetry(t *testing.T) {
	executions := 0
	fake := newFakeExecStore()
	fake.add(approvedApproval(1, ""))
	retry := &fakeRetry{}
	executor := NewExecutor(fake, executorTestRegistry(t, &executions), false, 0, &fakeVerifier{}, retry, nil, nil)
	executor.drain(context.Background())
	if len(retry.calls) != 0 {
		t.Fatalf("retry called on pass: %v", retry.calls)
	}
}

type failingVerifier struct{}

func (failingVerifier) VerifyAfterExecution(context.Context, uint64, uint64, time.Duration) VerifyOutcome {
	return VerifyOutcome{Passed: false, Detail: "still firing"}
}

// inconclusiveVerifier 模拟"没能判定"：进程正在关闭、读库失败、
// 或 incident 没有可复查的成员。
type inconclusiveVerifier struct{}

func (inconclusiveVerifier) VerifyAfterExecution(context.Context, uint64, uint64, time.Duration) VerifyOutcome {
	return VerifyOutcome{Inconclusive: true, Detail: "verify canceled before recheck"}
}

// 不可判定的验证不能被当成失败：既不重诊也不降级记忆，
// 更不能当成成功写进故障记忆。动作本身已执行，审批仍落 executed。
func TestExecutorInconclusiveVerifySkipsRetryAndMemory(t *testing.T) {
	executions := 0
	fake := newFakeExecStore()
	fake.add(approvedApproval(1, ""))
	planJSON := datatypes.JSON([]byte(`{"action":"docker_restart","confidence":"high"}`))
	fake.runs[2] = store.AgentRun{ID: 2, Mode: "memory_hit", PlanJSON: &planJSON}
	mem := &fakeMemoryWriter{}
	retry := &fakeRetry{created: true}
	executor := NewExecutor(fake, executorTestRegistry(t, &executions), false, 0, inconclusiveVerifier{}, retry, mem, nil)
	executor.drain(context.Background())
	if executions != 1 {
		t.Fatalf("executions = %d, want 1", executions)
	}
	if fake.finished[1] != "executed" {
		t.Fatalf("status = %q, want executed", fake.finished[1])
	}
	if len(retry.calls) != 0 {
		t.Fatalf("retry scheduled on inconclusive verify: %v", retry.calls)
	}
	if len(mem.demoted) != 0 || len(mem.committed) != 0 {
		t.Fatalf("memory touched on inconclusive verify: committed=%v demoted=%v", mem.committed, mem.demoted)
	}
}

func TestExecutorToolFailureMarksFailed(t *testing.T) {
	// target 消失：容器已被删，docker_restart 返回 container not found。
	// 必须 approval=failed、executed=false、且不触发重诊（执行失败 ≠ verify 失败）。
	fake := newFakeExecStore()
	fake.add(approvedApproval(1, ""))
	retry := &fakeRetry{}
	registry := tools.NewRegistry()
	if err := registry.Register(tools.ToolSpec{
		Name: "docker_restart", Description: "restart", Level: tools.L2LowRisk,
		Timeout: 5 * time.Second, MaxOutput: 512,
		Handler: func(context.Context, json.RawMessage) (string, error) {
			return "", errors.New("container not found")
		},
	}); err != nil {
		t.Fatal(err)
	}
	executor := NewExecutor(fake, registry, false, 0, &fakeVerifier{}, retry, nil, nil)
	executor.drain(context.Background())
	if fake.finished[1] != "failed" {
		t.Fatalf("status = %q, want failed", fake.finished[1])
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(*fake.approvals[1].ResultJSON), &result); err != nil {
		t.Fatal(err)
	}
	if result["executed"] != false {
		t.Fatalf("result = %v, want executed=false", result)
	}
	if len(retry.calls) != 0 {
		t.Fatalf("retry scheduled on exec failure: %v", retry.calls)
	}
}

// fakeMemoryWriter 记录 commit/demote。
type fakeMemoryWriter struct {
	committed []store.FaultMemory
	demoted   []string
}

func (f *fakeMemoryWriter) Commit(_ context.Context, entry store.FaultMemory) error {
	f.committed = append(f.committed, entry)
	return nil
}

func (f *fakeMemoryWriter) Demote(_ context.Context, fp string) error {
	f.demoted = append(f.demoted, fp)
	return nil
}

func TestExecutorCommitsMemoryOnVerifiedSuccess(t *testing.T) {
	executions := 0
	fake := newFakeExecStore()
	fake.add(approvedApproval(1, ""))
	rca := "根因"
	planJSON := datatypes.JSON([]byte(`{"action":"docker_restart","confidence":"high"}`))
	fake.runs[2] = store.AgentRun{ID: 2, Mode: "full", RCAText: &rca, PlanJSON: &planJSON}
	mem := &fakeMemoryWriter{}
	executor := NewExecutor(fake, executorTestRegistry(t, &executions), false, 0, &fakeVerifier{}, nil, mem, nil)
	executor.drain(context.Background())
	if len(mem.committed) != 1 {
		t.Fatalf("committed = %d, want 1", len(mem.committed))
	}
	entry := mem.committed[0]
	if entry.GroupKey != "payments" || entry.AlertName != "HighCPU" || entry.Confidence != "high" || entry.Fingerprint == "" {
		t.Fatalf("entry = %+v", entry)
	}
}

func TestExecutorSkipsMemoryForRetryOrLowConfidence(t *testing.T) {
	executions := 0
	fake := newFakeExecStore()
	fake.add(approvedApproval(1, ""))
	retryOf := uint64(9)
	planJSON := datatypes.JSON([]byte(`{"action":"docker_restart","confidence":"high"}`))
	fake.runs[2] = store.AgentRun{ID: 2, Mode: "full", RetryOf: &retryOf, PlanJSON: &planJSON}
	mem := &fakeMemoryWriter{}
	executor := NewExecutor(fake, executorTestRegistry(t, &executions), false, 0, &fakeVerifier{}, nil, mem, nil)
	executor.drain(context.Background())
	if len(mem.committed) != 0 {
		t.Fatalf("retry run committed memory: %+v", mem.committed)
	}
}

func TestExecutorDemotesMemoryOnHitVerifyFailure(t *testing.T) {
	executions := 0
	fake := newFakeExecStore()
	fake.add(approvedApproval(1, ""))
	fake.runs[2] = store.AgentRun{ID: 2, Mode: "memory_hit"}
	fake.members = []store.IncidentMember{{Name: "HighCPU", Status: "firing"}}
	mem := &fakeMemoryWriter{}
	retry := &fakeRetry{}
	executor := NewExecutor(fake, executorTestRegistry(t, &executions), false, 0, &failingVerifier{}, retry, mem, nil)
	executor.drain(context.Background())
	if len(mem.demoted) != 1 {
		t.Fatalf("demoted = %v, want 1", mem.demoted)
	}
	if len(retry.calls) != 1 {
		t.Fatalf("retry calls = %v, want 1 (完整重诊)", retry.calls)
	}
}
