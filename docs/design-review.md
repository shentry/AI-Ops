# 设计评审：oncall-agent

审查日期 2026-08-23 · 基线 `c517c6a` + 工作区未提交改动

> **状态（2026-08-24 更新）**：这是一份**带日期的快照**，行号对应当时的基线。
> 此后做过一轮「删除过度设计」的重构，下列结论已发生变化，读本文时请一并参照：
>
> | 本文条目 | 现状 |
> |---|---|
> | **B1** 建议「把已写好的 auth 接进路由」 | **已按相反方向处理**：`internal/auth`、`internal/api/auth_feishu.go`、`Login.tsx` 已整体删除，控制台确认为公开匿名面。见下方补记。 |
> | **B6** `conversation_message` 缺 `status` 索引 | 已不成立：`migrations/006` 的 `idx_conversation_claim (status, claimed_at)` 已覆盖该查询。`approval` 的索引建议仍然成立。 |
> | **B9** `store.go` 2818 行上帝对象 | **部分处理**：已按告警链路阶段拆成 13 个文件（最大 439 行），但 78 个方法仍挂在同一个 `*DB` 上，业务规则也未移出。见下方补记。 |
> | 各处 `file:line` | 大部分已偏移。定位请按符号名搜索，不要直接按行号跳。`internal/store/store.go` 已不存在 —— 文件地图见 `internal/store/doc.go`。 |
>
> A 组（会真的出事的四个）与 B2/B3/B4/B5/B7/B8 未受本次重构影响，结论依然有效。

**范围**：`internal/` 全部包、`cmd/server/main.go`、`migrations/`、`web/src/`、
部署配置（compose / prometheus / alertmanager）。不含前端样式与文档正确性。

**总体判断**：代码质量与文档纪律都在平均线以上 —— 分层收窄接口、事务边界清晰、
脱敏与截断成体系、队列用 CAS 领取、`go vet` 干净、单测全过。下面列的都是
**设计层面**的问题，不是代码风格问题。

按"会不会真的出事"排序，不按模块排序。

---

## A. 会真的出事的四个

### A1. 审批面板看不到"要重启哪个容器" —— 人工闸门是瞎的

| | |
|---|---|
| 位置 | `internal/api/dto.go:157-171`、`web/src/api.ts:97-115`、`web/src/components/ApprovalPanel.tsx:62-68` |
| 类型 | 前后端契约漂移 + 决策信息缺失 |

后端 `ApprovalDTO` 只输出 `tool_name / reason / plan_hash / expires_at`。
前端却按 `target / scope / risk / dry_run` 四个字段渲染，而且全部是
`{approval.target && ...}` 条件渲染 —— 后端不发就**静默不显示**，没有报错。

审批人实际看到的是：

```
请求执行  docker_restart
理由      L3 requires approval
计划指纹  a3f9c1…
过期时间  30 分钟后
```

整条 Guard → Policy → plan_hash 的确定性链条，最后交给一个不知道自己在批什么的人。
`args_json` 里的 `target_name` 被 DTO 刻意去掉了（防泄露），但结果是把决策依据
也一起去掉了 —— 目标名来自告警标签，不是敏感数据。

**修**：`approvalDTO` 增发 `target`（从 `args_json` 取 `target_kind`/`target_name`）、
`risk`（Plan 的等级）、`dry_run`（从 `result_json` 或配置取）。几十行。

---

### A2. Verify 的判据在默认时序下几乎必然假失败

| | |
|---|---|
| 位置 | `internal/diagnose/verify.go:90-110`；`verify_delay_seconds` 默认 30，`config.yaml` 未覆盖 |
| 类型 | 判据与外部系统时序不匹配 |

Verify 唯一判据 = incident 全部成员 `last_alert.status == "resolved"`，执行后
**30 秒**复查一次。但告警恢复的链路延迟是：

```
Prometheus evaluation_interval   5s
+ rule for                      30s ~ 1m     (alerts.yml)
+ Alertmanager group_wait        5s
+ group_interval                30s          (alertmanager.yml)
────────────────────────────────────────
≈ 70s ~ 100s   才可能收到 resolved webhook
```

