package diagnose

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/datatypes"

	"oncall-agent/internal/approval"
	"oncall-agent/internal/eventlog"
	"oncall-agent/internal/llm"
	faultmemory "oncall-agent/internal/memory"
	"oncall-agent/internal/metrics"
	"oncall-agent/internal/store"
)

// stepPayloadMaxRunes 是 step input/output 摘要的截断预算：
// 审计要可回放，但不把整份 Evidence 大文本塞进 step 表（A13）。
const stepPayloadMaxRunes = 1024

// reasoner 是 Pipeline 对 LLM 层的收窄接口，单测换假实现。
type reasoner interface {
	Diagnose(ctx context.Context, evidence string, mode string) (*llm.DiagnoseResult, error)
}

// reporter 是 Pipeline 对通知层的收窄接口。
type reporter interface {
	NotifyDiagnosis(ctx context.Context, report DiagnosisReport) error
}

// runStore 是 Pipeline 对存储层的收窄接口。
type runStore interface {
	AppendRunStepRecord(ctx context.Context, record store.RunStepRecord) error
	CompleteRun(ctx context.Context, completion store.RunCompletion) error
	AppendIncidentEvent(ctx context.Context, event store.IncidentEvent) (store.IncidentEvent, error)
	GetAgentRun(ctx context.Context, id uint64) (store.AgentRun, error)
	ListRunSteps(ctx context.Context, runID uint64) ([]store.AgentRunStep, error)
	UpdateAgentRunMode(ctx context.Context, id uint64, mode string) error
}

// DiagnosisReport 是发给 IM 的诊断报告内容。
type DiagnosisReport struct {
	IncidentID uint64
	RunID      uint64
	Mode       string
	RCA        string
	Confidence string
	Plan       llm.Plan
	Decision   string // guard 结论：allow / deny / escalate
	Overridden bool
	GuardNote  string
	// D10：policy 结论（none/auto_l1/auto_l2/approval/denied）与审批单号。
	PolicyDecision string
	ApprovalID     *uint64
	// 审批卡片回调只携带不可变引用，plan hash 是其中一项：
	// 缺失会让飞书按钮在字段校验阶段被拒。
	PlanHash string
	// D13：记忆命中时的命中次数（0 = 未命中）。
	MemoryHits int
}

// Pipeline 串联诊断各阶段：memory → evidence → reason → guard → policy → report。
// 每个阶段落一条 agent_run_step；任何阶段失败 run 标 failed，
// 错误返回给 worker 记日志，进程不死。
type Pipeline struct {
	db               runStore
	builder          evidenceBuilder
	reasoner         reasoner
	policy           policyEngine
	approvals        approvalCreator
	reporter         reporter
	memories         memoryLookup
	cmdHistoryInject int
}

// evidenceBuilder 是 Pipeline 对证据装配的收窄接口（单测换假实现）。
type evidenceBuilder interface {
	BuildForIncident(ctx context.Context, incidentID uint64) (Evidence, error)
	LoadTarget(ctx context.Context, incidentID uint64) (Target, error)
}

// memoryLookup 是 Pipeline 对 D13 故障记忆的收窄接口。
type memoryLookup interface {
	Lookup(ctx context.Context, fingerprint string) (store.FaultMemory, bool, error)
	RecentCmds(ctx context.Context, fingerprint string, limit int) ([]store.FaultCmdHistory, error)
}

// policyEngine 是 Pipeline 对 D10 Policy 的收窄接口。
type policyEngine interface {
	Decide(ctx context.Context, plan llm.Plan, input approval.PolicyInput) approval.Decision
}

// approvalCreator 是 Pipeline 对审批服务的收窄接口。
type approvalCreator interface {
	Create(ctx context.Context, incidentID, runID uint64, decision approval.Decision, reason string) (store.Approval, error)
	CreateSystemApproved(ctx context.Context, incidentID, runID uint64, decision approval.Decision, reason string) (store.Approval, error)
}

