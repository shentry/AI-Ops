# Action 三步走：以重启为例

> 所属：[亮点四 · 可信执行引擎](README.md)

## 一句话

每个写动作都实现同一个接口，分三步：**Prepare**（冻结现场，决定验证标准）、**Execute**（写之前重读现场，确认没变才写，最多写一次）、**Reconcile**（出错之后读真实状态，判断到底写没写）。每个动作的特殊逻辑都封装在自己内部，执行器和验证器里没有「如果是重启就……」这样的分支。

## Action 接口

```go
// 摘自 internal/tools/action.go
// Action 是一个已注册的写操作。模型只能在计划里点名已启用的动作；
// 执行器按批准过的冻结快照运行它。每个动作自己负责参数检查、快照准备、执行、
// 对账和验证检查项，所以其他环节不需要为每个动作写分支
type Action interface {
	Definition() ActionDefinition
	// Prepare 用可信配置和实时对象校验模型的建议，冻结执行需要的一切
	Prepare(ctx context.Context, req PrepareRequest) (Prepared, error)
	// Execute 重读对象，最多写一次。写之前发现不对（对象变了、部署锁被占）时，
	// 返回 Written=false 的回执；返回 error 表示写失败或结果未知，需要对账
	Execute(ctx context.Context, op Operation) (Receipt, error)
	// Reconcile 读真实状态，判断 op 的写入到底有没有发生
	Reconcile(ctx context.Context, op Operation) (Reconciliation, error)
}
```

三步分别发生在：

| 步骤 | 什么时候 | 谁调用 |
|---|---|---|
| Prepare | 诊断流水线的 Policy 阶段（审批之前） | `Policy.Decide` |
| Execute | 认领成功之后 | `Executor.execute` |
| Reconcile | Execute 报错之后；进程重启对账时 | `Executor.reconciled` |

## 重启动作的完整实现

### 第一步：Prepare，冻结现场

```go
// 摘自 internal/tools/action_restart.go（有删减）
// 重启动作的 revision 是容器的启动时间：快照之后任何人重启过容器，启动时间都会变，
// 执行时就会拒绝，而不是再重启一次
func startedRevision(at time.Time) string { return "started_at=" + at.UTC().Format(time.RFC3339Nano) }

func (a *restartAction) Prepare(ctx context.Context, req PrepareRequest) (Prepared, error) {
	// 目标只能是配置里的那个服务容器
	if req.Target.Kind != "container" || req.Target.Name != a.container {
		return Prepared{}, refuse("restart target must be the service container %s", a.container)
	}
	if err := decodeParams(req.Params, &struct{}{}); err != nil { // 重启没有参数：模型给了参数就拒绝
		return Prepared{}, err
	}
	live, err := a.docker.Inspect(ctx, a.container) // 读实时对象
	if req.Target.ID != "" && live.ID != req.Target.ID {
		// Guard 从证据里确认的 ID，和现在的容器不是同一个（被删除重建了）
		return Prepared{}, refuse("container identity changed since evidence was collected")
	}
	if live.Restarting {
		return Prepared{}, refuse("docker is already restarting the container")
	}
	// 由动作自己决定「怎样算修好」
	checks := []incident.Check{
		check(incident.CheckContainer, ContainerCheck{Name: a.container, ID: live.ID, StartedAfter: live.StartedAt}),
		check(incident.CheckHealth, HealthCheck{BaseURL: a.healthURL}),
	}
	if a.probe {
		checks = append(checks, check(incident.CheckProbe, struct{}{})) // 业务探针：真实调用一次上游
	}
	return Prepared{
		Target:   incident.Object{Kind: "container", Name: a.container, ID: live.ID},
		Args:     mustJSON(map[string]string{"container": a.container}),
		Revision: startedRevision(live.StartedAt),
		PreState: mustJSON(restartState{Status: live.Status, Running: live.Running, StartedAt: live.StartedAt, RestartCount: live.RestartCount}),
		Checks:   checks,
	}, nil
}
```

`refuse(...)` 产生的错误带有 `ErrActionRefused` 标记，Policy 会把它当作「事实不支持」而拒绝授权。

### 第二步：Execute，重读现场，最多写一次

```go
// 摘自 internal/tools/action_restart.go（有删减）
func (a *restartAction) Execute(ctx context.Context, op Operation) (Receipt, error) {
	var before restartState
	json.Unmarshal(op.PreState, &before)
	live, err := a.docker.Inspect(ctx, op.Target.Name) // 写之前再读一次
	if err != nil {
		return Receipt{Detail: "could not read the container before restarting: " + err.Error()}, nil
	}
	switch { // 任何一项和快照不一致，都不写（Written=false）
	case live.ID != op.Target.ID:
		return Receipt{Detail: "container identity changed after approval; not restarted"}, nil
	case startedRevision(live.StartedAt) != op.Revision:
		return Receipt{Detail: "container restarted by someone else after approval; not restarted again"}, nil
	case live.Running != before.Running || live.Status != before.Status:
		return Receipt{Detail: "container process state changed after approval"}, nil
	case !live.Running && (live.RestartPolicy == "always" || live.RestartPolicy == "unless-stopped"):
		return Receipt{Detail: "container is deliberately stopped under an automatic restart policy"}, nil
	case live.Restarting:
		return Receipt{Detail: "docker is already restarting the container; not restarted"}, nil
	}
	if err := a.docker.Restart(ctx, op.Target.ID); err != nil { // 按 ID 重启，不按名字
		return Receipt{Before: op.Revision}, err // 报错：结果未知，交给 Reconcile
	}
	receipt := Receipt{Written: true, Before: op.Revision, Detail: "restarted container " + op.Target.Name}
	if after, err := a.docker.Inspect(ctx, op.Target.Name); err == nil {
		receipt.After = startedRevision(after.StartedAt) // 记录新的启动时间
	}
	return receipt, nil
}
```

