# 处置规则：observe、manual、auto

> 所属：[亮点三 · 确定性闸门与规则授权](README.md)

## 一句话

处置规则是**唯一的执行授权来源**。一条规则写明：哪些告警、可以用什么动作、什么模式（只记录 / 人工批准 / 自动执行）、一段时间内最多几次。Policy 负责匹配规则、调用动作的 Prepare 读取实时对象，再决定「拒绝」「降级为人工」还是「通过并冻结快照」。

## 规则长什么样

```yaml
# 摘自 config.example.yaml
remediation:
  rules_version: "2026-09-24.1"      # 人工维护的版本标签，改规则时一起改
  rules:
    - id: restart-stopped-process
      action: docker_restart
      mode: manual                    # 需要人批准
      alerts: [Sub2APIDown]           # 只对这些告警生效
      max_executions: 1               # 窗口内最多执行 1 次
      window_minutes: 60
    - id: rollback-bad-release
      action: deployment_rollback
      mode: observe                   # 只记录「本来会做什么」
      alerts: [Sub2APIBusinessErrors, Sub2APIDown]
      max_executions: 1
      window_minutes: 240
      max_error_ratio: 0.05           # 验证时：错误率不超过 5% 才算恢复
      min_requests: 20                # 验证时：至少 20 个真实请求，否则不算
      verify_window_seconds: 900      # 这条规则的验证窗口：15 分钟
    - id: quarantine-upstream
      action: upstream_quarantine
      mode: observe
      alerts: [Sub2APIUpstreamAccountErrors, Sub2APIBusinessErrors]
      max_executions: 2
      window_minutes: 60
      min_available_accounts: 1       # 隔离后至少还要有 1 个可用账号（执行前会再查一次容量）
      max_error_ratio: 0.05
      min_requests: 20
      verify_window_seconds: 900
      compensate: true                # 验证失败时，自动执行预先冻结的补偿（恢复调度）
  maintenance: []                     # 维护窗口：窗口内不自动执行任何写操作
```

## 三种模式

| 模式 | 效果 | 适合 |
|---|---|---|
| **observe** | 只记录「如果执行，会对谁做什么」，不生成审批单 | 新规则试运行，积累数据 |
| **manual** | 生成 pending 状态的审批单，等人批准 | 默认模式 |
| **auto** | 生成 approved 状态的审批单，批准人记为 `system:rule:<规则 id>` | 验证过、风险可控的场景 |

新规则的推荐路径是 observe → manual → auto，逐步积累信任。

**auto 的启动门槛**（配置不满足，服务直接拒绝启动）：必须配置通知渠道，出了问题要能通知到人；自动重启必须配置业务探针，因为 `/health` 只能证明进程活着，证明不了业务恢复。

## 启动时：规则必须引用已启用的动作

```go
// 摘自 internal/approval/policy.go（有删减）
// Authority 是当前可信配置：服务、规则版本、已启用的动作定义。
// Policy、认领、启动都读它，所以「现在授权了什么」只有一个答案
func NewAuthority(service config.ServiceConfig, remediation config.RemediationConfig, registry *tools.Registry) (*Authority, error) {
	a := &Authority{service: service.Name, remediation: remediation, release: remediation.Release(service) /* … */}
	for _, rule := range remediation.Rules {
		action, ok := registry.Action(rule.Action)
		if !ok || action.Definition().Compensation {
			// 比如配置了回退规则，却没配置发布入口，回退动作就不会注册。
			// 这时拒绝启动，避免一条规则「看起来生效、实际什么也不授权」
			return nil, fmt.Errorf("approval: rule %s names action %q, which is not enabled in this deployment", rule.ID, rule.Action)
		}
	}
	return a, nil
}
```

**规则版本号**是「人工标签 + 内容摘要」：

```go
// 摘自 internal/config/config.go
func (r RemediationConfig) Release(service ServiceConfig) string {
	content, _ := json.Marshal(struct {
		Rules        []RuleConfig
		Service      ServiceConfig
		Verification VerificationConfig
	}{r.Rules, service, r.Verification})
	sum := sha256.Sum256(content)
	return r.RulesVersion + "@" + hex.EncodeToString(sum[:])[:12] // 例如 2026-09-24.1@a1b2c3d4e5f6
}
```

规则、服务配置、验证标准，任何一个改动都会让版本号变化，哪怕忘了改 `rules_version` 也一样。每次启动都会把规则发布写进审计。

## 匹配：规则授权的是「故障 + 动作」

```go
// 摘自 internal/approval/policy.go
// 返回第一条「动作相同」且「告警覆盖所有 firing 成员」的规则
func (a *Authority) match(action string, members []incident.ExecutionMember) (config.RuleConfig, []string, bool) {
	for _, rule := range a.remediation.Rules {
		if rule.Action != action {
			continue
		}
		if firing, err := incident.FiringFingerprints(members, a.service, rule.Alerts); err == nil && len(firing) > 0 {
			return rule, firing, true
		}
	}
	return config.RuleConfig{}, nil, false
}
```

