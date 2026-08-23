// Package approval 是 D10 的权限审批层：把诊断 Plan 翻译成确定性
// 安全决策（Policy），并管理 L3 审批单从创建到过期的生命周期。
// LLM 输出不是权限结论（GC-09）——所有执行许可都从这里出。
package approval

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

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
	// AllowedTargets 是 L2 自动路径的目标白名单（来自 tools.docker.allowed_containers）。
	// 空 = 没有任何目标可自动动作，L2 全部降级审批（fail closed）。
	AllowedTargets []string
	// RateWindow / MaxPerWindow 是限频护栏：同一 plan_hash（= 同 tool + 同 target）
	// 在窗口内已执行 MaxPerWindow 次后不再自动执行，降级审批。
	RateWindow   time.Duration
	MaxPerWindow int
}

// PolicyInput 是 Plan 之外、L2 护栏需要的事实输入。由调用方（Pipeline）
// 从 incident 上下文装配 —— Policy 只做判定，不自己查库找证据。
type PolicyInput struct {
	// KnownTargets 是可信 target 来源：告警标签/服务清单/运行时查询里出现过的
	// 真实对象名。plan.Target.Name 不在其中 = LLM 自己编的名字（GC-11）。
	KnownTargets []string
	// Verifiable 表示这次动作事后能被 Verify 判定（incident 有成员告警可复查）。
	// 不可验证的动作不许自动执行：那等于"执行完没人知道有没有修好"。
	Verifiable bool
}

// executionCounter 让 Policy 查限频窗口内的执行次数（同 plan_hash）。
// 查不到（出错）时 Policy 按超限处理 —— 限频护栏 fail closed。
type executionCounter interface {
	CountRecentExecutions(ctx context.Context, planHash string, since time.Time) (int, error)
}

// Policy 把 Plan 翻译成决策。动作名必须在工具注册表里存在，
// 不存在即拒绝（GC-12，不在名单即拒绝）。
type Policy struct {
	registry *tools.Registry
	cfg      PolicyConfig
	counter  executionCounter
	allowed  map[string]bool
	now      func() time.Time
}

// NewPolicy 装配策略。counter 为空表示没有限频数据源 —— 此时 L2 自动路径
// 一律降级审批，而不是"没数据就放行"。
func NewPolicy(registry *tools.Registry, cfg PolicyConfig, counter executionCounter) *Policy {
	allowed := make(map[string]bool, len(cfg.AllowedTargets))
	for _, name := range cfg.AllowedTargets {
		if trimmed := strings.TrimSpace(name); trimmed != "" {
			allowed[trimmed] = true
		}
	}
	return &Policy{registry: registry, cfg: cfg, counter: counter, allowed: allowed, now: func() time.Time { return time.Now().UTC() }}
}

// Decide 判定一个 Plan 的执行路径。action=none 短路。
// plan_hash 绑定 tool + args 的规范化内容，审批和执行前都验它。
func (p *Policy) Decide(ctx context.Context, plan llm.Plan, input PolicyInput) Decision {
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
		// L2 自动路径必须同时满足全部护栏（GC-10）；任一不满足降级审批。
		if blocked := p.l2Guardrails(ctx, plan, input, base.PlanHash); blocked != "" {
			base.Kind = DecisionApproval
			base.Reason = "L2 guardrail not satisfied (" + blocked + "), degraded to approval"
		} else {
			base.Kind = DecisionAutoL2
			base.Reason = "L2 guardrails satisfied"
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

// broadTargetKinds 是影响面不受控的 target 类型：L2 只允许单实例受控动作，
// 对集群/主机/命名空间/数据库这类范围的动作一律走人工审批。
var broadTargetKinds = map[string]bool{
	"cluster": true, "host": true, "node": true, "namespace": true,
	"database": true, "db": true, "all": true, "group": true, "zone": true, "region": true,
}

// l2Guardrails 逐条检查设计要求的 L2 护栏，返回第一条不满足的护栏名；
// 全部满足返回空串。顺序按"越便宜越先查"排列，限频查库放最后。
func (p *Policy) l2Guardrails(ctx context.Context, plan llm.Plan, input PolicyInput, planHash string) string {
	if !p.cfg.AutoExecuteL2 {
		return "auto_execute_l2 disabled"
	}
	if p.cfg.DryRun {
		return "dry_run enabled"
	}
	targetKind := strings.ToLower(strings.TrimSpace(plan.Target.Kind))
	targetName := strings.TrimSpace(plan.Target.Name)
	if targetKind == "" || targetName == "" {
		return "target is incomplete"
	}
	// 影响范围：只接受单个具体对象。通配、列表分隔符、宽范围 kind 全部降级。
	if strings.ContainsAny(targetName, "*?,; \t") || strings.EqualFold(targetName, "all") {
		return "target is not a single concrete object"
	}
	if broadTargetKinds[targetKind] {
		return fmt.Sprintf("target kind %q has unbounded blast radius", targetKind)
	}
	// 白名单：配置之外的目标没有自动执行资格（GC-11）。
	if !p.allowed[targetName] {
		return fmt.Sprintf("target %q is not in the auto-execute allowlist", targetName)
	}
	// target 来源：必须在告警标签/服务清单/运行时查询里出现过，不能是 LLM 编的。
	if !matchesKnownTarget(targetName, input.KnownTargets) {
		return fmt.Sprintf("target %q does not come from alert labels or runtime lookup", targetName)
	}
	// 可验证：执行完必须有东西可复查，否则"成功"无法判定。
	if !input.Verifiable {
		return "action outcome is not verifiable"
	}
	// 限频：同 target+action 在窗口内已达上限就不再自动执行。
	if p.counter == nil || p.cfg.RateWindow <= 0 || p.cfg.MaxPerWindow < 1 {
		return "rate limit is not configured"
	}
	count, err := p.counter.CountRecentExecutions(ctx, planHash, p.now().Add(-p.cfg.RateWindow))
	if err != nil {
		return "rate limit check failed: " + err.Error()
	}
	if count >= p.cfg.MaxPerWindow {
		return fmt.Sprintf("rate limit reached (%d in %s)", count, p.cfg.RateWindow)
	}
	return ""
}

// matchesKnownTarget 比对 target 与可信来源。除全等（忽略大小写）外，
// 还接受 instance 标签常见的 host:port 形态里的主机段。
func matchesKnownTarget(name string, known []string) bool {
	for _, candidate := range known {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		if strings.EqualFold(candidate, name) {
			return true
		}
		if host, _, found := strings.Cut(candidate, ":"); found && strings.EqualFold(strings.TrimSpace(host), name) {
			return true
		}
	}
	return false
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