func NewPipeline(db runStore, builder evidenceBuilder, r reasoner, policy policyEngine, approvals approvalCreator, rep reporter, memories memoryLookup, cmdHistoryInject int) *Pipeline {
	return &Pipeline{
		db: db, builder: builder, reasoner: r, policy: policy, approvals: approvals,
		reporter: rep, memories: memories, cmdHistoryInject: cmdHistoryInject,
	}
}

// Run 执行一条 agent_run 的诊断。调用方（worker）已把 run 置为 running。
func (p *Pipeline) Run(ctx context.Context, run store.AgentRun) error {
	var rca, confidence string
	var tokensIn, tokensOut int
	var plan llm.Plan
	var memoryHits int

	// 装配 incident 上下文（只读库，不跑 collector）。
	target, err := p.builder.LoadTarget(ctx, run.IncidentID)
	if err != nil {
		return p.fail(ctx, run, err)
	}

	// 阶段 1：记忆查找。重诊 run 不查记忆（防坏记忆循环命中，A11）。
	// 命中则跳过证据采集与 LLM：0 次 LLM 调用（D13 验收），
	// 但 Guard、Policy、审批和 Verify 一步不少（GC-17）。
	fingerprint := ""
	if len(target.Alerts) > 0 {
		fingerprint = faultmemory.FaultFingerprint(target.Incident.GroupKey, target.Alerts[0].Name)
	}
	if run.RetryOf == nil && fingerprint != "" && p.memories != nil {
		entry, hit, lookupErr := p.memories.Lookup(ctx, fingerprint)
		if hit {
			memoryHits = entry.Hits + 1 // Lookup 已 touch，报告用命中后的次数
			rca = entry.RCAText
			confidence = entry.Confidence
			if err := json.Unmarshal(entry.PlanJSON, &plan); err != nil {
				return p.fail(ctx, run, fmt.Errorf("memory plan undecodable: %w", err))
			}
			// run mode 改写为 memory_hit：审计能区分 LLM 诊断和记忆命中。
			if err := p.db.UpdateAgentRunMode(ctx, run.ID, "memory_hit"); err != nil {
				return p.fail(ctx, run, fmt.Errorf("mark memory_hit: %w", err))
			}
			run.Mode = "memory_hit"
		}
		p.appendStep(ctx, run.ID, run.IncidentID, 1, "tool", "memory_lookup",
			fmt.Sprintf("fingerprint=%s retry_of=%v", fingerprint, run.RetryOf),
			fmt.Sprintf("hit=%v hits=%d err=%v", hit, memoryHits, lookupErr != nil), lookupErr)
		metrics.Inc(map[bool]string{true: metrics.MemoryHit, false: metrics.MemoryMiss}[hit])
	}

	if run.Mode != "memory_hit" {
		// 阶段 2：证据采集。重诊 run 额外注入上一轮的失败结论；
		// 记忆 miss 但有历史命令时注入作为参考证据（不构成权限依据）。
		var collected Evidence
		evidence, err := p.withStep(ctx, run.ID, run.IncidentID, 2, "evidence", "collect", func() (string, error) {
			ev, err := p.builder.BuildForIncident(ctx, run.IncidentID)
			collected = ev
			if err != nil {
				return "", err
			}
			rendered := ev.Render()
			if run.RetryOf != nil {
				rendered = p.retryContext(ctx, *run.RetryOf) + rendered
			}
			if run.RetryOf == nil && fingerprint != "" && p.memories != nil && p.cmdHistoryInject > 0 {
				if cmds, err := p.memories.RecentCmds(ctx, fingerprint, p.cmdHistoryInject); err == nil && len(cmds) > 0 {
					rendered = renderCmdHistory(cmds) + rendered
				}
			}
			return rendered, nil
		}, func() ([]store.IncidentEvent, []store.ProblemMutation) {
			events, problems := collectorAudit(run, collected)
			return events, problems
		})
		if err != nil {
			return p.fail(ctx, run, err)
		}

		// 阶段 3：LLM 推理。mode=skip 的行不会进队列（D05 已直接落 succeeded）。
		result, err := p.runReasonStep(ctx, run, evidence)
		if err != nil {
			return p.fail(ctx, run, err)
		}
		rca = result.RCA
		confidence = result.Confidence
		plan = result.Plan
		tokensIn, tokensOut = result.TokensIn, result.TokensOut
	}

	// 阶段 4：Guard。确定性规则，LLM 之后没有任何环节能改回它的结论。
	// 记忆命中的 RCA/Plan 同样过 Guard（GC-17）。
	guardResult := Guard(rca, plan)
	if guardResult.Overridden {
		plan = guardResult.Plan
	}
	guardProblems := make([]store.ProblemMutation, 0, 1)
	if guardResult.Overridden {
		guardProblems = append(guardProblems, openProblem(run, "guard_overridden", "warning", guardResult.Reason))
	}
	guardEvents := []store.IncidentEvent{{IncidentID: run.IncidentID, RunID: uint64Ptr(run.ID), EventType: string(eventlog.EventGuardEvaluated), Phase: "guard", Status: guardResult.Decision, Summary: safeEventSummary("guard evaluated"), CreatedAt: time.Now().UTC()}}
	if guardResult.Overridden {
		guardEvents = append(guardEvents, store.IncidentEvent{IncidentID: run.IncidentID, RunID: uint64Ptr(run.ID), EventType: string(eventlog.EventGuardOverridden), Phase: "guard", Status: "overridden", Summary: safeEventSummary(guardResult.Reason), CreatedAt: time.Now().UTC()})
	}
	p.appendStepWithTimeEventsProblems(ctx, run.ID, 4, "guard", "rules",
		fmt.Sprintf("action=%s target=%s/%s", plan.Action, plan.Target.Kind, plan.Target.Name),
		fmt.Sprintf("decision=%s overridden=%v reason=%s", guardResult.Decision, guardResult.Overridden, guardResult.Reason), nil, time.Now().UTC(), time.Now().UTC(), guardEvents, guardProblems)

	planBytes, err := json.Marshal(plan)
	if err != nil {
		return p.fail(ctx, run, fmt.Errorf("marshal plan: %w", err))
	}

	// 阶段 5：Policy。Guard 之后的 plan 翻译成确定性执行决策；
	// L3（或条件不满足的 L2）在这里落 pending 审批单（GC-13）。
	// PolicyInput 是 L2 护栏的事实输入：可信 target 来源 + 可验证性，
	// 由 incident 上下文装配 —— Policy 不自己找证据。
	policyInput := approval.PolicyInput{
		KnownTargets: KnownTargets(target),
		Verifiable:   len(target.Members) > 0,
	}
	decision := p.policy.Decide(ctx, plan, policyInput)
	var approvalID *uint64
	switch decision.Kind {
	case approval.DecisionApproval:
		// reason 取 policy 的决策理由（如 "L3 requires approval"）；
		// guard 未命中时 guardResult.Reason 是空串，审批单不能没理由。
		reason := decision.Reason
		if guardResult.Overridden {
			reason = decision.Reason + "; guard: " + guardResult.Reason
		}
		created, createErr := p.approvals.Create(ctx, run.IncidentID, run.ID, decision, Sanitize(reason))
		if createErr != nil {
			return p.fail(ctx, run, fmt.Errorf("create approval: %w", createErr))
		}
		approvalID = &created.ID
		metrics.Inc(metrics.ApprovalCreated)
	case approval.DecisionAutoL2:
		// 自动 L2 也走审批单通道（系统批准），执行面只有一个入口。
		created, createErr := p.approvals.CreateSystemApproved(ctx, run.IncidentID, run.ID, decision, Sanitize(decision.Reason))
		if createErr != nil {
			return p.fail(ctx, run, fmt.Errorf("create system approval: %w", createErr))
		}
		approvalID = &created.ID
		metrics.Inc(metrics.ApprovalCreated)
	}
	policyProblems := make([]store.ProblemMutation, 0, 1)
	if decision.Kind == approval.DecisionDenied {
		policyProblems = append(policyProblems, openProblem(run, "policy_blocked", "warning", decision.Reason))
	} else if decision.Kind == approval.DecisionApproval {
		policyProblems = append(policyProblems, openProblem(run, "policy_degraded", "warning", decision.Reason))
	}
	policyEvents := []store.IncidentEvent{{IncidentID: run.IncidentID, RunID: uint64Ptr(run.ID), EventType: string(eventlog.EventPolicyEvaluated), Phase: "policy", Status: string(decision.Kind), Summary: safeEventSummary("policy evaluated"), CreatedAt: time.Now().UTC()}}
	if decision.Kind == approval.DecisionApproval || decision.Kind == approval.DecisionDenied {
		policyEvents = append(policyEvents, store.IncidentEvent{IncidentID: run.IncidentID, RunID: uint64Ptr(run.ID), EventType: string(eventlog.EventPolicyDegraded), Phase: "policy", Status: string(decision.Kind), Summary: safeEventSummary(decision.Reason), CreatedAt: time.Now().UTC()})
	}
	p.appendStepWithTimeEventsProblems(ctx, run.ID, 5, "approval", "policy",
		fmt.Sprintf("action=%s tool=%s", plan.Action, decision.ToolName),
		fmt.Sprintf("decision=%s reason=%s approval_id=%v", decision.Kind, decision.Reason, approvalID), nil, time.Now().UTC(), time.Now().UTC(), policyEvents, policyProblems)

	// 阶段 6：报告。通知失败独立记录，不改变 run 终态（验收清单）。
	report := DiagnosisReport{
		IncidentID: run.IncidentID, RunID: run.ID, Mode: run.Mode,
		RCA: rca, Confidence: confidence, Plan: plan,
		Decision: guardResult.Decision, Overridden: guardResult.Overridden, GuardNote: guardResult.Reason,
		PolicyDecision: decision.Kind, ApprovalID: approvalID, PlanHash: decision.PlanHash, MemoryHits: memoryHits,
	}
	notifyErr := p.reporter.NotifyDiagnosis(ctx, report)
	notifyType, notifyStatus := eventlog.EventNotificationSent, "succeeded"
	if notifyErr != nil {
		notifyType, notifyStatus = eventlog.EventNotificationFailed, "failed"
	}
	p.appendStepWithTimeEventsProblems(ctx, run.ID, 6, "tool", "notify", "diagnosis report", fmt.Sprintf("decision=%s", guardResult.Decision), notifyErr, time.Now().UTC(), time.Now().UTC(), []store.IncidentEvent{{IncidentID: run.IncidentID, RunID: uint64Ptr(run.ID), EventType: string(notifyType), Phase: "notification", Status: notifyStatus, Summary: safeEventSummary("diagnosis notification"), CreatedAt: time.Now().UTC()}}, notificationProblems(run, notifyErr))

	// 终态必须与 run.succeeded 同一短事务提交。通知 step 已先落库，
	// 因此这里仅提交 run 状态和事实事件，避免重复审计行。
	finishedAt := time.Now().UTC()
	runID := run.ID
	if err := p.db.CompleteRun(ctx, store.RunCompletion{RunID: run.ID, RCA: rca, PlanJSON: planBytes, TokensIn: tokensIn, TokensOut: tokensOut, Status: "succeeded", FinishedAt: finishedAt,
		Events: []store.IncidentEvent{{IncidentID: run.IncidentID, RunID: &runID, EventType: string(eventlog.EventRunSucceeded), Phase: "run", Status: "succeeded", Summary: safeEventSummary("diagnosis completed"), CreatedAt: finishedAt}}}); err != nil {
		return fmt.Errorf("pipeline: complete run %d: %w", run.ID, err)
	}
	return nil
}

