// Package approval 是 D10 的权限审批层：把诊断 Plan 翻译成确定性
// 安全决策（Policy），并管理 L3 审批单从创建到过期的生命周期。
// LLM 输出不是权限结论（GC-09）——所有执行许可都从这里出。
package approval

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"oncall-agent/internal/llm"
	"oncall-agent/internal/tools"
)

// 决策结果。
const (
	DecisionNone     = "none"     // 无动作，无需任何许可
	DecisionAutoL1   = "auto_l1"  // L1 只读，直接允许
	DecisionAutoL2   = "auto_l2"  // L2 低风险，条件全满足，自动路径
	DecisionApproval = "approval" // L3（或条件不满足的 L2 降级），创建审批单
	DecisionDenied   = "denied"   // L4 或未注册动作，硬拒绝
)

// Decision 是 Policy 对一个 Plan 的确定性结论。
type Decision struct {
	Kind     string
	ToolName string
	Args     json.RawMessage
	PlanHash string
	Reason   string
}

// PolicyConfig 是 L2 自动路径的护栏开关。
type PolicyConfig struct {
	// AutoExecuteL2 是全局开关：关闭时 L2 一律降级为审批。
	AutoExecuteL2 bool
	// DryRun 为真时 L2 只演练不执行（执行层检查）。
	DryRun bool
}

// Policy 把 Plan 翻译成决策。动作名必须在工具注册表里存在，
// 不存在即拒绝（GC-12，不在名单即拒绝）。
type Policy struct {
	registry *tools.Registry
	cfg      PolicyConfig
}

func NewPolicy(registry *tools.Registry, cfg PolicyConfig) *Policy {
	return &Policy{registry: registry, cfg: cfg}
}

// Decide 判定一个 Plan 的执行路径。action=none 短路。
// plan_hash 绑定 tool + args 的规范化内容，审批和执行前都验它。
func (p *Policy) Decide(plan llm.Plan) Decision {
	action := strings.TrimSpace(plan.Action)
	if action == "" || action == "none" {
		return Decision{Kind: DecisionNone, Reason: "no action"}
	}
	spec, ok := p.registry.Get(action)
	if !ok {
		return Decision{Kind: DecisionDenied, Reason: fmt.Sprintf("action %q is not a registered tool", action)}
	}
	args, err := CanonicalArgs(plan)
	if err != nil {
		return Decision{Kind: DecisionDenied, Reason: fmt.Sprintf("plan args are not encodable: %v", err)}
	}
	base := Decision{ToolName: action, Args: args, PlanHash: PlanHash(action, args)}
	switch spec.Level {
	case tools.L1ReadOnly:
		base.Kind = DecisionAutoL1
		base.Reason = "L1 read-only"
	case tools.L2LowRisk:
		// L2 自动路径的全部条件（GC-10/白名单/开关）；任一不满足降级审批。
		if p.cfg.AutoExecuteL2 && !p.cfg.DryRun {
			base.Kind = DecisionAutoL2
			base.Reason = "L2 guardrails satisfied"
		} else {
			base.Kind = DecisionApproval
			base.Reason = "L2 guardrails not satisfied, degraded to approval"
		}
	case tools.L3Approval:
		base.Kind = DecisionApproval
		base.Reason = "L3 requires approval"
	case tools.L4Forbidden:
		// L4 永远禁止，审批也不能解除（GC-10）。
		base.Kind = DecisionDenied
		base.Reason = "L4 is forbidden and cannot be approved"
	}
	return base
}

// CanonicalArgs 从 Plan 生成规范化参数 JSON：结构固定（target kind/name），
// 经 normalizeJSON 后键序与空白稳定，保证同一计划每次算出同一 plan_hash。
func CanonicalArgs(plan llm.Plan) (json.RawMessage, error) {
	args := map[string]string{
		"target_kind": plan.Target.Kind,
		"target_name": plan.Target.Name,
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	return normalizeJSON(encoded), nil
}

// PlanHash 计算计划内容指纹：审批绑定的是这份内容，
// 执行前重算比对，任何字段被篡改都会失配。
func PlanHash(toolName string, args json.RawMessage) string {
	sum := sha256.Sum256(append([]byte(toolName+"\x00"), normalizeJSON(args)...))
	return hex.EncodeToString(sum[:])
}

// normalizeJSON 把 JSON 归一成规范形态：Unmarshal → Marshal 重编码。
// MySQL JSON 列会重排键序和空白，只去空格治不了键序，必须全量重编码；
// Go 的 map 序列化按键排序，两侧走同一归一即可稳定比对。
func normalizeJSON(raw json.RawMessage) []byte {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return raw // 非法 JSON 原样参与哈希：Create 端同样走这里，两侧一致即可
	}
	normalized, err := json.Marshal(value)
	if err != nil {
		return raw
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, normalized); err != nil {
		return normalized
	}
	return buf.Bytes()
}
