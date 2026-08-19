package diagnose

import (
	"context"
	"fmt"
	"time"

	"oncall-agent/internal/store"
)

// verifyStore 是 Verifier 对存储层的收窄接口。
type verifyStore interface {
	ListIncidentMembers(ctx context.Context, incidentID uint64) ([]store.IncidentMember, error)
	AppendRunStep(ctx context.Context, step store.AgentRunStep) error
}

// VerifyResult 是一次恢复验证的结论。
type VerifyResult struct {
	Passed bool
	Detail string
}

// Verifier 做执行后的独立验证（GC-15：执行成功 ≠ 故障恢复）。
// V1 判定固定为一种：审批关联 incident 的成员告警全部 resolved
// （配合 simulate -resolved 可控演示成功/失败两条路径）。
type Verifier struct {
	db verifyStore
}

func NewVerifier(db verifyStore) *Verifier {
	return &Verifier{db: db}
}

// VerifyAfterExecution 延时复查：给系统留出自愈时间再判定。
// 结果落 agent_run_step(kind=verify)，挂在审批来源的 run 上（GC-18 回放链）。
func (v *Verifier) VerifyAfterExecution(ctx context.Context, runID, incidentID uint64, delay time.Duration) VerifyResult {
	if delay > 0 {
		select {
		case <-ctx.Done():
			// 取消（进程退出等）也落 step：审计链要能看到"验证没跑完"，
			// 否则这条 run 看起来像永远等不到 verify。
			result := VerifyResult{Passed: false, Detail: "verify canceled"}
			v.record(context.WithoutCancel(ctx), runID, incidentID, delay, result, ctx.Err())
			return result
		case <-time.After(delay):
		}
	}
	result := v.check(ctx, incidentID)
	v.record(ctx, runID, incidentID, delay, result, nil)
	return result
}

func (v *Verifier) check(ctx context.Context, incidentID uint64) VerifyResult {
	members, err := v.db.ListIncidentMembers(ctx, incidentID)
	if err != nil {
		return VerifyResult{Passed: false, Detail: fmt.Sprintf("read members failed: %v", err)}
	}
	firing := 0
	for _, member := range members {
		if member.Status != "resolved" {
			firing++
		}
	}
	if firing > 0 {
		return VerifyResult{Passed: false, Detail: fmt.Sprintf("%d/%d members still firing", firing, len(members))}
	}
	return VerifyResult{Passed: true, Detail: fmt.Sprintf("all %d members resolved", len(members))}
}

func (v *Verifier) record(ctx context.Context, runID, incidentID uint64, delay time.Duration, result VerifyResult, stepErr error) {
	now := time.Now().UTC()
	output := fmt.Sprintf("incident=%d delay=%s passed=%v detail=%s", incidentID, delay, result.Passed, Sanitize(result.Detail))
	step := store.AgentRunStep{
		RunID:      runID,
		Seq:        90, // verify 在 pipeline 主链（1-5）之后，用高位段避免撞号
		Kind:       "verify",
		Name:       "last_alert_recheck",
		StartedAt:  now,
		FinishedAt: &now,
	}
	if j := toJSON(output); j != nil {
		step.OutputJSON = j
	}
	if stepErr != nil {
		msg := Sanitize(stepErr.Error())
		step.Error = &msg
	}
	_ = v.db.AppendRunStep(ctx, step)
}