// renderCmdHistory 把同指纹的历史命令渲染成证据前缀。
// 明确标注"参考信息，不构成权限依据"——历史命令不提高任何动作的安全等级。
func renderCmdHistory(cmds []store.FaultCmdHistory) string {
	var out strings.Builder
	out.WriteString("# 历史命令（同类故障曾被审批执行过，仅供参考，不构成权限依据）\n")
	for _, cmd := range cmds {
		fmt.Fprintf(&out, "- tool=%s args=%s result=%s at=%s\n",
			cmd.ToolName, Sanitize(string(cmd.ArgsJSON)), Sanitize(cmd.ResultBrief),
			cmd.CreatedAt.UTC().Format(time.RFC3339))
	}
	out.WriteString("\n")
	return out.String()
}

// retryContext 读上一轮 run 的结论，组装"上次失败"上下文。
// 读不到不阻断诊断（记一行说明），重诊注入是增强不是前置条件。
func (p *Pipeline) retryContext(ctx context.Context, previousRunID uint64) string {
	previous, err := p.db.GetAgentRun(ctx, previousRunID)
	if err != nil {
		return fmt.Sprintf("# 重诊上下文\n上一轮诊断 run %d 读取失败：%s\n\n", previousRunID, Sanitize(err.Error()))
	}
	var out strings.Builder
	fmt.Fprintf(&out, "# 重诊上下文（上一轮的诊断与执行结论已失败，不要复用，仅供对照）\n")
	fmt.Fprintf(&out, "previous_run_id: %d\n", previousRunID)
	if previous.RCAText != nil {
		fmt.Fprintf(&out, "previous_rca: %s\n", Sanitize(*previous.RCAText))
	}
	if previous.PlanJSON != nil {
		fmt.Fprintf(&out, "previous_plan: %s\n", Sanitize(string(*previous.PlanJSON)))
	}
	fmt.Fprintf(&out, "previous_status: %s\n", previous.Status)
	// verify 的失败详情在上一轮 run 的 step 里，把它带出来 ——
	// 这是"为什么上次没修好"的直接证据。
	if steps, err := p.db.ListRunSteps(ctx, previousRunID); err == nil {
		for _, step := range steps {
			if step.Kind != "verify" {
				continue
			}
			if step.OutputJSON != nil {
				fmt.Fprintf(&out, "previous_verify: %s\n", Sanitize(string(*step.OutputJSON)))
			}
			if step.Error != nil {
				fmt.Fprintf(&out, "previous_verify_error: %s\n", Sanitize(*step.Error))
			}
		}
	}
	out.WriteString("\n")
	return out.String()
}

