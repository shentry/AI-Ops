package diagnose

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"gorm.io/datatypes"

	"oncall-agent/internal/approval"
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
	AppendRunStep(ctx context.Context, step store.AgentRunStep) error
	CompleteAgentRun(ctx context.Context, id uint64, rca string, planJSON []byte, tokensIn, tokensOut int, status string, finishedAt time.Time) error
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
	Decide(plan llm.Plan) approval.Decision
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
		return p.fail(ctx, run.ID, err)
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
				return p.fail(ctx, run.ID, fmt.Errorf("memory plan undecodable: %w", err))
			}
			// run mode 改写为 memory_hit：审计能区分 LLM 诊断和记忆命中。
			if err := p.db.UpdateAgentRunMode(ctx, run.ID, "memory_hit"); err != nil {
				return p.fail(ctx, run.ID, fmt.Errorf("mark memory_hit: %w", err))
			}
			run.Mode = "memory_hit"
		}
		p.appendStep(ctx, run.ID, 1, "tool", "memory_lookup",
			fmt.Sprintf("fingerprint=%s retry_of=%v", fingerprint, run.RetryOf),
			fmt.Sprintf("hit=%v hits=%d err=%v", hit, memoryHits, lookupErr != nil), lookupErr)
		metrics.Inc(map[bool]string{true: metrics.MemoryHit, false: metrics.MemoryMiss}[hit])
	}

	if run.Mode != "memory_hit" {
		// 阶段 2：证据采集。重诊 run 额外注入上一轮的失败结论；
		// 记忆 miss 但有历史命令时注入作为参考证据（不构成权限依据）。
		evidence, err := p.withStep(ctx, run.ID, 2, "evidence", "collect", func() (string, error) {
			ev, err := p.builder.BuildForIncident(ctx, run.IncidentID)
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
		})
		if err != nil {
			return p.fail(ctx, run.ID, err)
		}

		// 阶段 3：LLM 推理。mode=skip 的行不会进队列（D05 已直接落 succeeded）。
		result, err := p.runReasonStep(ctx, run, evidence)
		if err != nil {
			return p.fail(ctx, run.ID, err)
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
	p.appendStep(ctx, run.ID, 4, "guard", "rules",
		fmt.Sprintf("action=%s target=%s/%s", plan.Action, plan.Target.Kind, plan.Target.Name),
		fmt.Sprintf("decision=%s overridden=%v reason=%s", guardResult.Decision, guardResult.Overridden, guardResult.Reason), nil)

	planBytes, err := json.Marshal(plan)
	if err != nil {
		return p.fail(ctx, run.ID, fmt.Errorf("marshal plan: %w", err))
	}

	// 阶段 5：Policy。Guard 之后的 plan 翻译成确定性执行决策；
	// L3（或条件不满足的 L2）在这里落 pending 审批单（GC-13）。
	decision := p.policy.Decide(plan)
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
			return p.fail(ctx, run.ID, fmt.Errorf("create approval: %w", createErr))
		}
		approvalID = &created.ID
		metrics.Inc(metrics.ApprovalCreated)
	case approval.DecisionAutoL2:
		// 自动 L2 也走审批单通道（系统批准），执行面只有一个入口。
		created, createErr := p.approvals.CreateSystemApproved(ctx, run.IncidentID, run.ID, decision, Sanitize(decision.Reason))
		if createErr != nil {
			return p.fail(ctx, run.ID, fmt.Errorf("create system approval: %w", createErr))
		}
		approvalID = &created.ID
		metrics.Inc(metrics.ApprovalCreated)
	}
	p.appendStep(ctx, run.ID, 5, "approval", "policy",
		fmt.Sprintf("action=%s tool=%s", plan.Action, decision.ToolName),
		fmt.Sprintf("decision=%s reason=%s approval_id=%v", decision.Kind, decision.Reason, approvalID), nil)

	// 阶段 6：报告。通知失败独立记录，不改变 run 终态（验收清单）。
	report := DiagnosisReport{
		IncidentID: run.IncidentID, RunID: run.ID, Mode: run.Mode,
		RCA: rca, Confidence: confidence, Plan: plan,
		Decision: guardResult.Decision, Overridden: guardResult.Overridden, GuardNote: guardResult.Reason,
		PolicyDecision: decision.Kind, ApprovalID: approvalID, MemoryHits: memoryHits,
	}
	notifyErr := p.reporter.NotifyDiagnosis(ctx, report)
	p.appendStep(ctx, run.ID, 6, "tool", "notify", "diagnosis report", fmt.Sprintf("decision=%s", guardResult.Decision), notifyErr)

	// 终态落库。
	if err := p.db.CompleteAgentRun(ctx, run.ID, rca, planBytes, tokensIn, tokensOut, "succeeded", time.Now().UTC()); err != nil {
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
func (p *Pipeline) runReasonStep(ctx context.Context, run store.AgentRun, evidence string) (*llm.DiagnoseResult, error) {
	var result *llm.DiagnoseResult
	_, err := p.withStep(ctx, run.ID, 3, "llm", "reason", func() (string, error) {
		out, err := p.reasoner.Diagnose(ctx, evidence, run.Mode)
		if err != nil {
			return "", err
		}
		result = out
		return fmt.Sprintf("rca=%s confidence=%s tokens=%d/%d", out.RCA, out.Confidence, out.TokensIn, out.TokensOut), nil
	})
	return result, err
}

// withStep 执行一个阶段并落 step（含耗时和错误）。返回阶段产出。
func (p *Pipeline) withStep(ctx context.Context, runID uint64, seq int, kind, name string, fn func() (string, error)) (string, error) {
	started := time.Now().UTC()
	output, err := fn()
	finished := time.Now().UTC()
	input := fmt.Sprintf("run=%d seq=%d", runID, seq)
	p.appendStepWithTime(ctx, runID, seq, kind, name, input, output, err, started, finished)
	return output, err
}

func (p *Pipeline) appendStep(ctx context.Context, runID uint64, seq int, kind, name, input, output string, stepErr error) {
	now := time.Now().UTC()
	p.appendStepWithTime(ctx, runID, seq, kind, name, input, output, stepErr, now, now)
}

func (p *Pipeline) appendStepWithTime(ctx context.Context, runID uint64, seq int, kind, name, input, output string, stepErr error, started, finished time.Time) {
	step := store.AgentRunStep{
		RunID:      runID,
		Seq:        seq,
		Kind:       kind,
		Name:       name,
		StartedAt:  started,
		FinishedAt: &finished,
	}
	if input != "" {
		step.InputJSON = toJSON(sanitizeForStep(input))
	}
	if output != "" {
		step.OutputJSON = toJSON(sanitizeForStep(output))
	}
	if stepErr != nil {
		message := Sanitize(stepErr.Error())
		step.Error = &message
	}
	// step 落库失败只记日志诉求在调用方 —— 这里尽力而为，不让审计失败
	// 反过来决定诊断成败；run 终态由 CompleteAgentRun 独立保证。
	_ = p.db.AppendRunStep(ctx, step)
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

// fail 把 run 标 failed 并返回错误给 worker 记日志。
func (p *Pipeline) fail(ctx context.Context, runID uint64, cause error) error {
	if err := p.db.CompleteAgentRun(ctx, runID, "", nil, 0, 0, "failed", time.Now().UTC()); err != nil {
		return fmt.Errorf("pipeline: fail run %d: %v (complete: %w)", runID, cause, err)
	}
	return fmt.Errorf("pipeline: run %d failed: %w", runID, cause)
}
