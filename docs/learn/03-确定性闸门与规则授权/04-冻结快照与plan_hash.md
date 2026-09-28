# 冻结快照与 plan_hash

> 所属：[亮点三 · 确定性闸门与规则授权](README.md)

## 一句话

Policy 通过后，把执行需要的一切（规则及其版本、目标 ID、执行前状态、验证标准、补偿动作、过期时间）冻结成一份不可变的**执行快照**（ExecutionContext），再对「动作名 + 参数 + 快照」计算 SHA-256，得到 **plan_hash**。之后的审批、认领、执行、验证，都核对同一个 hash。

## 先弄懂：为什么要「冻结」

从诊断完成到真正执行，中间可能隔了好几分钟（等人批准）。这段时间里：

- 配置可能改了（规则、验证标准、服务地址）；
- 现场可能变了（容器被别人重启、告警恢复、又来了新告警）；
- 数据库里的审批单可能被改了。

如果执行时再去读「当前的配置和状态」，执行的就不是人批准的那一份。**冻结**的意思是：批准的时候，把所有影响决策的内容定格下来；执行的时候，按定格的内容执行，并核对现场是否仍然匹配。

## 快照里冻结了什么

```go
// 摘自 internal/incident/execution.go
// ExecutionContext 是不可变的审批内容，不是运行时配置。
// 决定这个动作的一切都冻结在这里，并且被 PlanHash 覆盖
type ExecutionContext struct {
	Version       int     `json:"version"`        // 快照格式版本，目前只接受 3
	Kind          string  `json:"kind"`           // primary 主动作 / compensation 补偿
	Service       string  `json:"service"`
	Rule          RuleRef `json:"rule"`           // 规则 ID、规则版本、模式、告警列表
	ActionVersion int     `json:"action_version"` // 动作定义的版本号
	Target        Object  `json:"target"`         // {kind, name, id}，id 来自可信响应
	// Revision 是冻结时对象的状态标记，比如容器的启动时间。
	// 执行时如果实时对象的 Revision 变了，就拒绝写入
	Revision     string          `json:"revision"`
	PreState     json.RawMessage `json:"pre_state"`           // 执行前的状态，供执行时比对
	EvidenceRefs []string        `json:"evidence_refs"`       // 支持这个动作的证据
	Members      []string        `json:"member_fingerprints"` // 批准时的故障范围（成员指纹）
	FaultAlert   string          `json:"fault_alert"`         // 用于故障记忆的告警名
	Verification VerificationSpec `json:"verification"`       // 检查项、间隔、窗口、超时、连续次数、观察期
	Compensation *Compensation   `json:"compensation"`        // 预先冻结的补偿动作（可选）
	ExpiresAt    time.Time       `json:"expires_at"`          // 过期时间（默认 30 分钟）
}
```

一份重启动作的快照大致长这样：

```json
{
  "version": 3,
  "kind": "primary",
  "service": "sub2api",
  "rule": {"id": "restart-stopped-process", "version": "2026-09-24.1@a1b2c3d4e5f6", "mode": "manual", "alerts": ["Sub2APIDown"]},
  "action_version": 3,
  "target": {"kind": "container", "name": "sub2api", "id": "3f2a9c…"},
  "revision": "started_at=2026-09-25T08:00:00Z",
  "pre_state": {"status": "exited", "running": false, "started_at": "2026-09-25T08:00:00Z", "restart_count": 0},
  "evidence_refs": ["docker_inspect", "sub2api_health"],
  "member_fingerprints": ["9c1e…"],
  "fault_alert": "Sub2APIDown",
  "verification": {
    "checks": [
      {"kind": "container", "params": {"name": "sub2api", "id": "3f2a9c…", "started_after": "2026-09-25T08:00:00Z"}},
      {"kind": "health", "params": {"base_url": "http://sub2api:8080"}},
      {"kind": "probe", "params": {}}
    ],
    "interval_seconds": 10, "window_seconds": 300, "timeout_seconds": 5,
    "required_passes": 3, "watch_seconds": 1800
  },
  "compensation": null,
  "expires_at": "2026-09-26T10:31:00Z"
}
```

注意 `verification.checks` 是在 **Prepare 阶段由动作自己决定**的。重启动作要求：同一个容器（ID 不变）、在快照时刻之后重启过、`/health` 正常、业务探针通过。验证标准和动作一起被批准，事后不能换一个更宽松的标准来证明「修好了」。

## plan_hash 怎么算