// runReasonStep 把 LLM 阶段包成一条 step：输入是证据摘要长度，输出是 RCA 摘要。
// 工具调用另外逐条落库（recordToolSteps）—— 摘要回答"结论是什么"，
// 工具 step 回答"结论是怎么来的、调了几次工具"。
func (p *Pipeline) runReasonStep(ctx context.Context, run store.AgentRun, evidence string) (*llm.DiagnoseResult, error) {
	var result *llm.DiagnoseResult
	var reasonErr error
	_, err := p.withStep(ctx, run.ID, run.IncidentID, 3, "llm", "reason", func() (string, error) {
		out, diagErr := p.reasoner.Diagnose(ctx, evidence, run.Mode)
		// 失败的 Diagnose 也可能带回 Steps（审计残骸），先接住再判错。
		result = out
		reasonErr = diagErr
		if diagErr != nil {
			return "", diagErr
		}
		return fmt.Sprintf("rca=%s confidence=%s tokens=%d/%d tool_calls=%d", out.RCA, out.Confidence, out.TokensIn, out.TokensOut, len(out.Steps)), nil
	}, func() ([]store.IncidentEvent, []store.ProblemMutation) {
		if reasonErr == nil {
			return nil, nil
		}
		if errors.Is(reasonErr, llm.ErrContractParse) {
			return nil, []store.ProblemMutation{openProblem(run, "reasoner_parse_failed", "warning", reasonErr.Error())}
		}
		if errors.Is(reasonErr, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, []store.ProblemMutation{openProblem(run, "reasoner_timeout", "warning", "reasoner deadline exceeded")}
		}
		return nil, nil
	})
	if result != nil {
		p.recordToolSteps(ctx, run, result.Steps)
	}
	if err != nil {
		return nil, err
	}
	return result, nil
}

