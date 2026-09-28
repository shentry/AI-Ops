# Guard：用事实检查动作前提

> 所属：[亮点三 · 确定性闸门与规则授权](README.md)

## 一句话

Guard 是一组确定性规则。它只读本次诊断采集到的**结构化事实**，检查模型建议的动作在当前情况下是否成立，并确认目标是谁。任何一条规则不满足，就把动作改成 `none`；没有规则的动作一律拒绝（默认拒绝，fail closed）。

## Guard 的三种结论

```go
// 摘自 internal/diagnose/guard.go
const (
	DecisionAllow    = "allow"    // 计划原样通过（能不能执行，仍然由处置规则和 Policy 决定）
	DecisionDeny     = "deny"     // 计划被拒绝
	DecisionEscalate = "escalate" // 需要人工判断
)
```

注意：**allow 不等于可以执行**。它只表示「这个动作的事实前提成立，目标身份已经确认」。

## 入口：按顺序应用规则，命中即返回

```go
// 摘自 internal/diagnose/guard.go（有删减）
var commonRules = []guardRule{actionWithoutTarget, dependencyUnavailable}

// 每个动作要求的事实。没有规则的动作一律拒绝
var actionRules = map[string][]guardRule{
	tools.ActionDockerRestart:      {nonRestartableFailure, restartTargetIdentity, restartPreconditions},
	tools.ActionDeploymentRollback: {rollbackPreconditions},
	tools.ActionUpstreamQuarantine: {quarantinePreconditions},
}

func Guard(rca string, plan llm.Plan, evidence Evidence) GuardResult {
	action := strings.TrimSpace(plan.Action)
	if action == "" || action == "none" {
		return GuardResult{Decision: DecisionAllow, Plan: plan} // 不做动作，无需检查
	}
	rules, known := actionRules[action]
	if !known {
		return override(plan, DecisionDeny, "no guard rules exist for action "+action)
	}
	for _, rule := range append(append([]guardRule{}, commonRules...), rules...) {
		if decision, reason := rule(rca, plan, evidence); decision != "" {
			return override(plan, decision, reason) // 命中即返回
		}
	}
	return GuardResult{Decision: DecisionAllow, Plan: plan, Target: trustedTarget(plan, evidence)}
}

// 命中规则：动作、参数、目标一律改写成 none
func override(plan llm.Plan, decision, reason string) GuardResult {
	plan.Action, plan.Params = "none", nil
	plan.Target = llm.PlanTarget{Kind: "none", Name: "none"}
	return GuardResult{Decision: decision, Plan: plan, Overridden: true, Reason: reason}
}
```

被改写时，流水线会打开待处理问题 `guard_overridden`，并追加事件 `guard.overridden`，通知里也会写明「这条建议被规则改过，原因是……」。**Guard 之后没有任何环节能把它的结论改回来。**

## 公共规则：所有动作都要过

| 规则 | 条件 | 结论 | 防什么 |
|---|---|---|---|
| `actionWithoutTarget` | 有动作，但目标为空或为 none | deny | 没有明确对象的动作 |
| `dependencyUnavailable` | postgres 或 redis 证据的状态是 error | escalate | 依赖挂了时，重启、回退、隔离都不是有依据的补救 |

## 重启的三条规则

```go
// 摘自 internal/diagnose/guard.go（有删减）

// 1. 诊断自己认定是配置、镜像、凭据类错误时禁止重启：重启解决不了，还会抹掉现场
var nonRestartableKeywords = []string{"配置错误", "config error", "配置缺失",
	"镜像不存在", "imagepullbackoff", "image pull", "no such image", "凭据", "unauthorized", "认证失败"}

func nonRestartableFailure(rca string, plan llm.Plan, _ Evidence) (string, string) {
	text := strings.ToLower(rca + " " + plan.Reason)
	for _, keyword := range nonRestartableKeywords {
		if strings.Contains(text, keyword) {
			return DecisionEscalate, "root cause looks like config/image/credential failure; restart blocked"
		}
	}
	return "", ""
}

// 2. 重启目标必须由本次 docker_inspect 的可信响应确认身份。
//    「证据里出现过这个名字」不够：告警标签和没有身份信息的日志都不能证明对象
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

// 3. 重启只对两种情况成立：已退出且不会自愈；在运行但健康检查失败（卡死）
func restartPreconditions(_ string, _ llm.Plan, evidence Evidence) (string, string) {
	inspect, _ := evidence.Item("docker_inspect")
	c := inspect.Container
	if c.Restarting {
		return DecisionDeny, "docker restart policy is already restarting the container"
	}
	if !c.Running {
		// 重启策略是 always / unless-stopped，容器却停着：只能是有人手动 stop 的，可能在维护
		if c.RestartPolicy == "always" || c.RestartPolicy == "unless-stopped" {
			return DecisionEscalate, "container was stopped although restart policy would restart it"
		}
		return "", "" // 停了、而且不会自己起来：可以重启
	}
	health, ok := evidence.Item("sub2api_health")
	if !ok || health.Health == nil {
		return DecisionDeny, "container is running and no health observation shows it hung"
	}
	if health.Health.Observation == "healthy" {
		return DecisionDeny, "container is running and its health probe is healthy"
	}
	return "", "" // 在跑但健康检查失败：卡死了，可以重启
}
```

