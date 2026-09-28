package diagnose

import (
	"fmt"
	"strconv"
	"strings"

	"oncall-agent/internal/incident"
	"oncall-agent/internal/llm"
	"oncall-agent/internal/tools"
)

// Guard 决策。
const (
	DecisionAllow    = "allow"    // 计划原样通过（执行权仍归处置规则与 Policy）
	DecisionDeny     = "deny"     // 计划被拒绝
	DecisionEscalate = "escalate" // 升级人工处理
)

// GuardResult 是 Guard 的结论。改写后的 Plan 随结果返回，改写原因进 Reason，
// 通知层必须能看到"这条被规则改过"。Target 是允许的动作经本次证据确认的
// 目标身份，Policy 冻结快照时以它核对实时对象。
type GuardResult struct {
	Decision   string
	Plan       llm.Plan
	Overridden bool
	Reason     string
	Target     incident.Object
}

// guardRule 命中时返回决策与原因，计划一律改写为 none。代码优先于 LLM：
// 动作前提只读当前证据的结构化事实，模型引用的证据名只是解释，不能替代这里的检查。
type guardRule func(rca string, plan llm.Plan, evidence Evidence) (decision, reason string)

var commonRules = []guardRule{actionWithoutTarget, dependencyUnavailable}

// actionRules 是每个动作要求的事实。没有规则的动作一律拒绝（fail closed）。
var actionRules = map[string][]guardRule{
	tools.ActionDockerRestart:      {nonRestartableFailure, restartTargetIdentity, restartPreconditions},
	tools.ActionDeploymentRollback: {rollbackPreconditions},
	tools.ActionUpstreamQuarantine: {quarantinePreconditions},
}

// Guard 按顺序应用规则，命中即返回。规则是确定性代码，不接受 LLM 输入改写。
// evidence 必须是本次 run 当前采集的证据，记忆命中的计划同样按当前事实复核。
func Guard(rca string, plan llm.Plan, evidence Evidence) GuardResult {
	action := strings.TrimSpace(plan.Action)
	if action == "" || action == "none" {
		return GuardResult{Decision: DecisionAllow, Plan: plan}
	}
	rules, known := actionRules[action]
	if !known {
		return override(plan, DecisionDeny, "no guard rules exist for action "+action)
	}
	for _, rule := range append(append([]guardRule{}, commonRules...), rules...) {
		if decision, reason := rule(rca, plan, evidence); decision != "" {
			return override(plan, decision, reason)
		}
	}
	return GuardResult{Decision: DecisionAllow, Plan: plan, Target: trustedTarget(plan, evidence)}
}

func override(plan llm.Plan, decision, reason string) GuardResult {
	plan.Action, plan.Params = "none", nil
	plan.Target = llm.PlanTarget{Kind: "none", Name: "none"}
	return GuardResult{Decision: decision, Plan: plan, Overridden: true, Reason: reason}
}

// trustedTarget is the identity the passing rules established: the container
// ID from docker_inspect, the current release ID from the release records, or
// the upstream account ID present in sub2api's own state.
func trustedTarget(plan llm.Plan, evidence Evidence) incident.Object {
	target := incident.Object{Kind: plan.Target.Kind, Name: plan.Target.Name}
	switch plan.Action {
	case tools.ActionDockerRestart:
		item, _ := evidence.Item("docker_inspect")
		target.ID = item.Object.ID
	case tools.ActionDeploymentRollback:
		item, _ := evidence.Item("recent_changes")
		target.ID = item.Release.CurrentID
	case tools.ActionUpstreamQuarantine:
		target.ID = plan.Target.Name
	}
	return target
}

// actionWithoutTarget：有动作但没有真实 target。编造或缺失 target 的计划不许往下走。
func actionWithoutTarget(_ string, plan llm.Plan, _ Evidence) (string, string) {
	name := strings.TrimSpace(strings.ToLower(plan.Target.Name))
	if name == "" || name == "none" {
		return DecisionDeny, "plan has action but no real target; forced to none"
	}
	return "", ""
}

// dependencyUnavailable：数据库或 Redis 不可用时，重启、回退和上游隔离都不是
// 有依据的补救，交还人工。
func dependencyUnavailable(_ string, _ llm.Plan, evidence Evidence) (string, string) {
	for _, dependency := range []string{"postgres", "redis"} {
		if item, ok := evidence.Item(dependency); ok && item.Status == ItemError {
			return DecisionEscalate, dependency + " is unavailable; application actions do not address it"
		}
	}
	return "", ""
}

// nonRestartableKeywords 是"重启无救"类根因的关键词：命中这些的故障重启进程
// 解决不了，盲目重启只会抹掉现场。
var nonRestartableKeywords = []string{
	"配置错误", "config error", "配置缺失",
	"镜像不存在", "imagepullbackoff", "image pull", "no such image",
	"凭据", "unauthorized", "认证失败",
}

// nonRestartableFailure：诊断自己认定是配置/镜像/凭据类错误时禁止重启，升级人工。
func nonRestartableFailure(rca string, plan llm.Plan, _ Evidence) (string, string) {
	text := strings.ToLower(rca + " " + plan.Reason)
	for _, keyword := range nonRestartableKeywords {
		if strings.Contains(text, keyword) {
			return DecisionEscalate, "root cause looks like config/image/credential failure; restart blocked, escalate to human"
		}
	}
	return "", ""
}