```go
// 摘自 internal/incident/execution.go
func PlanHash(toolName string, args, executionContext []byte) (string, error) {
	if _, err := ParseExecutionContext(executionContext); err != nil {
		return "", err // 计算 hash 的同时，校验快照是否完整合法
	}
	content, _ := json.Marshal(map[string]json.RawMessage{
		"tool_name":         mustJSONString(toolName),
		"args":              args,
		"execution_context": executionContext,
	})
	canonical, err := CanonicalJSON(content) // 规范化：统一字段顺序和空白
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}
```

**为什么要规范化 JSON？** 同样的内容，字段顺序、空白、数字写法不同，算出的 hash 就不同。MySQL 的 JSON 字段在存储时会重新排列 key，读回来的字节和写进去的不一样。不规范化的话，存进去再读出来，hash 就对不上了。

## hash 在哪些环节被核对

| 环节 | 核对什么 | 对不上时 |
|---|---|---|
| 审批草稿生成（`Service.Prepare`） | 重新计算，必须等于 Policy 给出的 hash | 诊断失败，不发布审批 |
| 人工批准（`DecideApproval`） | 请求里带的 hash = 数据库重算的 hash = 存储的 hash | 返回冲突，要求刷新 |
| 飞书卡片回调 | 按钮里带的 hash = 审批单的 hash | 提示「审批计划已变更，请刷新卡片」 |
| 执行器认领（`claimRefusal`） | 在锁内重算 | 审批单置为 expired |
| 执行前（`Executor.operation`） | 再算一次 | 中止，不写入 |
| 验证时 | 再算一次 | 判为 inconclusive |

## 版本绑定：配置改了，旧快照作废

hash 保证快照**本身**没被改。但如果快照没变、配置变了呢？比如规则从 auto 改成 observe，旧的 auto 快照还能执行吗？`ValidateBinding` 负责这件事：

```go
// 摘自 internal/incident/execution.go
// 快照冻结时的服务、规则版本、规则、动作定义，任何一个变了，快照就作废；
// 规则降为 observe 也作废。auto 快照还要求规则现在仍然是 auto
func (c ExecutionContext) ValidateBinding(toolName string, binding ExecutionBinding) error {
	if binding.Service != c.Service {
		return errors.New("incident: approved service configuration has changed")
	}
	rule, ok := binding.Rules[c.Rule.ID]
	if !ok || binding.RulesVersion != c.Rule.Version {
		return errors.New("incident: remediation rules changed after approval; a new decision is required")
	}
	if rule.Mode == ModeObserve || (c.Kind == KindPrimary && c.Rule.Mode == ModeAuto && rule.Mode != ModeAuto) {
		return errors.New("incident: rule no longer authorizes this action")
	}
	if version, ok := binding.Actions[toolName]; !ok || version != c.ActionVersion {
		return errors.New("incident: action is no longer enabled with the approved definition")
	}
	return nil
}
```

规则版本里包含了规则、服务配置、验证标准的内容摘要（见上一篇），所以改任何一项，旧快照都会在认领、执行、验证时被拒绝。**快照格式版本**也一样：只接受版本 3，旧格式的快照不会被按新规则重新解读。

## 故障范围：批准之后又来了新告警怎么办

快照里的 `member_fingerprints` 记录了批准时的故障范围。认领和验证时会重新读取当前成员：

```go
// 摘自 internal/incident/execution.go 的 ValidateMembers（有删减）
firing, err := FiringFingerprints(members, c.Service, c.Rule.Alerts) // 当前的 firing 成员仍须在规则范围内
if executing && len(firing) == 0 {
	return errors.New("incident: approved fault is no longer firing") // 故障已经恢复了，不用再执行
}
for _, fp := range firing {
	if !approved[fp] {
		return errors.New("incident: fault scope changed after approval") // 出现了批准时没有的新成员
	}
}
```

批准的是「对这几条告警代表的故障做重启」。批准之后又来了一条新告警，故障的范围就变了，需要重新决策。

## 常见追问

- **快照为什么存完整 JSON，而不是存规则 ID，执行时再查？** 查的是执行时的配置，不是批准时的配置。快照的意义就是「批准时看到的一切」。
- **hash 是防篡改吗？能防数据库管理员吗？** 它防的是**内容不一致**：审批单被误改、前端显示的是旧版本、按钮被伪造。能同时改数据库内容和 hash 的人不在它的防御范围内，那属于数据库访问控制的问题。
