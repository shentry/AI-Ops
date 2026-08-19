package approval

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"oncall-agent/internal/memory"
	"oncall-agent/internal/metrics"
	"oncall-agent/internal/store"
	"oncall-agent/internal/tools"
	"strings"
	"time"
)

// execStore 是执行器对存储层的收窄接口。
type execStore interface {
	NextApprovedApproval(ctx context.Context, now time.Time) (store.Approval, bool, error)
	ClaimApprovalExecution(ctx context.Context, id uint64, now time.Time) (bool, error)
	FinishApprovalExecution(ctx context.Context, id uint64, status string, resultJSON []byte) error
	InsertFaultCmdHistory(ctx context.Context, row store.FaultCmdHistory) error
	GetIncident(ctx context.Context, id uint64) (store.Incident, error)
	ListIncidentMembers(ctx context.Context, incidentID uint64) ([]store.IncidentMember, error)
	GetAgentRun(ctx context.Context, id uint64) (store.AgentRun, error)
	ListRunSteps(ctx context.Context, runID uint64) ([]store.AgentRunStep, error)
}

// verifier 是执行器对 Verify 的收窄接口。
type verifier interface {
	VerifyAfterExecution(ctx context.Context, runID, incidentID uint64, delay time.Duration) VerifyOutcome
}

// retryScheduler 是执行器对 D12 重诊调度的收窄接口。
type retryScheduler interface {
	ScheduleRetry(ctx context.Context, incidentID, failedRunID uint64, reason string) (bool, error)
}

// memoryWriter 是执行器对故障记忆的收窄接口（D13）。
type memoryWriter interface {
	Commit(ctx context.Context, entry store.FaultMemory) error
	Demote(ctx context.Context, fingerprint string) error
}

// VerifyOutcome 与 diagnose.VerifyResult 同构，避免 approval → diagnose 依赖。
type VerifyOutcome struct {
	Passed bool
	Detail string
}

// Executor 消费 approved 审批单：领取 → 校验 → 执行 → 回写 → 记忆 → 验证。
type Executor struct {
	db          execStore
	registry    *tools.Registry
	dryRun      bool
	verifyDelay time.Duration
	verify      verifier
	retry       retryScheduler
	memory      memoryWriter
	logger      *log.Logger
	done        chan struct{}
}

func NewExecutor(db execStore, registry *tools.Registry, dryRun bool, verifyDelay time.Duration, v verifier, retry retryScheduler, memory memoryWriter, logger *log.Logger) *Executor {
	if logger == nil {
		logger = log.Default()
	}
	return &Executor{db: db, registry: registry, dryRun: dryRun, verifyDelay: verifyDelay, verify: v, retry: retry, memory: memory, logger: logger, done: make(chan struct{})}
}

func (e *Executor) Start(ctx context.Context) {
	go e.loop(ctx)
}

func (e *Executor) Wait() { <-e.done }