// restartTargetIdentity：重启目标必须由本次 docker_inspect 的可信响应确认身份。
// 仅"证据里出现过这个名字"不够：告警标签和无身份日志都不能证明对象。
func restartTargetIdentity(_ string, plan llm.Plan, evidence Evidence) (string, string) {
	item, ok := evidence.Item("docker_inspect")
	if !ok || item.Status != ItemOK || item.Object == nil || item.Object.ID == "" || item.Container == nil {
		return DecisionDeny, "restart target identity is not established by current docker_inspect evidence"
	}
	if plan.Target.Kind != "container" || plan.Target.Name != item.Object.Name {
		return DecisionDeny, "restart target is not the container identified by docker_inspect"
	}
	return "", ""
}

// restartPreconditions：进程恢复只对"已退出且不会自愈"或"在运行但探测失败（卡死）"
// 的目标成立。Docker 正在恢复、人为停止、目标健康都不是重启的理由。
func restartPreconditions(_ string, _ llm.Plan, evidence Evidence) (string, string) {
	inspect, _ := evidence.Item("docker_inspect")
	container := inspect.Container
	if container.Restarting {
		return DecisionDeny, "docker restart policy is already restarting the container"
	}
	if !container.Running {
		// always/unless-stopped 下容器停着只能是被人为 stop：可能是维护，交还人工。
		if container.RestartPolicy == "always" || container.RestartPolicy == "unless-stopped" {
			return DecisionEscalate, "container was stopped although restart policy " + container.RestartPolicy + " would restart it; treat as a deliberate stop"
		}
		return "", ""
	}
	health, ok := evidence.Item("sub2api_health")
	if !ok || health.Health == nil {
		return DecisionDeny, "container is running and no health observation shows it hung"
	}
	if health.Health.Observation == "healthy" {
		return DecisionDeny, "container is running and its health probe is healthy"
	}
	return "", ""
}

// rollbackPreconditions：发布回退要求发布记录确认当前版本、故障在最近一次发布之后
// 开始，并且真实流量指标确实被读到（版本回归要有业务信号支撑）。迁移兼容、目标
// 版本已验证、现版本仍等于快照版本由动作准备阶段按实时状态复核。
func rollbackPreconditions(_ string, plan llm.Plan, evidence Evidence) (string, string) {
	changes, ok := evidence.Item("recent_changes")
	if !ok || changes.Status != ItemOK || changes.Release == nil || changes.Release.CurrentID == "" || changes.Object == nil {
		return DecisionDeny, "no release record establishes the current version"
	}
	if plan.Target.Kind != "service" || plan.Target.Name != changes.Object.Name {
		return DecisionDeny, "rollback target is not the service named by the release records"
	}
	if !changes.Release.DeployedBeforeFault {
		return DecisionDeny, "the current release did not happen shortly before the fault; no version regression to roll back"
	}
	metrics, ok := evidence.Item("sub2api_metrics")
	if !ok || metrics.Status != ItemOK || metrics.Business == nil {
		return DecisionEscalate, "business metrics were not fully read; a version regression cannot be confirmed"
	}
	if metrics.Business.Requests == 0 || metrics.Business.Errors == 0 {
		return DecisionDeny, "current business traffic does not show failures attributable to the release"
	}
	return "", ""
}

// quarantinePreconditions：上游隔离要求错误集中在一个明确账号上、同组还有正常账号
// 承接，且 sub2api 自身的临时停调度没有覆盖它；大多数上游都在报错时不盲目切换。
func quarantinePreconditions(_ string, plan llm.Plan, evidence Evidence) (string, string) {
	item, ok := evidence.Item("upstream_accounts")
	if !ok || item.Status != ItemOK || item.Upstream == nil || !item.Upstream.RealtimeEnabled {
		return DecisionDeny, "sub2api account state is not established by current evidence"
	}
	id, err := strconv.ParseInt(plan.Target.Name, 10, 64)
	if plan.Target.Kind != "upstream_account" || err != nil {
		return DecisionDeny, "quarantine target must be an upstream_account id"
	}
	var target *UpstreamAccountFacts
	attributed, failingInGroup, groupSize := 0, 0, 0
	for i, account := range item.Upstream.Accounts {
		attributed += account.Errors
		if account.ID == id {
			target = &item.Upstream.Accounts[i]
		}
	}
	if target == nil {
		return DecisionDeny, fmt.Sprintf("account %d is not in sub2api's account state", id)
	}
	for _, account := range item.Upstream.Accounts {
		if account.GroupID == target.GroupID {
			groupSize++
			if account.Errors > 0 {
				failingInGroup++
			}
		}
	}
	switch {
	case target.TempUnschedulable:
		return DecisionDeny, fmt.Sprintf("sub2api has already temporarily unscheduled account %d", id)
	case target.Errors < 10 || target.Errors*2 < attributed:
		return DecisionDeny, fmt.Sprintf("errors are not concentrated on account %d (%d of %d attributed errors)", id, target.Errors, attributed)
	case groupSize < 2 || failingInGroup*2 > groupSize:
		return DecisionEscalate, fmt.Sprintf("most accounts of group %d are failing; switching upstreams would not help", target.GroupID)
	}
	return "", ""
}
