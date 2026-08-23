package diagnose

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/datatypes"

	"oncall-agent/internal/approval"
	"oncall-agent/internal/llm"
	"oncall-agent/internal/store"
)

// fakeRunStore 记录 step 与终态，不碰真库。
type fakeRunStore struct {
	mu          sync.Mutex
	steps       []store.AgentRunStep
	completed   map[uint64]string // run id -> final status
	completeRCA map[uint64]string
	tokensIn    map[uint64]int
	runs        map[uint64]store.AgentRun // 预置的重诊上下文数据
}

func newFakeRunStore() *fakeRunStore {
	return &fakeRunStore{completed: map[uint64]string{}, completeRCA: map[uint64]string{}, tokensIn: map[uint64]int{}, runs: map[uint64]store.AgentRun{}}
}

func (f *fakeRunStore) AppendRunStep(_ context.Context, step store.AgentRunStep) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.steps = append(f.steps, step)
	return nil
}

func (f *fakeRunStore) AppendRunStepRecord(ctx context.Context, record store.RunStepRecord) error {
	return f.AppendRunStep(ctx, record.Step)
}

func (f *fakeRunStore) CompleteAgentRun(_ context.Context, id uint64, rca string, _ []byte, tokensIn, _ int, status string, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.completed[id] = status
	f.completeRCA[id] = rca
	f.tokensIn[id] = tokensIn
	return nil
}

func (f *fakeRunStore) CompleteRun(ctx context.Context, completion store.RunCompletion) error {
	return f.CompleteAgentRun(ctx, completion.RunID, completion.RCA, completion.PlanJSON, completion.TokensIn, completion.TokensOut, completion.Status, completion.FinishedAt)
}

func (f *fakeRunStore) AppendIncidentEvent(_ context.Context, event store.IncidentEvent) (store.IncidentEvent, error) {
	return event, nil
}

func (f *fakeRunStore) GetAgentRun(_ context.Context, id uint64) (store.AgentRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runs[id], nil
}