**即使重启真的修好了，30 秒后不可能已经 resolved。** 后果是连锁的：

1. Verify 判 `failed`
2. → `ScheduleRetry` 建 full 重诊（`retry.go`）
3. → 最多 2 轮后升级人工
4. 如果这次是 memory_hit，还会 `demoteMemoryIfHit` 把一条**正确的**记忆拉黑
   （`approval/executor.go:176-187`）

也就是说：自愈闭环的成功路径在真实时序下走不通，而且会自己毁掉记忆。

**修**（两个方向，建议都做）：
- 判据改成直接复查目标健康 —— `docker inspect` 状态、blackbox probe、
  或对 `sub2api-health` 打一次 PromQL 即时查询。"告警 resolved" 降为辅助信号。
- 延迟对齐 `for + group_interval`，并且改成**多次轮询**（如 30s 间隔轮 4 次）
  而不是一次性判定。

---

### A3. Verify 内联 sleep 阻塞整个执行队列

| | |
|---|---|
| 位置 | `internal/approval/executor.go:161-189` → `internal/diagnose/verify.go:63-72` |
| 类型 | 队头阻塞 + 崩溃窗口误判 |

`executeOne` 里 `VerifyAfterExecution(delay)` 直接 `time.After(delay)` 睡在
executor 唯一的 goroutine 里。**一张审批单卡住全队列 30 秒**，延迟配大就更糟。

同时审批一直停在 `executing` 状态。这个窗口里进程重启 →
`RecoverExecutingApprovals`（`store/execution.go:165-222`）无条件标 `failed` +
开 `manual_check` critical 问题 —— 尽管动作**已经真的执行成功了**。

**修**：Verify 移出执行主循环，做成独立的 `verify_task` 表 + 独立 worker
（和现有 queue 模式一致）。审批状态增加 `executed_pending_verify`，
恢复逻辑就不必把它当失败。

---

### A4. `/rediagnose` 匿名 + 零门控

| | |
|---|---|
| 位置 | `cmd/server/main.go:315-333`、`internal/api/conversation.go:175-193` |
| 类型 | 无界资源消耗 |

同类接口都有门，只有这个没有：

| 入口 | 并发门 | 频率门 |
|---|---|---|
| `Ask` | `hasRunning` 单飞（`conversation/service.go:113-117`）| — |
| 自动重诊 | `CreateRetryAgentRun` 事务内活跃 run 检查 | 链长上限 2 |
| **Web 重诊** | **无** | **无** |

直接调 `CreateAgentRun`。任何能访问 `:18080` 的人可以无限入队 full 诊断，
每条 = 7 个 collector + 最多 8 步 ReAct。LLM 账单和 Prometheus/Docker
负载没有上界。

**修**：复用 `CreateRetryAgentRun` 的活跃 run 事务检查；加每 incident 的
最小重诊间隔。

---

## B. 结构性约束

### B1. 匿名控制台掌握变更执行权，而写好的鉴权是死代码

| | |
|---|---|
| 位置 | `internal/api/dto.go:36-54`、`cmd/server/main.go:182-189` |

`AnonymousConsoleAuthenticator` 的三个方法全部无条件放行：

```go
func (AnonymousConsoleAuthenticator) Authenticate(...) (auth.Actor, store.WebSession, error) {
    return auth.Actor{ID: anonymousConsoleActorID, Name: "匿名用户"}, store.WebSession{}, nil
}
func (AnonymousConsoleAuthenticator) CheckCSRF(*http.Request, store.WebSession) error { return nil }
func (AnonymousConsoleAuthenticator) CheckOperator(auth.Actor) error                  { return nil }
```

`config.yaml` 里 `web.trusted_operator: "oncall"` 已经让 `webEnabled=true`。
**谁能连上端口谁就能 approve L3 重启**，审计里永远是 `anonymous`，事后无法归因。