把重启的判断画成一棵决策树：

```text
docker_inspect 确认了身份？ ── 否 ──▶ deny
      │ 是
Docker 正在自动重启？ ────── 是 ──▶ deny（交给 Docker）
      │ 否
容器在运行？
  ├─ 否：重启策略是 always / unless-stopped？ ── 是 ──▶ escalate（像是有人手动停的）
  │                                          └ 否 ──▶ 允许
  └─ 是：健康检查 healthy？ ── 是 ──▶ deny（没坏）
                           └ 否 / 读不到 ──▶ unhealthy 则允许；没有观测则 deny
```

## 回退和隔离的规则

| 动作 | 前提（全部满足才放行） |
|---|---|
| `deployment_rollback` 回退发布 | 发布记录能确认当前版本；目标服务名与发布记录一致；当前版本是故障前不久才发布的（否则不是版本回归）；业务指标读到了，而且确实有请求、有错误。业务指标没读全时转人工 |
| `upstream_quarantine` 隔离上游账号 | 读到了 sub2api 的实时账号状态；目标是一个数字账号 ID，且存在于账号列表里；sub2api 自己没有临时停调度它；错误集中在它身上（至少 10 次，且至少占全部归因错误的一半）；同组至少 2 个账号，且报错的账号不超过一半（否则转人工：大部分上游都在出错时，切换也没用） |

## 目标身份从哪里来

Guard 放行时，会输出一个**由证据确认的目标身份**：

```go
// 摘自 internal/diagnose/guard.go
func trustedTarget(plan llm.Plan, evidence Evidence) incident.Object {
	target := incident.Object{Kind: plan.Target.Kind, Name: plan.Target.Name}
	switch plan.Action {
	case tools.ActionDockerRestart:
		item, _ := evidence.Item("docker_inspect")
		target.ID = item.Object.ID // 容器 ID：来自 Docker 的响应
	case tools.ActionDeploymentRollback:
		item, _ := evidence.Item("recent_changes")
		target.ID = item.Release.CurrentID // 当前发布 ID：来自发布记录
	case tools.ActionUpstreamQuarantine:
		target.ID = plan.Target.Name // 账号 ID：已确认存在于 sub2api 的账号状态里
	}
	return target
}
```

这个 ID 会一路传下去：Policy 用它冻结快照，`Action.Prepare` 读实时对象时会比对，执行前还会再比对一次。**名字只用于展示，身份靠 ID。** 容器被删了重建、名字一样但 ID 不同，操作就会被拒绝。

## Guard 的一个已知弱点

`nonRestartableFailure` 是在模型写的 RCA 文字里做关键词匹配。它的方向是安全的（只会转人工，不会放行），但模型换个说法就可能漏掉。真正兜底的是后面两条读结构化事实的规则：即使关键词没拦住，容器在跑且健康、身份对不上，照样过不去。

## 常见追问

- **为什么 Guard 不读日志正文？** 正文是外部文本，可能被注入，可能被截断，也可能来自别的容器。结构化事实来自可信接口的响应，格式固定，适合用代码判断。
- **记忆命中时也要过 Guard 吗？** 要。记忆里的计划是上次的，前提是否还成立，必须按本次采集的事实重新检查。
- **Guard 为什么在 Policy 之前？** Guard 判断「事实上该不该做」，Policy 判断「制度上允不允许做」。事实都不成立，就不用再查制度。另外，Policy 需要用到 Guard 确认过的目标身份。
