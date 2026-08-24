package diagnose

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"gorm.io/datatypes"

	"oncall-agent/internal/eventlog"
	"oncall-agent/internal/store"
)

// verifyDetachedTimeout 是 Verify 自己的读写时限。Verify 的两次落库
// （复查查询 + 审计 step）都脱离调用方 ctx：进程正在关闭时，验证结论
// 既不能被写成"失败"，也不能干脆丢掉 —— 但也不许无限期挂住关闭流程。
const verifyDetachedTimeout = 10 * time.Second

// verifyStore 是 Verify 的窄接口：一次成员复查读，一次审计批次写。
// 批次接口保证 step、event、problem 原子提交。
type verifyStore interface {
	ListIncidentMembers(ctx context.Context, incidentID uint64) ([]store.IncidentMember, error)
	AppendRunStepRecord(ctx context.Context, record store.RunStepRecord) error
}

// VerifyResult 是一次恢复验证的结论。
// Inconclusive 表示"没能判定"，区别于"判定为没恢复"：取消、读库失败、
// 没有可复查的成员都属于这一类。不可判定不能触发重诊或记忆降级 ——
// 那是拿运行环境的问题去惩罚诊断结论，只能升级人工核查。
type VerifyResult struct {
	Passed       bool
	Inconclusive bool
	Detail       string
}

// Verifier 做执行后的独立验证（GC-15：执行成功 ≠ 故障恢复）。
// V1 判定固定为一种：审批关联 incident 的成员告警全部 resolved
// （配合 simulate -resolved 可控演示成功/失败两条路径）。
type Verifier struct {
	db     verifyStore
	logger *log.Logger
}

func NewVerifier(db verifyStore, logger *log.Logger) *Verifier {
	if logger == nil {
		logger = log.Default()
	}
	return &Verifier{db: db, logger: logger}
}

// VerifyAfterExecution 延时复查：给系统留出自愈时间再判定。
// 结果落 agent_run_step(kind=verify)，挂在审批来源的 run 上（GC-18 回放链）。
func (v *Verifier) VerifyAfterExecution(ctx context.Context, runID, incidentID uint64, delay time.Duration) VerifyResult {
	if delay > 0 {
		select {
		case <-ctx.Done():
			// 取消（进程退出等）也落 step：审计链要能看到"验证没跑完"，
			// 否则这条 run 看起来像永远等不到 verify。
			return v.finish(ctx, runID, incidentID, delay,
				VerifyResult{Inconclusive: true, Detail: "verify canceled before recheck"}, ctx.Err())
		case <-time.After(delay):
		}
	}
	if err := ctx.Err(); err != nil {
		return v.finish(ctx, runID, incidentID, delay,
			VerifyResult{Inconclusive: true, Detail: "verify canceled before recheck"}, err)
	}
	// 复查查询用脱离取消的 ctx：调用方 ctx 在这一刻被取消时，
	// 查询会返回 context canceled，那会被误记成"故障没恢复"。
	checkCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), verifyDetachedTimeout)
	defer cancel()
	return v.finish(ctx, runID, incidentID, delay, v.check(checkCtx, incidentID), nil)
}

// finish 落审计 step 并返回结论，保证"有结论必有留痕"只有一条路径。
func (v *Verifier) finish(ctx context.Context, runID, incidentID uint64, delay time.Duration, result VerifyResult, stepErr error) VerifyResult {
	v.record(ctx, runID, incidentID, delay, result, stepErr)
	return result
}

func (v *Verifier) check(ctx context.Context, incidentID uint64) VerifyResult {
	members, err := v.db.ListIncidentMembers(ctx, incidentID)
	if err != nil {
		return VerifyResult{Inconclusive: true, Detail: fmt.Sprintf("read members failed: %v", err)}
	}
	// 没有成员 = 没有任何可复查的对象。这不是"全部恢复"：空集合判成功
	// 会让任何动作都被标记为已修复，并把它送进记忆提交流程。
	if len(members) == 0 {
		return VerifyResult{Inconclusive: true, Detail: "incident has no members to recheck"}
	}
	firing := 0
	for _, member := range members {
		if member.Status != "resolved" {
			firing++
		}
	}
	if firing > 0 {
		return VerifyResult{Detail: fmt.Sprintf("%d/%d members still firing", firing, len(members))}
	}
	return VerifyResult{Passed: true, Detail: fmt.Sprintf("all %d members resolved", len(members))}
}