// toolStepSeqBase 是 Reasoner 工具调用的 seq 段。主链占 1-6，verify 占 90，
// 工具调用用 30 起的独立段：既不撞号，排序后也自然落在 reason(3) 之后。
const toolStepSeqBase = 30

// maxRecordedToolSteps 是单次 run 落库的工具 step 上限，
// 与 seq 段宽度（30..89）对齐，防止越界撞上 verify 的 90。
const maxRecordedToolSteps = 60

// recordToolSteps 把 Reasoner 的工具调用逐条写进审计；每条工具调用
// 同时写 llm.tool_called 事实事件。重复同一 tool+args 第三次打开问题。
func (p *Pipeline) recordToolSteps(ctx context.Context, run store.AgentRun, steps []llm.StepLog) {
	seen := make(map[string]int)
	for i, step := range steps {
		if i >= maxRecordedToolSteps {
			p.appendStep(ctx, run.ID, run.IncidentID, toolStepSeqBase+maxRecordedToolSteps, "tool", "tool_calls_truncated",
				fmt.Sprintf("total=%d", len(steps)), fmt.Sprintf("recorded=%d dropped=%d", maxRecordedToolSteps, len(steps)-maxRecordedToolSteps), nil)
			return
		}
		var stepErr error
		if step.Err != "" {
			stepErr = errors.New(step.Err)
		}
		started, finished := step.StartedAt, step.FinishedAt
		if started.IsZero() {
			started = time.Now().UTC()
		}
		if finished.IsZero() {
			finished = started
		}
		key := step.Name + "\x00" + sanitizeForStep(step.Input)
		seen[key]++
		problems := make([]store.ProblemMutation, 0, 1)
		if seen[key] >= 3 {
			problems = append(problems, openProblem(run, "tool_repeated", "warning", fmt.Sprintf("tool %s repeated with identical arguments", step.Name)))
		}
		events := []store.IncidentEvent{{IncidentID: run.IncidentID, RunID: uint64Ptr(run.ID), EventType: string(eventlog.EventLLMToolCalled), Phase: "llm", Status: "completed", Summary: safeEventSummary(fmt.Sprintf("tool=%s truncated=%v", step.Name, step.Truncated)), CreatedAt: finished}}
		p.appendStepRecord(ctx, store.RunStepRecord{Step: makeStep(run.ID, toolStepSeqBase+i, "tool", step.Name, step.Input, step.Output, stepErr, started, finished), Events: events, Problems: problems})
	}
}