同时 `internal/auth`（554 行 session/CSRF/OAuth/PKCE）+
`internal/api/auth_feishu.go`（657 行）**已经写完，但 `main.go` 没绑定路由** ——
1200 行死代码。这不是"待接入"，是安全模型和实现反向了。

附带：`/metrics` 也无鉴权（暴露队列深度、审批/执行/记忆命中计数）。

**修**：把 `SessionAuthenticator` 换成 `auth` 包的实现，绑定 `/auth/feishu/*`。
改的是 `main.go` 的组装，不是新功能。

> **补记（2026-08-24）**：实际采取了**相反**的处理。这条本来给了两个出口 ——
> 「接上鉴权」或「承认它是公开面」—— 选择了后者：
>
> - 删除 `internal/auth`、`internal/api/auth_feishu.go`、`Login.tsx`，
>   `web_session` / `web_oauth_state` 两表由 `migrations/008` 标记弃用（未 DROP）；
> - `SessionAuthenticator` / `SessionAuthorizer` / `AnonymousConsoleAuthenticator`
>   三层接口塌缩为 `api.Console` 一个具体类型，`console == nil` 即控制台未启用；
> - `web.session_secret` / `session_ttl_minutes` / `cookie_secure` /
>   `trusted_operator` 四个配置项删除，`operator_allowlist` 保留（仅约束飞书卡片）。
>
> **本条指出的安全事实没有被修复，只是被明确化了**：任何能连上该端口的人
> 仍可批准 L3 重启，审计仍记为 `anonymous`。区别在于现在不再有「1200 行
> 现成鉴权随时能接上」的假象 —— 要鉴权就得真写。README 已写明该端口
> 只应绑回环或完全可信网络。`/metrics` 无鉴权一项同样未修复。

---

### B2. 单实例锁死，且诊断全局串行

**横向扩容不安全。** 所有 worker 都是单 goroutine 顺序消费，没有 leader 选举
也没有实例锁。`RequeueStaleAgentRuns` 用「`started_at` 超过 5 分钟」当租约
（`diagnose/worker.go:18`），但**没有租约续期**：

```
进程 A: claim run#42 → pipeline 跑了 6 分钟（LLM 慢 + 8 步工具）
进程 B: requeueStale 看到 run#42 running 超 5 分钟 → 放回 pending → 重新跑
结果:   重复诊断、重复审批单、重复通知
```

单副本下 claim 的行锁保证了安全，但这个安全性**没有任何机制强制**。

**单副本下的另一面**：诊断是全局串行的。7 个 collector 顺序执行
（`evidence.go:64-71` 的 for 循环，每个 5s 超时）+ ReAct 最多 8 步，
一条 incident 能占住整个诊断队列几分钟。**故障往往同时爆发多个**，
这对值班系统是硬伤。

**修**：
- collector 并发执行（它们互相独立，契约已经是「不返回 error」，天然可并发）
- claim 加 `lease_expires_at` 字段 + 处理中定期续期，替代 wall-clock 猜测
- 诊断 worker 支持有界并发（N 个 goroutine 各自 claim）

---

### B3. 动作空间被写死成"一个动作 + 一个目标"

| | |
|---|---|
| 位置 | `internal/approval/policy.go:206-216`、`internal/llm/reasoner.go:22-35` |

```go
func CanonicalArgs(plan llm.Plan) (json.RawMessage, error) {
    args := map[string]string{
        "target_kind": plan.Target.Kind,
        "target_name": plan.Target.Name,
    }
    ...
}
```

后果三条：

1. **任何需要第二个参数的修复动作都无法表达** —— 扩副本数、回滚到某版本、
   清某个 key、kill 慢查询、带参数重启，全都表达不出来。
2. 当前注册的变更工具**实际只有 `docker_restart` 一个**。整套 L1-L4 分级 +
   白名单 + 限频 + 审批机制，服务于一个动作。
3. 文档写「审批绑定 tool + args + plan_hash」，实际绑定的是 **tool + target**。
   等工具真有参数了，篡改检测就有盲区。