func (f *fakeRunStore) ListRunSteps(_ context.Context, runID uint64) ([]store.AgentRunStep, error) {
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

func (f *fakeRunStore) UpdateAgentRunMode(_ context.Context, id uint64, mode string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	run := f.runs[id]
	run.Mode = mode
	f.runs[id] = run
	return nil
}

type fakeEvidenceBuilder struct {
	evidence Evidence
	err      error
	target   Target
}

func (f fakeEvidenceBuilder) BuildForIncident(context.Context, uint64) (Evidence, error) {
	return f.evidence, f.err
}

// LoadTarget 返回预置 target；零值时给一个带告警名的最小 target，
// 让记忆指纹路径可测。
func (f fakeEvidenceBuilder) LoadTarget(context.Context, uint64) (Target, error) {
	if len(f.target.Alerts) > 0 || f.target.Incident.ID != 0 {
		return f.target, nil
	}
	return Target{Incident: store.Incident{ID: 7}}, nil
}

type fakeReasoner struct {
	result *llm.DiagnoseResult
	err    error
	calls  int
}

func (f *fakeReasoner) Diagnose(context.Context, string, string) (*llm.DiagnoseResult, error) {
	f.calls++
	return f.result, f.err
}

type fakeReporter struct {
	calls int
	err   error
}

func (f *fakeReporter) NotifyDiagnosis(context.Context, DiagnosisReport) error {
	f.calls++
	return f.err
}

// fakePolicy 固定返回一个决策，并记下收到的 PolicyInput（护栏事实输入）。
type fakePolicy struct {
	decision  approval.Decision
	lastInput *approval.PolicyInput
}

func (f *fakePolicy) Decide(_ context.Context, _ llm.Plan, input approval.PolicyInput) approval.Decision {
	f.lastInput = &input
	return f.decision
}

type fakeApprovals struct {
	created []store.Approval
	err     error
}

func (f *fakeApprovals) Create(_ context.Context, incidentID, runID uint64, decision approval.Decision, reason string) (store.Approval, error) {
	if f.err != nil {
		return store.Approval{}, f.err
	}
	created := store.Approval{ID: uint64(len(f.created) + 1), IncidentID: incidentID, RunID: runID, ToolName: decision.ToolName, Reason: reason, Status: "pending"}
	f.created = append(f.created, created)
	return created, nil
}

func (f *fakeApprovals) CreateSystemApproved(ctx context.Context, incidentID, runID uint64, decision approval.Decision, reason string) (store.Approval, error) {
	created, err := f.Create(ctx, incidentID, runID, decision, reason)
	if err != nil {
		return store.Approval{}, err
	}
	created.Status = "approved"
	f.created[len(f.created)-1].Status = "approved"
	return created, nil
}

// allowPolicy 是默认策略桩：全部放行（决策 none）。
func allowPolicy() *fakePolicy {
	return &fakePolicy{decision: approval.Decision{Kind: approval.DecisionNone, Reason: "no action"}}
}

func testEvidence() Evidence {
	return Evidence{IncidentID: 7, Items: []EvidenceItem{{Name: "snapshot", Source: "mysql", Status: ItemOK, Body: "data"}}}
}

func TestPipelineRunSucceedsWithSteps(t *testing.T) {
	db := newFakeRunStore()
	reasoner := &fakeReasoner{result: &llm.DiagnoseResult{
		RCA: "容器退出", Confidence: "high", TokensIn: 30, TokensOut: 13,
		Plan: llm.Plan{Action: "restart_container", Target: llm.PlanTarget{Kind: "container", Name: "sub2api"}},
	}}
	reporter := &fakeReporter{}
	pipeline := NewPipeline(db, fakeEvidenceBuilder{evidence: testEvidence()}, reasoner, allowPolicy(), &fakeApprovals{}, reporter, nil, 0)
	run := store.AgentRun{ID: 11, IncidentID: 7, Mode: "full", Status: "running"}

	if err := pipeline.Run(context.Background(), run); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if db.completed[11] != "succeeded" {
		t.Fatalf("final status = %q", db.completed[11])
	}
	if db.tokensIn[11] != 30 {
		t.Fatalf("tokens_in = %d", db.tokensIn[11])
	}
	if reasoner.calls != 1 {
		t.Fatalf("reasoner calls = %d", reasoner.calls)
	}
	// 五段 step 链：evidence → llm → guard → approval(policy) → notify。
	if len(db.steps) != 5 {
		t.Fatalf("steps = %d, want 5", len(db.steps))
	}
	kinds := []string{"evidence", "llm", "guard", "approval", "tool"}
	for i, want := range kinds {
		if db.steps[i].Kind != want {
			t.Fatalf("step %d kind = %q, want %q", i, db.steps[i].Kind, want)
		}
	}
	if reporter.calls != 1 {
		t.Fatalf("reporter calls = %d", reporter.calls)
	}
}

// Reasoner 的每次工具调用都要落 agent_run_step：只有一条 RCA 摘要的话，
// 回放看不到模型实际查了什么，"工具调用次数"也没法断言。
func TestPipelineRecordsReasonerToolSteps(t *testing.T) {
	db := newFakeRunStore()
	reasoner := &fakeReasoner{result: &llm.DiagnoseResult{
		RCA: "容器退出", Confidence: "high",
		Plan: llm.Plan{Action: "restart_container", Target: llm.PlanTarget{Kind: "container", Name: "sub2api"}},
		Steps: []llm.StepLog{
			{Name: "prom_instant_query", Input: `{"query":"up"}`, Output: `{"result":[]}`,
				StartedAt:  time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC),
				FinishedAt: time.Date(2026, 8, 20, 9, 0, 1, 0, time.UTC)},
			{Name: "docker_logs", Input: `{"name":"sub2api"}`, Err: "container not found"},
		},
	}}
	pipeline := NewPipeline(db, fakeEvidenceBuilder{evidence: testEvidence()}, reasoner, allowPolicy(), &fakeApprovals{}, &fakeReporter{}, nil, 0)
	if err := pipeline.Run(context.Background(), store.AgentRun{ID: 21, IncidentID: 7, Mode: "full", Status: "running"}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	toolCalls := map[string]store.AgentRunStep{}
	for _, step := range db.steps {
		if step.Kind == "tool" && step.Name != "notify" {
			toolCalls[step.Name] = step
		}
	}
	if len(toolCalls) != 2 {
		t.Fatalf("recorded tool steps = %d, want 2: %+v", len(toolCalls), db.steps)
	}
	prom, ok := toolCalls["prom_instant_query"]
	if !ok || prom.InputJSON == nil || prom.OutputJSON == nil {
		t.Fatalf("prom step = %+v", prom)
	}
	if prom.Seq < 30 || prom.Seq >= 90 {
		t.Fatalf("tool step seq = %d, want the 30..89 band (clear of the main chain and verify)", prom.Seq)
	}
	if !prom.StartedAt.Equal(time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)) {
		t.Fatalf("tool step started_at = %s, want the recorded call time", prom.StartedAt)
	}
	// 失败的工具调用也要留痕，错误进 error 列。
	failed, ok := toolCalls["docker_logs"]
	if !ok || failed.Error == nil || !strings.Contains(*failed.Error, "container not found") {
		t.Fatalf("failed tool step = %+v", failed)
	}
	// llm 摘要里带调用次数，回放时先看摘要就知道该找几条。
	for _, step := range db.steps {
		if step.Kind == "llm" && !strings.Contains(string(*step.OutputJSON), "tool_calls=2") {
			t.Fatalf("llm step output = %s", *step.OutputJSON)
		}
	}
}