// withStep executes the external stage outside SQL and commits one step plus
// its completion/failure event and fixed problem mutations in one short tx.
func (p *Pipeline) withStep(ctx context.Context, runID, incidentID uint64, seq int, kind, name string, fn func() (string, error), extras func() ([]store.IncidentEvent, []store.ProblemMutation)) (string, error) {
	started := time.Now().UTC()
	p.appendStartedEvent(ctx, incidentID, runID, kind, name, started)
	output, err := fn()
	finished := time.Now().UTC()
	events := []store.IncidentEvent{{IncidentID: incidentID, RunID: uint64Ptr(runID), EventType: stageCompletedEvent(kind, name, err), Phase: name, Status: eventStatus(err), Summary: safeEventSummary(stageSummary(name, err)), CreatedAt: finished}}
	problems := []store.ProblemMutation(nil)
	if extras != nil {
		extraEvents, extraProblems := extras()
		events = append(events, extraEvents...)
		problems = append(problems, extraProblems...)
	}
	p.appendStepRecord(ctx, store.RunStepRecord{Step: makeStep(runID, seq, kind, name, fmt.Sprintf("run=%d seq=%d", runID, seq), output, err, started, finished), Events: events, Problems: problems})
	return output, err
}

func (p *Pipeline) appendStartedEvent(ctx context.Context, incidentID, runID uint64, kind, name string, at time.Time) {
	eventType := string(eventlog.EventType(kind + ".started"))
	if kind == "evidence" {
		eventType = string(eventlog.EventCollectorStarted)
	}
	if kind == "llm" {
		eventType = string(eventlog.EventLLMStarted)
	}
	_, _ = p.db.AppendIncidentEvent(ctx, store.IncidentEvent{IncidentID: incidentID, RunID: uint64Ptr(runID), EventType: eventType, Phase: name, Status: "running", Summary: safeEventSummary(name + " started"), CreatedAt: at})
}