另外 `KnownTargets`（`diagnose/targets.go`）只实现了「告警标签」这一类可信来源，
文档里说的「服务清单 / 运行时查询」没有 —— L2 自动路径只能作用于标签里恰好带
`container`/`pod`/`service` 的场景。

**修**：这是「下一版架构」的题目，不是补丁。Plan 需要升级成
`action + target + params map[string]any`，`CanonicalArgs` 全量参与 hash，
工具声明自己的参数 schema 并由 Policy 校验。

---

### B4. dry_run 下产出假的"已执行"

现行配置 `approval.dry_run` 默认 `true`（`config/config.go:286`）。路径：

```
L2 动作
 → Policy: l2Guardrails 命中 "dry_run enabled" → 降级为人工审批
 → 人点批准
 → Executor: e.dryRun → 不执行，只 log；跳过 Verify；标 executed
 → 前端: status = "executed"，且拿不到 dry_run 字段（见 A1）
```

**演练模式和真实执行在 UI 上不可区分。** 人以为自己批准并完成了一次修复。

位置：`approval/executor.go:139-141, 161, 191`、`approval/policy.go:145-147`。

---

### B5. 记忆键粒度过粗，命中路径不看现场

| | |
|---|---|
| 位置 | `internal/memory/fingerprint.go`、`internal/diagnose/pipeline.go:126-149` |

键 = `md5(group_key + 首条告警名)[:12]` —— 截断到 48 bit。
而 `config.yaml` 里 `correlate.group_by: [labels.service]`，group_key 就是
**一个 service 标签**。同一 service 下不同根因，只要首条告警名相同就共用一条记忆。

更重要的是命中行为：命中即**跳过全部证据采集**，直接复用 1 小时内的 RCA 和 Plan。
Guard/Policy 确实照走，但 Guard 是纯文本关键词规则（`guard.go:36-42`）、
Policy 只看等级和白名单 —— **命中路径上没有任何一环验证「现在的现场还是不是那样」**。

（顺带确认一个**不是**问题的点：`Alerts[0]` 和 `Members[0]` 都按
`linked_at ASC, fingerprint ASC` 排序（`store/incident.go:330, 348`），
pipeline 的写入键和 executor 的查找键是一致的。）

**修**：命中后仍跑一个精简证据集（snapshot + golden metrics）做「现场是否匹配」
的确定性比对，不匹配就降级为完整诊断。记忆键加入更多维度或不截断。

---

### B6. 队列表缺索引

| 表 | 队列查询 | 现有索引 |
|---|---|---|
| `approval` | **每秒** `WHERE status='approved' AND expires_at > ?` | 只有主键（`001_init.sql:97-111`）→ **全表扫** |
| `conversation_message` | `WHERE status='queued'` | `idx_conversation_claim (status, claimed_at)`（`006`）✓ ——本条原判「全表扫」有误 |
| `agent_run` | `WHERE status='pending'` | `idx_status` ✓ |
| `raw_event` | `WHERE status='pending'` | `idx_status` ✓ |

现在数据量小看不出来。

**修**：`ALTER TABLE approval ADD KEY idx_status_expires (status, expires_at);`

`conversation_message` 无需新增索引。注意重构后 worker 的
`drain()` 删掉了「按 incident 全量扫消息」的兜底分支，只走
`NextQueuedConversationMessage`，这条查询已成为唯一路径 —— 索引丢了就是全表扫，
不再有别的路可退。

---

### B7. SSE 是每连接每秒一次 MySQL 查询

`internal/api/stream.go:127-142`，上限 100 连接 → 常态 100 QPS 查
`incident_event`，外加前端 debounce 拉控制室聚合。没有进程内 fan-out
（一个 poller 广播给 N 个订阅者）。

---

### B8. 三处依赖倒置

```
internal/ingest    → internal/diagnose      只为了 Sanitize / ToSafeText
internal/llm       → internal/conversation  适配器层依赖领域层的类型
internal/diagnose  → internal/approval      反向靠 main.go 的 verifyAdapter 补
```

第三处的代价是具体的：`approval.VerifyOutcome` 和 `diagnose.VerifyResult`
必须维护**两份同构结构体**，再在 `main.go:350-369` 手写转换。