// 诊断失败时的工具调用同样要落库：失败的 run 也要能回放。
func TestPipelineRecordsToolStepsOnReasonFailure(t *testing.T) {
	db := newFakeRunStore()
	reasoner := &fakeReasoner{
		result: &llm.DiagnoseResult{Steps: []llm.StepLog{{Name: "prom_instant_query", Input: `{"query":"up"}`}}},
		err:    errors.New("model output is not valid JSON"),
	}
	pipeline := NewPipeline(db, fakeEvidenceBuilder{evidence: testEvidence()}, reasoner, allowPolicy(), &fakeApprovals{}, &fakeReporter{}, nil, 0)
	if err := pipeline.Run(context.Background(), store.AgentRun{ID: 22, IncidentID: 7, Mode: "full", Status: "running"}); err == nil {
		t.Fatal("Run() error = nil, want reason failure")
	}
	if db.completed[22] != "failed" {
		t.Fatalf("final status = %q, want failed", db.completed[22])
	}
	found := false
	for _, step := range db.steps {
		if step.Kind == "tool" && step.Name == "prom_instant_query" {
			found = true
		}
	}
	if !found {
		t.Fatalf("failed run lost its tool steps: %+v", db.steps)
	}
}

// Policy 收到的护栏输入必须来自 incident 上下文：可信 target 来自告警标签，
// 可验证性来自"有成员告警可复查"。
func TestPipelinePassesPolicyGuardrailInput(t *testing.T) {
	db := newFakeRunStore()
	reasoner := &fakeReasoner{result: &llm.DiagnoseResult{
		RCA: "容器退出", Confidence: "high",
		Plan: llm.Plan{Action: "restart_container", Target: llm.PlanTarget{Kind: "container", Name: "sub2api"}},
	}}
	builder := fakeEvidenceBuilder{evidence: testEvidence(), target: Target{
		Incident: store.Incident{ID: 7, GroupKey: "payments"},
		Members:  []store.IncidentMember{{Fingerprint: "fp1", Name: "HighCPU", Status: "firing"}},
		Alerts:   []store.Alert{{Name: "HighCPU", Labels: []byte(`{"alertname":"HighCPU","container":"sub2api"}`)}},
	}}
	policy := allowPolicy()
	pipeline := NewPipeline(db, builder, reasoner, policy, &fakeApprovals{}, &fakeReporter{}, nil, 0)
	if err := pipeline.Run(context.Background(), store.AgentRun{ID: 23, IncidentID: 7, Mode: "full", Status: "running"}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if policy.lastInput == nil {
		t.Fatal("policy did not receive guardrail input")
	}
	if !policy.lastInput.Verifiable {
		t.Fatal("Verifiable = false, want true for an incident with members")
	}
	if len(policy.lastInput.KnownTargets) != 1 || policy.lastInput.KnownTargets[0] != "sub2api" {
		t.Fatalf("KnownTargets = %v, want [sub2api] from the alert labels", policy.lastInput.KnownTargets)
	}
}

// 没有成员的 incident 不可验证：Policy 会据此拒绝自动执行。
func TestPipelineMarksUnverifiableWithoutMembers(t *testing.T) {
	db := newFakeRunStore()
	reasoner := &fakeReasoner{result: &llm.DiagnoseResult{RCA: "x", Confidence: "low"}}
	policy := allowPolicy()
	pipeline := NewPipeline(db, fakeEvidenceBuilder{evidence: testEvidence()}, reasoner, policy, &fakeApprovals{}, &fakeReporter{}, nil, 0)
	if err := pipeline.Run(context.Background(), store.AgentRun{ID: 24, IncidentID: 7, Mode: "full", Status: "running"}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if policy.lastInput == nil || policy.lastInput.Verifiable {
		t.Fatalf("input = %+v, want Verifiable=false", policy.lastInput)
	}
}

func TestPipelineGuardHitRecorded(t *testing.T) {
	db := newFakeRunStore()
	reasoner := &fakeReasoner{result: &llm.DiagnoseResult{
		RCA: "配置错误导致启动失败", Confidence: "medium",
		Plan: llm.Plan{Action: "restart_container", Target: llm.PlanTarget{Kind: "container", Name: "sub2api"}},
	}}
	pipeline := NewPipeline(db, fakeEvidenceBuilder{evidence: testEvidence()}, reasoner, allowPolicy(), &fakeApprovals{}, &fakeReporter{}, nil, 0)
	run := store.AgentRun{ID: 12, IncidentID: 7, Mode: "full", Status: "running"}
	if err := pipeline.Run(context.Background(), run); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	var guardStep *store.AgentRunStep
	for i := range db.steps {
		if db.steps[i].Kind == "guard" {
			guardStep = &db.steps[i]
		}
	}
	if guardStep == nil {
		t.Fatal("no guard step recorded")
	}
	if guardStep.OutputJSON == nil || !strings.Contains(string(*guardStep.OutputJSON), "escalate") {
		t.Fatalf("guard step output = %v", guardStep.OutputJSON)
	}
}

func TestPipelineFailureMarksRunFailed(t *testing.T) {
	db := newFakeRunStore()
	// 证据阶段失败 → run failed，不进入 LLM。
	reasoner := &fakeReasoner{}
	pipeline := NewPipeline(db, fakeEvidenceBuilder{err: errors.New("incident gone")}, reasoner, allowPolicy(), &fakeApprovals{}, &fakeReporter{}, nil, 0)
	run := store.AgentRun{ID: 13, IncidentID: 99, Mode: "full", Status: "running"}
	err := pipeline.Run(context.Background(), run)
	if err == nil {
		t.Fatal("Run() error = nil, want evidence failure")
	}
	if db.completed[13] != "failed" {
		t.Fatalf("final status = %q, want failed", db.completed[13])
	}
	if reasoner.calls != 0 {
		t.Fatalf("reasoner called %d times after evidence failure", reasoner.calls)
	}
}

func TestPipelineNotifyFailureKeepsRunSucceeded(t *testing.T) {
	db := newFakeRunStore()
	reasoner := &fakeReasoner{result: &llm.DiagnoseResult{RCA: "ok", Confidence: "low"}}
	pipeline := NewPipeline(db, fakeEvidenceBuilder{evidence: testEvidence()}, reasoner, allowPolicy(), &fakeApprovals{}, &fakeReporter{err: errors.New("webhook down")}, nil, 0)
	run := store.AgentRun{ID: 14, IncidentID: 7, Mode: "light", Status: "running"}
	if err := pipeline.Run(context.Background(), run); err != nil {
		t.Fatalf("Run() error = %v, want nil (notify failure is independent)", err)
	}
	if db.completed[14] != "succeeded" {
		t.Fatalf("final status = %q, want succeeded", db.completed[14])
	}
	// notify step 带错误记录。
	notifyStep := db.steps[len(db.steps)-1]
	if notifyStep.Error == nil {
		t.Fatal("notify step has no error recorded")
	}
}

func TestStepPayloadTruncation(t *testing.T) {
	long := strings.Repeat("字", 2000)
	if got := TruncateForStep(long); len([]rune(got)) > stepPayloadMaxRunes+16 {
		t.Fatalf("truncated len = %d runes", len([]rune(got)))
	}
	// step input/output 包成合法 JSON。
	if j := toJSON("hello"); j == nil || !strings.HasPrefix(string(*j), `"`) {
		t.Fatalf("toJSON = %v", j)
	}
	var _ datatypes.JSON // 类型锚定
}

func TestPipelineL3CreatesApproval(t *testing.T) {
	db := newFakeRunStore()
	reasoner := &fakeReasoner{result: &llm.DiagnoseResult{
		RCA: "连接池耗尽", Confidence: "high",
		Plan: llm.Plan{Action: "pool_resize", Target: llm.PlanTarget{Kind: "service", Name: "sub2api"}},
	}}
	approvals := &fakeApprovals{}
	policy := &fakePolicy{decision: approval.Decision{Kind: approval.DecisionApproval, ToolName: "pool_resize", PlanHash: "abc"}}
	pipeline := NewPipeline(db, fakeEvidenceBuilder{evidence: testEvidence()}, reasoner, policy, approvals, &fakeReporter{}, nil, 0)
	run := store.AgentRun{ID: 15, IncidentID: 7, Mode: "full", Status: "running"}
	if err := pipeline.Run(context.Background(), run); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	// L3 决策落 pending 审批单并记 approval step。
	if len(approvals.created) != 1 || approvals.created[0].Status != "pending" || approvals.created[0].RunID != 15 {
		t.Fatalf("approvals = %+v", approvals.created)
	}
	var policyStep *store.AgentRunStep
	for i := range db.steps {
		if db.steps[i].Kind == "approval" {
			policyStep = &db.steps[i]
		}
	}
	if policyStep == nil || !strings.Contains(string(*policyStep.OutputJSON), "decision=approval") {
		t.Fatalf("policy step = %+v", policyStep)
	}
	if db.completed[15] != "succeeded" {
		t.Fatalf("final status = %q", db.completed[15])
	}
}

func TestPipelineApprovalCreateFailureFailsRun(t *testing.T) {
	db := newFakeRunStore()
	reasoner := &fakeReasoner{result: &llm.DiagnoseResult{
		RCA: "x", Confidence: "high",
		Plan: llm.Plan{Action: "pool_resize", Target: llm.PlanTarget{Kind: "service", Name: "sub2api"}},
	}}
	policy := &fakePolicy{decision: approval.Decision{Kind: approval.DecisionApproval, ToolName: "pool_resize", PlanHash: "abc"}}
	pipeline := NewPipeline(db, fakeEvidenceBuilder{evidence: testEvidence()}, reasoner, policy, &fakeApprovals{err: errors.New("db down")}, &fakeReporter{}, nil, 0)
	run := store.AgentRun{ID: 16, IncidentID: 7, Mode: "full", Status: "running"}
	// 审批单落不了 = 执行许可拿不到，run 必须 failed（不能假装已审批）。
	if err := pipeline.Run(context.Background(), run); err == nil {
		t.Fatal("Run() error = nil, want approval create failure")
	}
	if db.completed[16] != "failed" {
		t.Fatalf("final status = %q, want failed", db.completed[16])
	}
}

// fakeMemory 实现 memoryLookup。
type fakeMemory struct {
	entry   store.FaultMemory
	hit     bool
	history []store.FaultCmdHistory
	lookups int
}

func (f *fakeMemory) Lookup(context.Context, string) (store.FaultMemory, bool, error) {
	f.lookups++
	return f.entry, f.hit, nil
}

func (f *fakeMemory) RecentCmds(context.Context, string, int) ([]store.FaultCmdHistory, error) {
	return f.history, nil
}

func TestPipelineMemoryHitSkipsLLM(t *testing.T) {
	db := newFakeRunStore()
	db.runs[20] = store.AgentRun{ID: 20, IncidentID: 7, Mode: "full", Status: "running"}
	planJSON := []byte(`{"action":"docker_restart","target":{"kind":"container","name":"sub2api"},"reason":"上次有效","confidence":"high","risk":"low","expected":"恢复"}`)
	mem := &fakeMemory{
		hit: true,
		entry: store.FaultMemory{
			Fingerprint: "fp", RCAText: "记忆中的根因", Confidence: "high",
			PlanJSON: planJSON, Hits: 2,
		},
	}
	reasoner := &fakeReasoner{}
	builder := fakeEvidenceBuilder{target: Target{
		Incident: store.Incident{ID: 7, GroupKey: "payments"},
		Alerts:   []store.Alert{{Name: "HighCPU"}},
	}}
	pipeline := NewPipeline(db, builder, reasoner, allowPolicy(), &fakeApprovals{}, &fakeReporter{}, mem, 5)
	run := store.AgentRun{ID: 20, IncidentID: 7, Mode: "full", Status: "running"}
	if err := pipeline.Run(context.Background(), run); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	// 0 次 LLM；RCA/Plan 来自记忆；token 为 0。
	if reasoner.calls != 0 {
		t.Fatalf("reasoner called %d times on memory hit", reasoner.calls)
	}
	if db.tokensIn[20] != 0 {
		t.Fatalf("tokens_in = %d, want 0", db.tokensIn[20])
	}
	if db.completeRCA[20] != "记忆中的根因" {
		t.Fatalf("rca = %q", db.completeRCA[20])
	}
	if db.runs[20].Mode != "memory_hit" {
		t.Fatalf("mode = %q, want memory_hit", db.runs[20].Mode)
	}
	// step 链：memory_lookup + guard + policy + notify，无 evidence/llm。
	var kinds []string
	for _, step := range db.steps {
		kinds = append(kinds, step.Kind)
	}
	want := []string{"tool", "guard", "approval", "tool"}
	if len(kinds) != 4 {
		t.Fatalf("steps = %v", kinds)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("step %d kind = %q, want %q", i, kinds[i], want[i])
		}
	}
}

func TestPipelineRetrySkipsMemoryLookup(t *testing.T) {
	db := newFakeRunStore()
	reasoner := &fakeReasoner{result: &llm.DiagnoseResult{RCA: "新诊断", Confidence: "low"}}
	mem := &fakeMemory{hit: true, entry: store.FaultMemory{RCAText: "旧记忆", Confidence: "high", PlanJSON: []byte(`{"action":"none"}`)}}
	builder := fakeEvidenceBuilder{evidence: testEvidence(), target: Target{
		Incident: store.Incident{ID: 7, GroupKey: "payments"},
		Alerts:   []store.Alert{{Name: "HighCPU"}},
	}}
	pipeline := NewPipeline(db, builder, reasoner, allowPolicy(), &fakeApprovals{}, &fakeReporter{}, mem, 5)
	retryOf := uint64(10)
	db.runs[10] = store.AgentRun{ID: 10, Status: "failed"}
	run := store.AgentRun{ID: 21, IncidentID: 7, Mode: "full", Status: "running", RetryOf: &retryOf}
	if err := pipeline.Run(context.Background(), run); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	// 重诊 run 不查记忆（防坏记忆循环命中）。
	if mem.lookups != 0 {
		t.Fatalf("memory looked up %d times on retry run", mem.lookups)
	}
	if reasoner.calls != 1 {
		t.Fatalf("reasoner calls = %d", reasoner.calls)
	}
}

func TestPipelineMissInjectsCmdHistory(t *testing.T) {
	db := newFakeRunStore()
	var gotEvidence string
	reasoner := &fakeReasoner{result: &llm.DiagnoseResult{RCA: "ok", Confidence: "low"}}
	mem := &fakeMemory{history: []store.FaultCmdHistory{{ToolName: "docker_restart", ArgsJSON: []byte(`{"target_name":"sub2api"}`), ResultBrief: "ok", CreatedAt: time.Now()}}}
	builder := fakeEvidenceBuilder{evidence: testEvidence(), target: Target{
		Incident: store.Incident{ID: 7, GroupKey: "payments"},
		Alerts:   []store.Alert{{Name: "HighCPU"}},
	}}
	// 包一层 reasoner 捕获输入。
	capturing := &capturingReasoner{inner: reasoner, out: &gotEvidence}
	pipeline := NewPipeline(db, builder, capturing, allowPolicy(), &fakeApprovals{}, &fakeReporter{}, mem, 5)
	run := store.AgentRun{ID: 22, IncidentID: 7, Mode: "full", Status: "running"}
	if err := pipeline.Run(context.Background(), run); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !strings.Contains(gotEvidence, "历史命令") || !strings.Contains(gotEvidence, "docker_restart") {
		t.Fatalf("evidence missing cmd history:\n%s", gotEvidence[:200])
	}
}

type capturingReasoner struct {
	inner *fakeReasoner
	out   *string
}

func (c *capturingReasoner) Diagnose(ctx context.Context, evidence string, mode string) (*llm.DiagnoseResult, error) {
	*c.out = evidence
	return c.inner.Diagnose(ctx, evidence, mode)
}