func (p *Pipeline) appendStep(ctx context.Context, runID, incidentID uint64, seq int, kind, name, input, output string, stepErr error) {
	now := time.Now().UTC()
	p.appendStepWithTimeProblems(ctx, runID, seq, kind, name, input, output, stepErr, now, now, nil)
}

func (p *Pipeline) appendStepWithTimeProblems(ctx context.Context, runID uint64, seq int, kind, name, input, output string, stepErr error, started, finished time.Time, problems []store.ProblemMutation) {
	p.appendStepWithTimeEventsProblems(ctx, runID, seq, kind, name, input, output, stepErr, started, finished, nil, problems)
}
func (p *Pipeline) appendStepWithTimeEventsProblems(ctx context.Context, runID uint64, seq int, kind, name, input, output string, stepErr error, started, finished time.Time, events []store.IncidentEvent, problems []store.ProblemMutation) {
	p.appendStepRecord(ctx, store.RunStepRecord{Step: makeStep(runID, seq, kind, name, input, output, stepErr, started, finished), Events: events, Problems: problems})
}

func (p *Pipeline) appendStepRecord(ctx context.Context, record store.RunStepRecord) {
	_ = p.db.AppendRunStepRecord(ctx, record)
}

func makeStep(runID uint64, seq int, kind, name, input, output string, stepErr error, started, finished time.Time) store.AgentRunStep {
	step := store.AgentRunStep{RunID: runID, Seq: seq, Kind: kind, Name: name, StartedAt: started.UTC(), FinishedAt: timePtr(finished.UTC())}
	if input != "" {
		step.InputJSON = toJSON(sanitizeForStep(input))
	}
	if output != "" {
		step.OutputJSON = toJSON(sanitizeForStep(output))
	}
	if stepErr != nil {
		msg := sanitizeForStep(stepErr.Error())
		step.Error = &msg
	}
	return step
}

func stageCompletedEvent(kind, name string, err error) string {
	if kind == "evidence" {
		if err != nil {
			return string(eventlog.EventCollectorFailed)
		}
		return string(eventlog.EventCollectorCompleted)
	}
	if kind == "llm" {
		if err != nil {
			return string(eventlog.EventLLMFailed)
		}
		return string(eventlog.EventLLMCompleted)
	}
	return string(eventlog.EventType(kind + "." + eventStatus(err)))
}
func eventStatus(err error) string {
	if err != nil {
		return "failed"
	}
	return "succeeded"
}
func stageSummary(name string, err error) string {
	if err != nil {
		return name + " failed: " + err.Error()
	}
	return name + " completed"
}
func safeEventSummary(text string) string {
	clean := sanitizeForStep(text)
	if clean == "" {
		return "pipeline event"
	}
	return clean
}
func uint64Ptr(v uint64) *uint64     { return &v }
func timePtr(v time.Time) *time.Time { return &v }

func openProblem(run store.AgentRun, code, severity, summary string) store.ProblemMutation {
	now := time.Now().UTC()
	return store.ProblemMutation{Kind: store.ProblemOpen, Problem: store.IncidentProblem{IncidentID: run.IncidentID, RunID: uint64Ptr(run.ID), Code: code, Severity: severity, Summary: safeEventSummary(summary), FirstSeenAt: now, LastSeenAt: now}}
}