三处都指向同一个缺口：**没有独立的 `sanitize`（文本安全原语）包和跨层 DTO 包**。

**修**：`Sanitize`/`ToSafeText`/`Truncate` 提到 `internal/safetext`；
`VerifyResult` 提到 `internal/types` 或 `eventlog` 旁边。两次移动，无逻辑改动。

---

### B9. store.go 是 2818 行的上帝对象，业务规则漏进去了

一个文件、一个 `DB` 类型、100+ 方法、18 张表，同时承担：

- ORM 映射
- 队列语义（claim / CAS / requeue / recover）
- 审计写入
- **业务规则** —— `AssignIncident` 里的时间窗归并和 promote 判定
  （`store/incident.go:64-164`）、`CreateRetryAgentRun` 里的活跃 run 判定
  （`store/agentrun.go:76-177`）

「唯一 DB 边界」这个决策是对的，但把规则也关进去了。

**直接后果：这 2818 行在默认 `go test ./...` 下一行没测。**
`store/helpers_test.go:43-48` 全部 `TEST_MYSQL_DSN` 门控：

```go
func openIntegrationDB(t *testing.T) *DB {
    dsn := os.Getenv("TEST_MYSQL_DSN")
    if dsn == "" {
        t.Skip("TEST_MYSQL_DSN is not set")
    }
    ...
}
```

最需要测的队列 CAS、事务回滚、恢复路径，覆盖率为零 —— 而且没有 CI。

> **补记（2026-08-24）**：本条**部分处理**。
>
> 已做 —— `store.go` 按告警链路阶段拆成 13 个文件（`rawevent` / `incident` /
> `event` / `problem` / `agentrun` / `runstep` / `approval` / `execution` /
> `conversation` / `integration` / `memory` / `llmmodel` / `db`），单文件最大
> 439 行；`store_test.go` 同步拆成 8 个，共享脚手架落在 `helpers_test.go`。
> 拆分是纯搬移：126 个 decl 中 122 个与拆分前逐字节一致，另 4 个的差异恰好等于
> 上一轮重构已知的改动集。文件地图和「为什么不拆子包」写在 `internal/store/doc.go`。
>
> **未做 —— 本条的两个真问题都还在**：
>
> 1. **仍是上帝对象**。78 个方法仍然全挂在同一个 `*DB` 上，任何拿到 `*store.DB`
>    的调用方仍能碰全部 18 张表。拆文件改善的是可读性，不是耦合。
> 2. **业务规则仍在存储层**。`AssignIncident` 的时间窗归并与 promote 判定、
>    `CreateRetryAgentRun` 的活跃 run 判定，只是换了文件，没有移出。
> 3. **默认覆盖率仍是零**，仍然没有 CI。
>
> 真正拆开需要先回答一个这次没有回答的问题：**跨域事务怎么办**。
> `FinishApprovalExecution` 在一笔事务里写 approval + incident_event +
> incident_problem，`CompleteRun` 在一笔事务里写 agent_run + agent_run_step +
> incident_event + incident_problem。拆子包就得导出 `*gorm.DB` 事务句柄，
> gorm 随之泄漏出 store，「唯一 DB 边界」当场失效 —— 换来的是更差的设计。
> 可行的方向是先把业务规则上提到领域层（让 store 只剩 ORM + 队列原语），
> 而不是横着切表。这仍是「下一版架构」的题目。

---

### B10. 审计写入是 best-effort，但审计声称可回放

`internal/diagnose/pipeline.go:452-454`：

```go
func (p *Pipeline) appendStepRecord(ctx context.Context, record store.RunStepRecord) {
    _ = p.db.AppendRunStepRecord(ctx, record)   // 丢了不影响主链
}
```

同时 `agent_run_step` 只有 `KEY idx_run(run_id, seq)`，**无唯一约束** ——
被 requeue 的 run 重跑会写出重复 seq。阶段号（主链 1-6 / 工具 30-89 / verify 90）
硬编码在代码常量里。

---