func (v *Verifier) record(ctx context.Context, runID, incidentID uint64, delay time.Duration, result VerifyResult, stepErr error) {
	now := time.Now().UTC()
	detail := verifyText(result.Detail)
	output := fmt.Sprintf("incident=%d delay=%s passed=%v inconclusive=%v detail=%s",
		incidentID, delay, result.Passed, result.Inconclusive, detail)
	step := store.AgentRunStep{
		RunID:      runID,
		Seq:        90, // verify 在 pipeline 主链（1-6）之后，用高位段避免撞号
		Kind:       "verify",
		Name:       "last_alert_recheck",
		StartedAt:  now,
		FinishedAt: &now,
	}
	if j := toJSON(TruncateForStep(output)); j != nil {
		step.OutputJSON = j
	}
	if stepErr != nil {
		msg := verifyText(stepErr.Error())
		step.Error = &msg
	}

	payloadBytes, _ := json.Marshal(struct {
		Passed       bool   `json:"passed"`
		Inconclusive bool   `json:"inconclusive"`
		Detail       string `json:"detail"`
	}{Passed: result.Passed, Inconclusive: result.Inconclusive, Detail: detail})
	eventPayload := datatypes.JSON(payloadBytes)
	problemPayload := datatypes.JSON(append([]byte(nil), payloadBytes...))
	runRef := runID
	eventType := eventlog.EventVerifyFailed
	eventStatus := "failed"
	eventSummary := "verify failed"
	problems := make([]store.ProblemMutation, 0, 2)
	switch {
	case result.Inconclusive:
		eventType = eventlog.EventVerifyInconclusive
		eventStatus = "inconclusive"
		eventSummary = "verify inconclusive"
		problems = append(problems, store.ProblemMutation{
			Kind: store.ProblemOpen,
			Problem: store.IncidentProblem{
				IncidentID: incidentID, RunID: &runRef, Code: "verify_inconclusive",
				Severity: "warning", Status: "open", Summary: "verification was inconclusive",
				DetailJSON: &problemPayload, FirstSeenAt: now, LastSeenAt: now,
			},
		})
	case result.Passed:
		eventType = eventlog.EventVerifyPassed
		eventStatus = "passed"
		eventSummary = "verify passed"
		problems = append(problems,
			store.ProblemMutation{Kind: store.ProblemResolve, IncidentID: incidentID, Code: "verify_failed", RunID: &runRef, ResolvedAt: now},
			store.ProblemMutation{Kind: store.ProblemResolve, IncidentID: incidentID, Code: "verify_inconclusive", RunID: &runRef, ResolvedAt: now},
		)
	default:
		problems = append(problems,
			store.ProblemMutation{
				Kind: store.ProblemOpen,
				Problem: store.IncidentProblem{
					IncidentID: incidentID, RunID: &runRef, Code: "verify_failed",
					Severity: "error", Status: "open", Summary: "verification found members still firing",
					DetailJSON: &problemPayload, FirstSeenAt: now, LastSeenAt: now,
				},
			},
			store.ProblemMutation{Kind: store.ProblemResolve, IncidentID: incidentID, Code: "verify_inconclusive", RunID: &runRef, ResolvedAt: now},
		)
	}
	record := store.RunStepRecord{
		Step: step,
		Events: []store.IncidentEvent{{
			IncidentID: incidentID, RunID: &runRef,
			EventType: string(eventType), Phase: "verify", Status: eventStatus,
			Summary: eventSummary, PayloadJSON: &eventPayload, CreatedAt: now,
		}},
		Problems: problems,
	}

	// 审计写入同样脱离取消：关闭中的进程也要留下这条结论。
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), verifyDetachedTimeout)
	defer cancel()
	if err := v.db.AppendRunStepRecord(writeCtx, record); err != nil {
		v.logger.Printf("verify: append step record for run %d failed: %v (result: %s)", runID, err, output)
	}
}

// verifyText 对进入事件、问题和 step 的结论做脱敏、控制字符清理和上限约束。
func verifyText(text string) string {
	text = Sanitize(ToSafeText(text))
	runes := []rune(text)
	if len(runes) <= 480 {
		return text
	}
	return string(runes[:480]) + "…[truncated]"
}
