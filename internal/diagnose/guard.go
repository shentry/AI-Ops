package diagnose

import (
	"strings"

	"oncall-agent/internal/llm"
)

// Guard 决策。
const (
	DecisionAllow    = "allow"    // 计划原样通过（执行权仍归 D10 Policy）
	DecisionDeny     = "deny"     // 计划被拒绝
	DecisionEscalate = "escalate" // 升级人工处理
)

// GuardResult 是 Guard 的结论。改写后的 Plan 随结果返回，
// 改写原因进 Reason —— 对应参考实现的 _overridden 字段，
// 通知层必须能看到"这条被规则改过"。
type GuardResult struct {
	Decision   string
	Plan       llm.Plan
	Overridden bool
	Reason     string
}

// GuardRule = 谓词 + 改写 + 原因。代码优先于 LLM（GC-09）：
// 已知幻觉/危险模式在确定性规则里硬拦，LLM 之后没有任何环节能改回。
type GuardRule struct {
	Name    string
	Matches func(rca string, plan llm.Plan) bool
	Apply   func(plan llm.Plan) GuardResult
}

// nonRestartableKeywords 是"重启无救"类根因的关键词（R3 直译）：
// 命中这些的故障重启进程解决不了，盲目重启只会抹掉现场。
var nonRestartableKeywords = []string{
	"配置错误", "config error", "配置缺失",
	"镜像不存在", "imagepullbackoff", "image pull", "no such image",
	"凭据", "unauthorized", "认证失败",
}

// Guard 按顺序应用规则，命中即返回。规则是确定性代码，不接受 LLM 输入改写。
func Guard(rca string, plan llm.Plan) GuardResult {
	rules := []GuardRule{
		{
			// R-target：有动作但没有真实 target。LLM 编造或缺失 target 的
			// 计划不许往下走（GC-11 的前置闸）。
			Name: "action_without_target",
			Matches: func(_ string, plan llm.Plan) bool {
				action := strings.TrimSpace(strings.ToLower(plan.Action))
				if action == "" || action == "none" {
					return false
				}
				name := strings.TrimSpace(strings.ToLower(plan.Target.Name))
				return name == "" || name == "none"
			},
			Apply: func(plan llm.Plan) GuardResult {
				plan.Action = "none"
				plan.Target = llm.PlanTarget{Kind: "none", Name: "none"}
				return GuardResult{Decision: DecisionDeny, Plan: plan, Overridden: true,
					Reason: "plan has action but no real target; forced to none"}
			},
		},
		{
			// R3：根因是配置/镜像/凭据类错误时禁止重启类动作，
			// 强制 none + 升级人工。
			Name: "non_restartable_failure",
			Matches: func(rca string, plan llm.Plan) bool {
				action := strings.ToLower(plan.Action)
				if !strings.Contains(action, "restart") && !strings.Contains(action, "reboot") {
					return false
				}
				text := strings.ToLower(rca + " " + plan.Reason)
				for _, keyword := range nonRestartableKeywords {
					if strings.Contains(text, keyword) {
						return true
					}
				}
				return false
			},
			Apply: func(plan llm.Plan) GuardResult {
				plan.Action = "none"
				plan.Target = llm.PlanTarget{Kind: "none", Name: "none"}
				return GuardResult{Decision: DecisionEscalate, Plan: plan, Overridden: true,
					Reason: "root cause looks like config/image/credential failure; restart blocked, escalate to human"}
			},
		},
	}
	for _, rule := range rules {
		if rule.Matches(rca, plan) {
			return rule.Apply(plan)
		}
	}
	return GuardResult{Decision: DecisionAllow, Plan: plan}
}