func collectorProblems(incidentID, runID uint64, name, _ string, err error) []store.ProblemMutation {
	if name != "collect" {
		return nil
	}
	if err != nil {
		run := store.AgentRun{ID: runID, IncidentID: incidentID}
		return []store.ProblemMutation{openProblem(run, "collector_failed", "warning", err.Error())}
	}
	return nil
}

func collectorAudit(run store.AgentRun, evidence Evidence) ([]store.IncidentEvent, []store.ProblemMutation) {
	events := make([]store.IncidentEvent, 0, len(evidence.Items))
	problems := make([]store.ProblemMutation, 0, len(evidence.Items))
	for _, item := range evidence.Items {
		at := item.CollectedAt.UTC()
		if at.IsZero() {
			at = time.Now().UTC()
		}
		status := item.Status
		if status == "" {
			status = ItemMissing
		}
		failed := status == ItemError || status == ItemMissing
		typ := eventlog.EventCollectorCompleted
		if failed {
			typ = eventlog.EventCollectorFailed
		}
		events = append(events, store.IncidentEvent{IncidentID: run.IncidentID, RunID: uint64Ptr(run.ID), EventType: string(typ), Phase: "collector", Status: status, Summary: safeEventSummary(item.Name + " " + status), CreatedAt: at})
		code := "collector_failed"
		if item.Name != "" {
			code = "collector_failed_" + item.Name
		}
		if failed {
			problems = append(problems, openProblem(run, code, "warning", item.Err))
		} else {
			problems = append(problems, store.ProblemMutation{Kind: store.ProblemResolve, IncidentID: run.IncidentID, Code: code, RunID: uint64Ptr(run.ID), ResolvedAt: at})
		}
	}
	return events, problems
}
func notificationProblems(run store.AgentRun, err error) []store.ProblemMutation {
	if err == nil {
		return []store.ProblemMutation{{Kind: store.ProblemResolve, IncidentID: run.IncidentID, Code: "notification_failed", RunID: uint64Ptr(run.ID), ResolvedAt: time.Now().UTC()}}
	}
	return []store.ProblemMutation{openProblem(run, "notification_failed", "warning", err.Error())}
}

// sanitizeForStep 统一 step 摘要卫生：脱敏 + 截断。
func sanitizeForStep(text string) string {
	return TruncateForStep(Sanitize(text))
}

// TruncateForStep 按 step 预算截断（rune）。
func TruncateForStep(text string) string {
	runes := []rune(text)
	if len(runes) <= stepPayloadMaxRunes {
		return text
	}
	return string(runes[:stepPayloadMaxRunes]) + "…[truncated]"
}

// toJSON 把摘要文本包成合法 JSON 值（字符串字面量），匹配 JSON 列。
func toJSON(text string) *datatypes.JSON {
	encoded, err := json.Marshal(text)
	if err != nil {
		return nil
	}
	jsonValue := datatypes.JSON(encoded)
	return &jsonValue
}

// fail 把 run 标 failed 并返回原始错误语义。失败事件与 run 终态同批提交。
func (p *Pipeline) fail(ctx context.Context, run store.AgentRun, cause error) error {
	finished := time.Now().UTC()
	runID := run.ID
	completion := store.RunCompletion{RunID: run.ID, RCA: "", TokensIn: 0, TokensOut: 0, Status: "failed", FinishedAt: finished,
		Events: []store.IncidentEvent{{IncidentID: run.IncidentID, RunID: &runID, EventType: string(eventlog.EventRunFailed), Phase: "run", Status: "failed", Summary: safeEventSummary(cause.Error()), CreatedAt: finished}}}
	if err := p.db.CompleteRun(ctx, completion); err != nil {
		return fmt.Errorf("pipeline: fail run %d: %v (complete: %w)", run.ID, cause, err)
	}
	return fmt.Errorf("pipeline: run %d failed: %w", run.ID, cause)
}