## C. 工程与交付

| 问题 | 事实 |
|---|---|
| **无 CI / 无 Makefile / 无 Dockerfile** | 一个值班自愈系统自己没有可复现的部署产物和重启保护；`oncall-agent` 不在 compose 里、裸跑在宿主机 |
| **migrations 手工执行、无版本表** | 7 个 SQL 文件，到 `007` 已有 `ALTER TABLE`，没人能确定某环境跑到第几个 |
| **`web/dist` 构建产物提交进 git** | `.gitignore` 专门开了 `!web/dist/`，靠 `go:embed` 嵌入。前端改完忘记 rebuild+commit → 二进制里是旧界面，没有任何检查会发现 |
| **Prometheus 不抓自己的 `/metrics`** | `prometheus.yml` 只有 node-exporter / blackbox / sub2api-* job |
| **metrics 自研无标签** | 只有 counter/gauge（`metrics/metrics.go`），没有标签和直方图 → 「诊断耗时分布」「按 incident 的失败率」结构上无法表达 |
| **Docker socket 无隔离** | 进程直接持有 unix socket ≈ 宿主机 root。应用层白名单只约束 `docker_restart` 这一个注册工具；进程一旦被 RCE，白名单形同虚设（无 socket-proxy / 只读挂载 / 降权用户） |
| **无外键、无 `raw_event_id` 血缘** | `current-architecture.md` §9 已记录 |

---

## D. 建议的动手顺序

| # | 做什么 | 为什么排这里 | 大概量级 |
|---|---|---|---|
| 1 | **A1** 审批 DTO 补 target/risk/dry_run | 直接决定人工闸门有没有意义 | 几十行 |
| 2 | **A2 + A3** Verify 判据改直接健康检查 + 移出 executor 主循环 | 决定自愈闭环能不能闭上 | 一个新 worker + 判据重写 |
| 3 | ~~**B1** 把已写好的 auth 接进路由~~ | 已按相反方向处理：删除该 1200 行，控制台确认为公开面（见 B1 补记）。若要鉴权需重写 | — |
| 4 | **A4 + B6** 重诊门控 + 补 `approval` 索引 | 便宜、立刻见效 | 一个 SQL + 一处复用 |
| 5 | **B4** dry_run 在 UI 上可区分 | 跟 A1 同一处改动 | 顺带 |
| 6 | **B2** collector 并发 + 租约续期 | 想上生产必须做 | 中等 |
| 7 | **B8 + B9** 提出 safetext 包、~~store 按域拆文件~~、业务规则移出 store | 拆文件已做（2026-08-24）；剩下的两件才是让队列逻辑能脱离真 MySQL 测的前提，然后建 CI | 大 |
| 8 | **B3 + B5** 动作模型升级、记忆键与命中校验 | 「下一版架构」的题目，不是补丁 | 大 |

---

## 附：已确认**不是**问题的点

审查过程中排除的几个可疑点，记下来避免重复排查：

- **记忆键在写入侧和读取侧不一致** —— 不成立。`ListIncidentAlerts` 和
  `ListIncidentMembers` 都是 `ORDER BY incident_alert.linked_at ASC, fingerprint ASC`
  （`store/incident.go:330, 348`），`Alerts[0].Name == Members[0].Name`。
- **诊断 worker 自己 requeue 自己正在跑的 run** —— 单副本下不成立。
  `requeueStale` 只在 `drain` 返回（队列见底）后调用，与 `pipeline.Run`
  在同一 goroutine 串行，不会并发。多副本下才成立（见 B2）。
- **executor `drain` 的 `continue` 无限自旋** —— 不成立。claim 失败时
  `ClaimApprovalExecution` 已把状态推进到 `executing`，
  `NextApprovedApproval` 不会再返回同一行。
- **`plan.confidence` 与顶层 `confidence` 混用导致记忆永不提交** —— 不成立。
  prompt（`llm/prompts.go`）同时要求两个字段，`maybeCommitMemory` 读的是
  持久化在 `plan_json` 里的 `plan.confidence`，两侧一致。