注意这里区分了两种「没写」：

- **返回 `Receipt{Written: false}, nil`**：写之前主动放弃。确定没写，审批单记为 aborted。
- **返回 `err`**：调用 Docker 重启时出错。可能写了，也可能没写，需要对账。

### 第三步：Reconcile，读真实状态判断结果

```go
// 摘自 internal/tools/action_restart.go
func (a *restartAction) Reconcile(ctx context.Context, op Operation) (Reconciliation, error) {
	before, err := time.Parse(time.RFC3339Nano, strings.TrimPrefix(op.Revision, "started_at="))
	if err != nil {
		return Reconciliation{Outcome: OutcomeUnknown}, errors.New("restart revision is not a start time")
	}
	live, err := a.docker.Inspect(ctx, op.Target.Name)
	if err != nil || live.ID != op.Target.ID {
		return Reconciliation{Outcome: OutcomeUnknown}, err
	}
	if live.StartedAt.After(before) {
		return Reconciliation{Outcome: OutcomeWritten}, nil // 启动时间变了：重启生效了
	}
	return Reconciliation{Outcome: OutcomeUnknown}, nil // 没变：也不能断定没写（可能稍后才生效）
}
```

**为什么「启动时间没变」返回 unknown，而不是 not_written？** 请求可能还在 Docker 里排队，稍后才生效。此刻读到「没变」，排除不了一个迟到的写入。如果判成 not_written，后续新动作就可能和这次迟到的重启叠加。

## 另外两个动作的要点

三个动作的结构完全相同，区别只在各自的业务细节：

| | 重启 `docker_restart` | 隔离上游 `upstream_quarantine` | 回退发布 `deployment_rollback` |
|---|---|---|---|
| 目标 | 配置里的服务容器 | sub2api 的一个上游账号 | 服务本身 |
| 模型可给的参数 | 无 | 无 | `release_id`（必须取自发布记录证据） |
| revision | 容器启动时间 | 账号当前的「可调度」状态 | 当前发布 ID |
| Prepare 额外检查 | 身份没变、没在重启 | sub2api 没临时停调度它；隔离后同组可用账号不低于规则下限 | 只能回退到**紧邻的上一个**已验证版本；当前版本声明了不兼容的数据库迁移就拒绝；运行中的镜像摘要必须匹配发布记录 |
| Execute 额外检查 | 状态和快照一致 | 账号状态、平台、分组都没变；**写之前再算一次剩余容量** | 拿到和 CI/CD 共用的部署文件锁（`flock`）；拿不到说明有人在部署，放弃 |
| 验证检查项 | 容器已重启 + `/health` + 业务探针 | 账号不可调度 + 分组仍有足够可用账号 + 错误率 | 运行中的镜像摘要为目标版本 + `/health` + 错误率 + 业务探针 |
| 补偿 | 无 | 有：`upstream_restore` 恢复调度，只在账号仍是「我们写的值」时才恢复 | 无 |
| 对账的局限 | 没变 ≠ 没写 | sub2api 的写接口不支持版本条件，回执里如实记录 | 镜像没变不能证明超时的部署不会稍后完成 |

几个值得注意的细节：

- **只回退紧邻的上一个版本**：中间如果有数据库结构变更，跳着回退可能导致代码和数据库不兼容。
- **镜像按摘要（digest）识别**，而不是按 tag：tag 可以被重新指向别的镜像，摘要不会。
- **回退不用 sub2api 自带的 `/system/rollback`**：容器重建会悄悄撤销那种回退。它必须走和 CI/CD 相同的部署入口。
- **补偿只撤销自己写的值**：如果别人已经手动改了账号状态，补偿不会覆盖人的操作。

## 常见追问

- **为什么验证检查项由动作自己决定？** 只有动作知道「怎样算我生效了」：重启要看启动时间变了，回退要看镜像摘要变了，隔离要看账号不可调度了。把这个知识放在动作内部，验证器就是通用的，不需要写任何分支。
- **按 ID 重启和按名字重启有什么区别？** 容器可能被删除后重建，名字一样，但已经是另一个实例。按 ID 操作保证作用在批准时的那个对象上；ID 对不上，Docker 会直接报错，Prepare 或 Execute 也会提前拒绝。