```go
// 摘自 internal/incident/execution.go
// incident 里每一个没恢复的成员都必须：状态是 firing、属于本服务、告警名在规则列表里。
// 有一个不满足，就说明故障范围超出了规则，需要人来看
func FiringFingerprints(members []ExecutionMember, service string, alerts []string) ([]string, error) {
	var fingerprints []string
	for _, member := range members {
		if member.Status == "resolved" {
			continue
		}
		if member.Status != "firing" || !allowed[member.Name] || member.Service != service || member.Fingerprint == "" {
			return nil, errors.New("incident: fault scope is outside the rule; manual review required")
		}
		fingerprints = append(fingerprints, member.Fingerprint)
	}
	return fingerprints, nil
}
```

例子：规则 `restart-stopped-process` 只覆盖 `Sub2APIDown`。

- incident 里只有 `Sub2APIDown` → 匹配。
- incident 里还有一条 `PostgresConnectionsExhausted` → **不匹配**。数据库有问题时，「重启进程」这条规则当初设想的前提已经不成立了。

## 决策流程：拒绝、降级、通过

```go
// 摘自 internal/approval/policy.go 的 Decide（有删减）
func (p *Policy) Decide(ctx context.Context, plan llm.Plan, input PolicyInput) Decision {
	if plan.Action == "none" {
		return Decision{Kind: DecisionNone}
	}
	act, ok := p.registry.Action(plan.Action)            // ① 动作必须已启用
	rule, firing, ok := p.authority.match(plan.Action, input.Members) // ② 必须有规则
	if !ok {
		return denied("no remediation rule authorizes %s for the firing alerts", plan.Action)
	}
	// ③ 动作自己准备：校验参数，读实时对象，冻结执行需要的一切
	prepared, err := act.Prepare(ctx, tools.PrepareRequest{Target: input.Target, Params: plan.Params, Rule: rule})
	if rule.Mode == incident.ModeObserve {
		return Decision{Kind: DecisionObserve /* 记录「本来会做什么」或「本来会被拒绝的原因」 */}
	}
	if err != nil {
		return denied("%v", err)
	}
	// ④ 读当前处置状态
	state, err := p.state.RemediationState(ctx, /* 服务、规则、incident、时间窗 */)
	switch {
	case state.Stopped:
		return denied("emergency stop is active: %s", state.StopReason)
	case state.BusyWith != 0:
		return denied("service %s is busy with approval %d", p.authority.service, state.BusyWith)
	case state.Executions >= rule.MaxExecutions:
		return denied("rule %s budget exhausted", rule.ID)
	}
	// ⑤ auto 规则在这些情况下降级为人工
	mode := rule.Mode
	var demoted []string
	if reason, in := p.authority.remediation.InMaintenance(now); in { demoted = append(demoted, "maintenance window "+reason) }
	if state.Blocked != ""       { demoted = append(demoted, "rule blocked by "+state.Blocked) }
	if state.IncidentActions > 0 { demoted = append(demoted, "a primary action already ran in this incident") }
	if !input.ObservationOK      { demoted = append(demoted, "monitoring data is unavailable") }
	if mode == incident.ModeAuto && len(demoted) > 0 {
		mode = incident.ModeManual
	}
	// ⑥ 冻结快照 + 计算 plan_hash（见下一篇）
	snapshot := incident.ExecutionContext{ /* … */ }
	hash, err := incident.PlanHash(plan.Action, prepared.Args, raw)
	return Decision{Kind: /* DecisionAuto 或 DecisionApproval */, PlanHash: hash /* … */}
}
```

**为什么有的情况拒绝、有的情况降级？** 看的是「这种情况下任何写操作都不对」，还是「需要人来判断」：

| 情况 | 结论 | 原因 |
|---|---|---|
| 急停中 | **拒绝** | 有人明确叫停了所有写操作 |
| 同一服务正在执行别的审批，或正在验证 | **拒绝** | 两个动作叠加，谁也说不清效果来自哪个 |
| 规则预算用完（窗口内次数达上限） | **拒绝** | 反复执行同一个动作，说明它没解决问题 |
| Prepare 拒绝（参数不对、对象身份变了、前提不满足） | **拒绝** | 事实不支持 |
| 在维护窗口内 | auto **降为人工** | 维护期间可能有人在操作，自动执行容易冲突 |
| 规则被阻断（之前执行失败、验证失败或复发） | auto **降为人工** | 这条规则最近出过问题 |
| 这个 incident 已经执行过一个主动作 | auto **降为人工** | 第一个动作没解决，第二个动作需要人判断 |
| 监控数据不可用 | auto **降为人工** | 缺数据既不是健康也不是故障，自动执行没有依据 |

「监控数据不可用」由流水线计算：业务指标证据不是 ok，或者采样超过 2 分钟，或者任何 Prometheus 证据是 error / partial，都算不可用。

所有拒绝和降级都会记录原因：打开待处理问题 `policy_blocked` 或 `policy_degraded`，写入 `policy.evaluated` / `policy.degraded` 事件，通知里也会带上原因。

## 常见追问

- **observe 模式有什么用？** 它让一条新规则在真实故障上「空跑」：记录它本来会对谁做什么、会不会被 Prepare 拒绝。积累一段时间，确认它的判断靠谱，再升级为 manual。
- **规则预算为什么按规则算，而不是按 incident 算？** 按规则算可以限制「同一类动作在一段时间内的总次数」。比如 1 小时内最多重启 1 次：第二次还需要重启，说明问题不是重启能解决的。
- **降级之后会怎样？** 审批单变成 pending，和 manual 规则一样等人批准。通知里会写明「本来是自动执行，因为……需要你确认」。