func (e *Executor) loop(ctx context.Context) {
	defer close(e.done)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		e.drain(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (e *Executor) drain(ctx context.Context) {
	for {
		approval, found, err := e.db.NextApprovedApproval(ctx, time.Now().UTC())
		if err != nil {
			e.logger.Printf("executor: poll approved: %v", err)
			return
		}
		if !found {
			return
		}
		claimed, err := e.db.ClaimApprovalExecution(ctx, approval.ID, time.Now().UTC())
		if err != nil {
			e.logger.Printf("executor: claim %d: %v", approval.ID, err)
			return
		}
		if !claimed {
			continue
		}
		e.executeOne(ctx, approval)
	}
}

// executeOne 执行一张审批单的全部后处理。任何失败都写回 failed —— 不静默。
func (e *Executor) executeOne(ctx context.Context, approval store.Approval) {
	// 执行前最终校验：plan_hash 必须等于 tool+args 的重算值，
	// 审批单内容在批准后被篡改在这里失配（GC-13）。
	if PlanHash(approval.ToolName, json.RawMessage(approval.ArgsJSON)) != approval.PlanHash {
		e.finishWithError(ctx, approval, nil, "plan hash mismatch, refusing to execute")
		return
	}
	if _, ok := e.registry.Get(approval.ToolName); !ok {
		// 审批单上的工具必须在册（GC-12），不论等级。
		e.finishWithError(ctx, approval, nil, fmt.Sprintf("tool %q is not registered", approval.ToolName))
		return
	}

	var output string
	var execErr error
	if e.dryRun {
		output = fmt.Sprintf(`{"dry_run":true,"tool":%q}`, approval.ToolName)
		e.logger.Printf("executor: dry-run approval %d tool %s", approval.ID, approval.ToolName)
	} else {
		output, execErr = e.registry.Execute(ctx, approval.ToolName, json.RawMessage(approval.ArgsJSON))
	}

	resultJSON, _ := json.Marshal(map[string]any{
		"output":   output,
		"dry_run":  e.dryRun,
		"tool":     approval.ToolName,
		"executed": execErr == nil,
	})
	if execErr != nil {
		e.finishWithError(ctx, approval, resultJSON, execErr.Error())
		metrics.Inc(metrics.ApprovalFailedExec)
		return
	}
	if err := e.db.FinishApprovalExecution(ctx, approval.ID, "executed", resultJSON); err != nil {
		e.logger.Printf("executor: finish %d: %v", approval.ID, err)
	}
	metrics.Inc(metrics.ApprovalExecuted)

	// 命令历史：已审批动作的结果写入故障记忆候选（D13 的注入源）。
	e.recordCmdHistory(ctx, approval, execErr == nil, output)

	// 独立验证（GC-15）。dry_run 没有真实变更，跳过验证。
	if !e.dryRun && e.verify != nil {
		result := e.verify.VerifyAfterExecution(ctx, approval.RunID, approval.IncidentID, e.verifyDelay)
		e.logger.Printf("executor: verify approval %d: passed=%v %s", approval.ID, result.Passed, result.Detail)
		metrics.Inc(map[bool]string{true: metrics.VerifyPassed, false: metrics.VerifyFailed}[result.Passed])
		if result.Passed {
			// D13：验证成功且够格的案例写入故障记忆。
			e.maybeCommitMemory(ctx, approval)
		} else {
			// D13：memory_hit 验证失败 → 记忆降级拉黑（防循环命中）。
			e.demoteMemoryIfHit(ctx, approval)
			// D12：Verify 失败 → 有限重诊或升级人工。
			if e.retry != nil {
				created, err := e.retry.ScheduleRetry(ctx, approval.IncidentID, approval.RunID, "verify failed: "+result.Detail)
				if err != nil {
					e.logger.Printf("executor: schedule retry for incident %d: %v", approval.IncidentID, err)
				} else if created {
					e.logger.Printf("executor: incident %d retry run enqueued (after run %d)", approval.IncidentID, approval.RunID)
				}
			}
		}
	}
}

// maybeCommitMemory 只收"干净"的成功案例（GC-16）：
// 非重诊、非记忆命中、confidence=high、Guard 未改写。
func (e *Executor) maybeCommitMemory(ctx context.Context, approval store.Approval) {
	if e.memory == nil {
		return
	}
	run, err := e.db.GetAgentRun(ctx, approval.RunID)
	if err != nil || run.PlanJSON == nil {
		return
	}
	if run.RetryOf != nil || run.Mode == "memory_hit" {
		return
	}
	var plan struct {
		Confidence string `json:"confidence"`
	}
	if err := json.Unmarshal([]byte(*run.PlanJSON), &plan); err != nil || plan.Confidence != "high" {
		return
	}
	// Guard 改写过的案例不降格入库：被规则改过的计划不是"被验证的原计划"。
	if steps, err := e.db.ListRunSteps(ctx, approval.RunID); err == nil {
		for _, step := range steps {
			if step.Kind == "guard" && step.OutputJSON != nil && strings.Contains(string(*step.OutputJSON), "overridden=true") {
				return
			}
		}
	}
	incident, err := e.db.GetIncident(ctx, approval.IncidentID)
	if err != nil {
		return
	}
	alertName := ""
	if members, err := e.db.ListIncidentMembers(ctx, approval.IncidentID); err == nil && len(members) > 0 {
		alertName = members[0].Name
	}
	rca := ""
	if run.RCAText != nil {
		rca = *run.RCAText
	}
	entry := store.FaultMemory{
		Fingerprint: memory.FaultFingerprint(incident.GroupKey, alertName),
		GroupKey:    incident.GroupKey,
		AlertName:   alertName,
		RCAText:     rca,
		PlanJSON:    *run.PlanJSON,
		Confidence:  "high",
	}
	if err := e.memory.Commit(ctx, entry); err != nil {
		e.logger.Printf("executor: memory commit for run %d: %v", run.ID, err)
	}
}

// demoteMemoryIfHit 在 memory_hit 验证失败时降级该记忆。
func (e *Executor) demoteMemoryIfHit(ctx context.Context, approval store.Approval) {
	if e.memory == nil {
		return
	}
	run, err := e.db.GetAgentRun(ctx, approval.RunID)
	if err != nil || run.Mode != "memory_hit" {
		return
	}
	incident, err := e.db.GetIncident(ctx, approval.IncidentID)
	if err != nil {
		return
	}
	alertName := ""
	if members, err := e.db.ListIncidentMembers(ctx, approval.IncidentID); err == nil && len(members) > 0 {
		alertName = members[0].Name
	}
	if err := e.memory.Demote(ctx, memory.FaultFingerprint(incident.GroupKey, alertName)); err != nil {
		e.logger.Printf("executor: memory demote for run %d: %v", run.ID, err)
	}
}

func (e *Executor) finishWithError(ctx context.Context, approval store.Approval, resultJSON []byte, message string) {
	if len(resultJSON) == 0 {
		resultJSON, _ = json.Marshal(map[string]any{"error": message})
	}
	if err := e.db.FinishApprovalExecution(ctx, approval.ID, "failed", resultJSON); err != nil {
		e.logger.Printf("executor: mark %d failed: %v (cause: %s)", approval.ID, err, message)
		return
	}
	e.logger.Printf("executor: approval %d failed: %s", approval.ID, message)
}

// recordCmdHistory 把执行结果写进 fault_cmd_history。
// 键与故障记忆同源（md5(group_key + 首条告警名)[:12]）。
func (e *Executor) recordCmdHistory(ctx context.Context, approval store.Approval, success bool, output string) {
	incident, err := e.db.GetIncident(ctx, approval.IncidentID)
	if err != nil {
		e.logger.Printf("executor: load incident %d for cmd history: %v", approval.IncidentID, err)
		return
	}
	alertName := ""
	if members, err := e.db.ListIncidentMembers(ctx, approval.IncidentID); err == nil && len(members) > 0 {
		alertName = members[0].Name
	}
	brief := output
	if !success {
		brief = "failed"
	}
	if runes := []rune(brief); len(runes) > 1024 {
		brief = string(runes[:1024])
	}
	row := store.FaultCmdHistory{
		Fingerprint: memory.FaultFingerprint(incident.GroupKey, alertName),
		ToolName:    approval.ToolName,
		ArgsJSON:    approval.ArgsJSON,
		ResultBrief: brief,
		ApprovalID:  &approval.ID,
		CreatedAt:   time.Now().UTC(),
	}
	if err := e.db.InsertFaultCmdHistory(ctx, row); err != nil {
		e.logger.Printf("executor: record cmd history: %v", err)
	}
}
