# OnCall Agent 简历 S 级核心亮点详解

## 总览与证据口径

本文以当前工作区源码为事实基准，面向刚接手项目的同学解释最值得写进简历的四个亮点。编号与主分析文档保持一致：S1 对应 H1，S2 对应 H2，S3 对应 H3，S4 对应 H4。四项均属于 **L3：架构师核心项**；这里的 L3 是简历能力分级，不是工具的 L3 审批安全等级。

当前系统是 Go 模块化单体，MySQL 同时承担业务存储、持久队列和审计存储；部署约束为同一业务库运行一个 server。真实写动作主要限定为 `Sub2APIDown` 对应单个 Sub2API 容器的 `docker_restart`，默认 `dry_run=true`、`auto_execute_l2=false`。不能把这套实现扩大成通用自治运维、分布式工作流平台或生产规模证明。

代码可以证明功能、调用链和异常处理已经实现，不能证明个人承担比例。本文简历句式统一使用“参与核心设计与实现”；实际使用前仍须确认与个人贡献相符。没有个人职责记录时，不改成“主导整体架构”。文中业务背景指当前可运行的告警处置场景，仓库不足以证明客户数量、生产流量、团队覆盖面或商业结果。

本轮进行源码与文档引用核对，没有启动外部服务、调用付费模型或重跑历史验收。[历史验收记录](</Users/zxy/oncall agent/docs/execution-trust-verification.md:1>)提供另一次验证的环境与范围，不能写成“本轮已通过”，也不能由测试数量推导线上可靠性。项目整体入口可先看[当前系统架构](</Users/zxy/oncall agent/docs/current-architecture.md:1>)。

| 排名 | 亮点标题 | 代码事实强度 | 简历价值 | 面试深挖价值 | 适配岗位 | 适配 JD | 推荐写法 |
|---|---|---|---|---|---|---|---|
| S1 / H1 | 审批不可变快照与执行复验 | 强：规则、执行事务及测试齐全 | 高 | 高 | AI 应用架构、后端架构、运维平台 | Agent 安全执行、审批编排、动作治理 | 将动作、目标和验证条件绑定到审批快照，执行前复验内容与当前故障范围 |
| S2 / H2 | 执行、恢复验证、告警恢复三个事实解耦 | 强：独立状态、任务和提交边界齐全 | 高 | 高 | AI 应用架构、SRE 平台、后端架构 | 自动化闭环、状态建模、故障恢复 | 拆分执行结果、恢复验证与告警状态，以持久验证任务驱动结果判断和后续处置 |
| S3 / H3 | MySQL 持久工作流与关键事务一致性 | 强：事务实现、锁等待与回滚测试齐全 | 高 | 高 | 后端架构、平台架构 | 数据一致性、异步任务、可靠性工程 | 在关键成员范围事务中结合父 Incident 锁与 READ COMMITTED，并将关键结果原子提交 |
| S4 / H4 | 证据驱动 Eino ReAct 与只读工具边界 | 强：采集、模型契约、工具适配与治理链齐全 | 高 | 高 | AI 应用架构、Agent 工程、AIOps | Agent 编排、工具治理、模型调用成本控制 | 构建证据驱动诊断链，限制模型只读工具权限，由确定性规则控制变更动作 |

四项的关系是：S4 生成建议，S1 决定何种建议可以执行，S2 判断执行以后发生了什么，S3 保证关键状态和审计在故障中保持一致。它们是同一闭环的四个观察角度，简历正文不必把所有底层细节重复写四遍。

## S1 / H1：审批不可变快照与执行复验

### 1. 这个亮点一句话说人话

人批准的是“对哪个容器做什么、是否演练、怎样检查结果”这一整件事；真正动手前再确认批准内容和现场仍然匹配，避免拿旧审批执行已经变化的操作。

### 2. 业务背景，小白版

使用者是处理告警的值班人员。系统诊断后可能建议重启容器，人员需要先看清对象、动作、演练模式及检查方式，再批准。审批生成与实际执行之间存在时间间隔：容器配置可能变化，原告警可能恢复，也可能归入新的故障成员。仅保存“已批准”三个字，无法说明这次批准是否仍适用于现在。

出错的后果是把批准过的旧计划套到新现场，或者将演练批准升级为真实重启。因此这不是新增、查询审批单的普通 CRUD，而是跨越“诊断完成—人工决策—异步执行”的业务约束。它位于模型建议和 Docker 变更之间，是执行控制面的一部分。

对 5 年及以上岗位，有价值的是识别时间变化、内容身份和状态竞争的组合问题，并把约束放在执行前的权威事务中。代码不足以证明真实生产事故或个人主导程度，简历应写设计和实现能力，不编造“杜绝所有误操作”的结果。

### 3. 核心术语解释

| 术语 | 小白解释 | 在代码中的体现 |
|---|---|---|
| 审批快照 | 将当时批准的完整条件保存下来，后续按这份内容执行 | `ExecutionContext` 保存安全等级、`dry_run` 和验证规格 |
| PlanHash | 对批准内容计算一个摘要，用来检查内容是否还是同一份 | `PlanHash` 对工具、参数、执行上下文计算 SHA-256 |
| 规范化 JSON | 忽略对象字段顺序和空白差异，避免同样内容产生不同摘要 | `CanonicalJSON` 处理 MySQL JSON 回读差异 |
| 可信绑定 | 把批准对象与服务端现在配置的真实目标进行对照 | `ValidateBinding` 检查容器、健康地址、白名单和安全等级 |
| 故障范围 | 这张审批针对的告警成员集合 | `MemberFingerprints`、`ValidateMembers` |
| TTL | 审批可以使用多长时间，过期后失效 | `expires_at`，执行领取事务比较时间 |
| CAS 条件更新 | 只有状态仍符合预期才修改，别人先处理则本次不生效 | `WHERE status='approved'` 更新为 `executing` |
| dry run / 演练 | 保存模拟结果，但不真正调用变更工具 | Executor 根据快照的 `DryRun` 生成 `simulated` |

### 4. 代码入口在哪里

审批内容生成链：

```text
diagnose.Pipeline.Run
  → approval.Policy.Decide
  → approval.Service.Prepare
  → store.CompleteRun（诊断结果与审批一起提交）
  → MySQL approval
```

人工批准入口是 `POST /api/v1/approvals/{id}/approve`，拒绝是同路径的 `/deny`：

```text
api.ApprovalAPI.ServeHTTP
  → approval.Service.Decide
  → store.DecideApproval
  → MySQL：审批决定及审计事件
  → HTTP 返回 ApprovalDTO
```

后台动作不在这个 HTTP 请求里执行：`approval.Executor.RunOnce → store.ClaimApprovalExecution → Executor.execute → tools.Registry.Execute → Docker Engine → store.FinishExecution`。审批 API 入口见[approval.go](</Users/zxy/oncall agent/internal/api/approval.go:41>)；该接口不调用 LLM、向量库、Redis 或消息中间件。LLM 只存在于审批生成之前的诊断链。控制台渠道当前记为 `anonymous`，快照机制并不等于个人身份认证或角色权限系统。

### 5. 核心代码文件清单

| 文件 | 方法 / 对象 | 作用 | 小白理解 | 阅读顺序 |
|---|---|---|---|---|
| [审批 API](</Users/zxy/oncall agent/internal/api/approval.go:41>) | `ServeHTTP`、`decide` | 接收审批 Hash、理由，返回决定 | 先看用户到底提交了什么 | 1 |
| [审批策略](</Users/zxy/oncall agent/internal/approval/policy.go:69>) | `Policy.Decide` | 从服务端配置与当前成员构造快照 | 把“建议”变成“可审批的具体内容” | 2 |
| [执行规则](</Users/zxy/oncall agent/internal/incident/execution.go:24>) | `ExecutionContext`、`PlanHash`、`ValidateBinding`、`ValidateMembers` | 定义内容身份及现场匹配条件 | 这份批准在哪些条件下还有效 | 3 |
| [审批服务](</Users/zxy/oncall agent/internal/approval/service.go:33>) | `Prepare`、`Decide` | 准备审批、提交裁决 | 连接接口、策略和存储 | 4 |
| [执行存储](</Users/zxy/oncall agent/internal/store/execution.go:59>) | `ClaimApprovalExecution` | 锁内复验并领取动作 | 真的动手前最后核对一次 | 5 |
| [执行器](</Users/zxy/oncall agent/internal/approval/executor.go:87>) | `RunOnce`、`execute`、`persist` | 使用领取后的快照调用工具并提交结果 | 动作与记账各做各的事 | 6 |
| [快照测试](</Users/zxy/oncall agent/internal/incident/execution_test.go:12>) | 快照、模式、配置与成员漂移测试 | 固化规则边界 | 从反例理解为什么不能放宽 | 7 |

### 6. 主链路逐步解释

1. **生成可批准的内容。** [Policy.Decide](</Users/zxy/oncall agent/internal/approval/policy.go:69>)先确认工具已注册、非禁止等级，再检查支持的告警类别。验证地址、时间窗、容器和演练模式来自服务端；这些内容不能由模型临时指定。
2. **建立内容身份。** [PlanHash](</Users/zxy/oncall agent/internal/incident/execution.go:91>)规范化工具、参数和执行上下文后计算摘要。这样用户提交的 `plan_hash` 能与数据库当前批准内容对应，JSON 字段顺序变化不会被误认为计划变更。
3. **发布审批与接受决定。** [CompleteRun](</Users/zxy/oncall agent/internal/store/runstep.go:183>)再次核对 Incident 和成员，再与诊断结果一起发布审批；用户通过 API 提交 Hash 和理由，由[审批服务](</Users/zxy/oncall agent/internal/approval/service.go:71>)转交存储裁决。HTTP 成功表示决定已持久化，不表示容器已经重启。
4. **在领取动作的事务里复验。** [ClaimApprovalExecution](</Users/zxy/oncall agent/internal/store/execution.go:59>)锁定父 Incident 与审批，检查 TTL、Hash、配置绑定、Incident 状态和当前成员。条件失效则把审批置为过期并记录原因；通过后才将 `approved` 改为 `executing`。
5. **按快照执行并单独记账。** [Executor.execute](</Users/zxy/oncall agent/internal/approval/executor.go:130>)使用新领取的行再次校验内容；演练只产生模拟结果，真实动作才调用 Registry。已完成领取的 TTL 不在回包后重新裁决，否则可能把已经领取的动作悬空。最终经 `FinishExecution` 持久化，之后的恢复判断交给 S2。

### 7. 数据是怎么流动的

```text
LLM Plan（action、target；只是建议）
  + 服务端 PolicyConfig + 当前告警 ExecutionMember
  → Decision（tool_name、args、execution_context、plan_hash）
  → Approval（incident_id、run_id、expires_at、status）
  → 用户决定（plan_hash、reason）
  → approved
  → 锁内复验 + 条件领取 → executing
  → Registry / Docker 返回 → executed / simulated / failed
```

核心表是 `approval`，关联 `agent_run`、`incident`、`incident_alert`、`last_alert`，审计落 `incident_event`。关键字段是内容摘要、目标、批准时演练模式、成员指纹及过期时间。接口使用 DTO；内部主要直接传递 Go 结构体和存储模型，没有必要给每一次函数传参都包装一个 DTO/BO/DO 层。

这里没有 Query→Chunk→Embedding→Recall 的 RAG 链路，也没有向量数据库。模型输出经过 S4 后成为计划数据，不能直接成为执行授权。

### 8. 为什么这样设计

审批和执行由不同时间发生的操作组成，所以“批准过”不足以覆盖“现在还能执行”。把动作和验证条件存成快照，解决批准对象不清的问题；执行前对照当前配置与告警，解决批准后现场变化的问题；把检查与领取放在同一事务，缩小检查通过但状态已被其他流程改变的竞争窗口。

纯规则放在 `incident`，是为了让策略、存储与执行器复用相同语义；持久化检查留在 store，是因为只有数据库事务能控制这次领取与成员读取的关系。代价是策略范围受到明确限制，配置变化后旧审批可能必须重建，不能随意把所有新动作接上来。

这体现的是把授权内容、时效和执行事实区分清楚的能力。它没有解决外部 Docker 与 MySQL 之间的全局事务，也不承诺在网络断开时推断出动作是否真实完成。

### 9. Trade-off

| 取舍 | 当前选择 | 得到什么 | 代价是什么 |
|---|---|---|---|
| 操作方便 vs 内容确定 | 批准整份快照，变化后失效 | 减少旧批准被用于新目标的风险 | 改配置或扩大故障范围后，需要重新诊断或审批 |
| 并发性能 vs 状态一致性 | 父 Incident 锁内复验并领取 | 同一故障的关键变更有明确顺序 | 同一 Incident 上的竞争会等待，热点吞吐受限 |
| 通用扩展 vs 可证明闭环 | 当前只支持明确绑定的单容器重启与健康验证 | 动作和验收条件可以一一对应 | 新工具不能仅注册名称，还必须补齐范围及验证契约 |
| 自动恢复 vs 避免重复副作用 | 外部动作未知时转人工核查 | 不盲目重放可能已成功的重启 | 无法承诺所有任务全自动恢复，需人工接手 |

### 10. 异常场景和兜底

| 场景 | 当前行为 | 需要理解的边界 |
|---|---|---|
| 重复点击、另一渠道先决定、Hash 不匹配 | 审批裁决冲突，API 返回 409 | 决定不会因重试而覆盖已有决定 |
| 审批过期、工具等级/目标配置变化、成员范围扩大 | 领取事务拒绝执行，并持久化失效原因 | 不能自动把旧审批“升级”为适用于新情况 |
| 旧审批缺少完整执行上下文 | `ParseExecutionContext` / Hash 检查失败 | 历史记录可查看，不补造可执行授权 |
| 实际工具调用失败或结果不确定 | 记录 `failed` 与人工核查问题 | 工具错误不等于目标一定没变 |
| 动作返回了，但 MySQL 提交失败 | Executor 保留结果并只重试持久化 | 不重新调用 Docker；进程退出后内存结果可能丢失 |
| 进程在 `executing` 中断 | 启动恢复记为失败/人工核查，不重放动作 | 当前是单实例恢复约束，不是分布式动作精确一次 |

该亮点不依赖 Redis 或 MQ；没有必要编造它们失败时的降级。LLM 失败发生在建议生成阶段，不会凭空生成可执行批准。没有发现通用动作回滚或跨系统补偿事务的实现，不能写成已实现。

### 11. 这个亮点为什么值得写架构师简历

普通 CRUD 关注一行审批有没有写成功；这里关注一份许可能否在时间、配置和故障变化中保持语义正确，并把这种语义落实到异步执行边界。它能体现领域约束提炼、应用安全边界、状态机与事务协作能力。

最适合 AI 应用架构、Agent 工程和运维平台 JD 中的“安全工具调用、可控执行、人工审批”。也适合后端架构的“异步一致性与异常处理”。对分布式/存储岗位只能证明应用层并发与事务理解；对 AI 性能岗位几乎不构成性能证据。数据库内核、GPU 推理或纯算法岗位不宜把它当核心匹配项。

### 12. 简历表述

| 版本 | 建议表述 |
|---|---|
| 克制专业版 | 参与核心设计与实现审批快照及执行复验机制，将工具参数、演练模式和恢复验证条件绑定到计划摘要，执行前校验配置与故障范围。 |
| 更偏架构版 | 参与核心设计与实现 Agent 变更控制链路，通过不可变审批内容、统一计划摘要和锁内条件领取，约束诊断、审批与异步执行之间的状态变化。 |
| 更偏业务结果版 | 参与核心设计与实现单容器重启审批闭环，使值班人员能够确认目标、演练模式和验证方式，并在现场条件变化时阻止旧审批继续执行。 |
| AI 应用架构师版 | 参与核心设计与实现模型建议到受控动作的转换机制，以服务端策略生成审批快照，结合执行前复验隔离模型推理与真实变更权限。 |
| 平台架构师版 | 参与核心设计与实现审批与异步执行控制面，统一动作内容、目标绑定、过期校验与状态审计，支持受控重启场景的可追踪处理。 |
| 分布式 / 存储架构师版 | 可作为应用事务案例：参与核心设计与实现基于 MySQL 行锁和条件更新的审批领取，保证内容校验、状态变化与审计在同一事务提交。不要包装成存储引擎架构。 |
| AI 性能优化架构师版 | 不建议单独改写为性能成果；本项没有模型算子、吞吐、时延或显存优化证据。 |

### 13. 面试三层追问准备

**第一层：你做了什么？**

“参与把审批从一个布尔决定改为具体内容约束。快照绑定工具、目标、演练模式与验证规格；用户批准时携带 Hash，后台领取时再核对当前配置、故障成员和 TTL。”

**第二层：为什么这么设计？**

“因为批准与执行之间有时间间隔。只在生成审批时校验，无法覆盖后来归入新告警、目标换绑或执行开关变化。Hash 回答内容是否相同，成员与绑定检查回答现在是否还适用，两者不能互相替代。”

**第三层：流量或失败场景扩大怎么办？**

“当前同库单实例，不能直接加多个 Executor。先明确动作是否有外部幂等键或结果查询能力，再设计持有者租约与隔离；仍无法确认的动作保持人工核查。热点 Incident 的锁等待先通过实际指标定位，不把去掉校验当作扩容方案。”这是后续设计方向，不是当前已实现能力。

### 14. 面试官验证问题

- **代码题：** 为什么 `dry_run` 解析使用指针布尔值？应能解释缺字段不能默认为 `false`，否则缺失信息可能变成真实动作许可。
- **内容题：** Hash 覆盖哪些字段，为什么规范化 JSON？应能指出工具、参数、执行上下文，以及 MySQL 回读的字段顺序差异。
- **现场题：** 批准后新增其他告警如何处理？应能走到 `ValidateMembers`，说明当前仍在 firing 的成员必须属于批准范围且符合支持的故障类别。
- **边界题：** 为什么演练审批不能因关闭全局演练开关而变成真实执行？执行依据是审批快照；当前绑定还要阻止在全局演练开启时执行旧真实审批。
- **一致性题：** Docker 成功、数据库失败怎么办？只重试结果提交；崩溃后未知状态转人工，不能答“事务保证全局 exactly-once”。
- **真实性题：** 没有误执行率指标怎么办？回答“源码与场景测试覆盖这些约束，尚无生产误执行率统计”。本项不证明 RAG、模型推理加速或个人身份权限体系。

### 15. 第三层追问最容易暴露的薄弱环节

最容易失分的是把 Hash 说成数字签名或完整权限系统，把数据库事务说成能回滚 Docker，或者认为状态 CAS 已解决所有多实例问题。其次是说不清 TTL 的权威裁决点：领取已提交后，Executor 不能仅因回包跨过过期时间就遗弃 `executing`。还需说明 Hash 无法单独阻止现场变化，真实范围必须从数据库和可信配置复验。

### 16. 新人接手阅读路线

1. 看[当前架构](</Users/zxy/oncall agent/docs/current-architecture.md:1>)和[执行开关](</Users/zxy/oncall agent/config.example.yaml:67>)，先理解单实例、匿名控制台、默认演练和有限动作范围。
2. 看[审批接口](</Users/zxy/oncall agent/internal/api/approval.go:41>)，弄清请求路径、Hash、理由、返回冲突及操作来源，理解“批准成功”与“动作完成”不同。
3. 看[审批服务](</Users/zxy/oncall agent/internal/approval/service.go:33>)和[策略](</Users/zxy/oncall agent/internal/approval/policy.go:69>)，理解快照由谁生成、哪些内容不接受模型决定。
4. 看[执行规则](</Users/zxy/oncall agent/internal/incident/execution.go:47>)，逐个构造缺模式、换容器、加成员的反例，掌握校验含义。
5. 看[存储领取](</Users/zxy/oncall agent/internal/store/execution.go:59>)与[执行器](</Users/zxy/oncall agent/internal/approval/executor.go:130>)，区分数据库内的原子状态变化与外部动作。
6. 看[审批快照迁移](</Users/zxy/oncall agent/migrations/009_approval_execution_context.sql:1>)和[验证任务迁移](</Users/zxy/oncall agent/migrations/010_verify_task.sql:1>)，理解存储字段与旧记录边界；这里无需找 Redis/MQ/向量库配置。
7. 看[快照规则测试](</Users/zxy/oncall agent/internal/incident/execution_test.go:12>)、[执行范围竞争测试](</Users/zxy/oncall agent/internal/store/execution_scope_race_test.go:216>)及历史验收 T3/T4/T8/T9，理解正常流程之外真正要防的失败。

### 17. 风险边界

- **可以说：** 已实现审批内容绑定、服务端目标复验、当前故障范围校验、条件领取和结果审计。
- **不能说：** 所有动作天然幂等、外部操作精确一次、审批具备个人实名授权、系统绝不会误操作。
- **补证后才能说：** 个人主导、生产上线范围、误执行率、审批效率提升幅度、覆盖多少团队。
- **不适合写的夸大版本：** “主导企业级自治运维安全平台，实现任意变更零风险自动执行。”
- **不适配 JD：** 数据库内核、模型底层推理、GPU 性能调优。
- **面试主动降调：** 真实写范围主要是受控单容器重启；当前单实例；控制台匿名；Hash 校验不能替代身份认证或外部系统幂等能力。

## S2 / H2：执行、恢复验证、告警恢复三个事实解耦

### 1. 这个亮点一句话说人话

“重启命令执行了”“服务现在健康了”“告警系统确认恢复了”是三件不同的事。系统分别记录它们，避免看到命令成功就误报故障已恢复，也避免检查结果不明时继续自动重启。

### 2. 业务背景，小白版

值班人员批准重启后，最关心的是业务能否使用。Docker 返回成功只说明动作接口完成，服务启动还需要时间；服务的直接健康检查恢复后，Prometheus 抓取、告警规则计算和 Alertmanager 推送也可能稍后才发生。反过来，告警恢复并不能自动证明某次重启动作成功或与恢复存在因果关系。

把三件事压到同一个状态会造成错误关单、错误成功记忆和重复执行。当前实现将动作结果放在审批记录、观察结果放在验证任务、Incident 恢复交给告警成员当前态。它位于执行器之后，与告警摄入链并行推进。

这不是单纯的“加一列状态”，而是要决定哪个模块有权写什么事实、什么证据足以触发重诊、什么时候必须承认不确定。这样的建模与异常语义适合资深后端或 AI 应用架构简历；仓库仍不能证明生产 MTTR 已下降多少。

### 3. 核心术语解释

| 术语 | 小白解释 | 在代码中的体现 |
|---|---|---|
| 执行结果 | 变更工具这次调用发生了什么 | `approval.status` 的 `executed`、`simulated`、`failed` |
| 恢复验证 | 对已执行动作绑定的目标做健康观察 | `verify_task`、`VerificationWorker`、`Verifier.Check` |
| 告警恢复 | 告警来源报告对应告警已解除 | `last_alert.status=resolved`，再检查 Incident 全部成员 |
| 三态观测 | 健康、不健康、无法观察，不能只用真/假表示 | `healthy`、`unhealthy`、`unavailable` |
| inconclusive / 无法判定 | 证据不足以说成功，也不足以说持续失败 | 验证窗口结束缺乏新鲜不健康观测，或内容/范围失效 |
| deadline / 截止时间 | 这一轮验证允许观察到什么时候 | `verify_task.deadline_at`，重启恢复不延长 |
| 租约 | 记录本次领取时间；超时可回收并重新领取，被回收的旧领取不能提交结果 | `claimed_at`、30 秒 `VerificationLease` |
| 重诊 | 重新分析失败原因并生成新建议，不是直接重复动作 | 验证失败请求新的 `agent_run`，关联 `retry_of` |
| 原子提交 | 一组相关记录一起成功或一起回滚 | 验证终态、事件、问题、记忆效果、重诊准入同事务 |

### 4. 代码入口在哪里

本项有三条独立入口，不能画成“一个 HTTP 请求串行等到故障恢复”：

```text
执行事实：Executor.execute
  → store.FinishExecution
  → MySQL approval + verify_task + incident_event

健康事实：VerificationWorker.RunOnce（后台轮询到期任务）
  → store.ClaimVerificationTask
  → Verifier.Check → 快照绑定的 HTTP /health
  → store.FinalizeVerification → MySQL → 提交后通知

告警事实：POST /webhook/alertmanager
  → api.AlertmanagerWebhook → raw_event
  → ingest.Worker → store.ApplyRawEvent
  → transactionIncidentTx.ResolveIncident
  → MySQL last_alert / incident
```

验证没有新建 Controller 接口，也不调用 LLM、Redis、向量库或 MQ。HTTP /health 是外部只读请求；LLM 只可能在明确失败后获准的新诊断 Run 中再次调用。用户通过控制台/API 读取持久化状态，通知发生在事务确认提交之后。

### 5. 核心代码文件清单

| 文件 | 方法 / 对象 | 作用 | 小白理解 | 阅读顺序 |
|---|---|---|---|---|
| [结果提交](</Users/zxy/oncall agent/internal/store/execution.go:127>) | `FinishExecution` | 保存动作结果，真实执行成功时原子创建验证任务 | 动作做完后把“需要验收”一并记下来 | 1 |
| [验证 Worker](</Users/zxy/oncall agent/internal/diagnose/verification_worker.go:81>) | `RunOnce`、`evaluateVerification` | 领取任务、检查、决定候选结论 | 定时观察，不阻塞后续动作领取 | 2 |
| [单次检查器](</Users/zxy/oncall agent/internal/diagnose/verify.go:24>) | `Verifier.Check` | 有界、只读、按快照访问健康地址 | 只负责测一次，不能自行关单 | 3 |
| [验证存储](</Users/zxy/oncall agent/internal/store/verification.go:102>) | `FinalizeVerification` | 复验范围并一起提交结论及后续影响 | 最终裁决必须以数据库内的新事实为准 | 4 |
| [Incident 恢复](</Users/zxy/oncall agent/internal/store/incident.go:195>) | `ResolveIncident` | 当前成员全部恢复后更新 Incident | “告警解除”有自己的来源 | 5 |
| [验证表](</Users/zxy/oncall agent/migrations/010_verify_task.sql:1>) | `verify_task` | 保存状态、原始窗口与领取信息 | 进程重启以后还能继续观察 | 6 |
| [验证边界测试](</Users/zxy/oncall agent/internal/diagnose/verification_worker_test.go:145>) | 红后绿、迟到健康、无观测、取消等 | 验证判定规则 | 用时间线理解为何不能简单 true/false | 7 |

### 6. 主链路逐步解释

1. **动作结果与待验证事项一起落库。** [FinishExecution](</Users/zxy/oncall agent/internal/store/execution.go:184>)仅对可信真实 `executed` 结果创建 `pending` 验证任务，与结果、事件同事务。`simulated` 不产生恢复任务，避免演练制造成功证据。
2. **独立 Worker 领取到期任务。** [RunOnce](</Users/zxy/oncall agent/internal/diagnose/verification_worker.go:81>)先回收过期领取，再挑选 `next_check_at` 到期的任务。`claimed_at` 记录本次领取身份，旧处理者提交时不能覆盖新领取。
3. **按批准规格做一次健康检查。** [Verifier.Check](</Users/zxy/oncall agent/internal/diagnose/verify.go:24>)复验目标绑定，超时取批准超时与剩余窗口的较小值。它没有存储、动作工具或模型依赖，因此一次健康读取无法偷偷变成重启。
4. **区分健康、不健康和无法观测。** [evaluateVerification](</Users/zxy/oncall agent/internal/diagnose/verification_worker.go:166>)在窗口内见到健康可提出 `passed`；非健康先继续安排观察。到期只有“截止前已持久化且仍新鲜的不健康观测”能支持 `failed`，否则 `inconclusive`。迟到的健康不能把已过窗口的任务判为通过。
5. **锁内再次校验后提交最终事实。** [FinalizeVerification](</Users/zxy/oncall agent/internal/store/verification.go:102>)核对领取身份、审批、配置及成员变化。Worker 先前看见健康，但提交时范围已改变，仍可降为无法判定；只能通知实际提交的状态。
6. **根据可信结论推进后续工作。** 明确失败可申请有预算的重诊、降级命中的错误记忆；通过且满足原始高置信等条件才写回记忆。这些效果与最终结论一起提交。另一路[ResolveIncident](</Users/zxy/oncall agent/internal/store/incident.go:195>)等待全部告警成员恢复，健康验证本身不修改告警事实。

### 7. 数据是怎么流动的

```text
ExecutionCompletion
  → approval.result_json + verify_task(pending, next_check_at, deadline_at)
  → VerifyTask(running, claimed_at)
  → ExecutionContext.Verification + HTTP 观测
  → VerificationCompletion（候选状态 + 候选后续效果）
  → FinalizeVerification 锁内复验
  → VerifyTask 终态 / 下一次时间 + 事件 / 问题 / 可选记忆或新 Run
  → VerificationFinalization（实际提交结论）→ 通知 / 控制台读取
```

另一条数据流是 `Alertmanager payload → raw_event → alert/last_alert → incident_alert 关联 → incident.status`。关键区别是 `approval_id` 绑定某次动作验证，`incident_id` 绑定整个故障，`claimed_at` 标识本次观察处理权，`deadline_at` 标识原始验证窗口。它们不能用最近一条日志或某个步骤的 `finished_at` 替代。

当前内部直接使用 `VerificationCompletion`、`VerifyTask` 等明确职责的结构体，没有额外的消息总线转换层；状态持久化在 MySQL，没有在 Redis、MQ 或向量库保存第二份结论。

### 8. 为什么这样设计

动作接口、直接健康检查、告警平台分别观察不同对象和时间点，天然可能不同步。与其强行把它们压成一个“成功”，不如让每个来源维护自己能证明的事实。这样 UI 能准确表达“动作已执行，仍在观察”“验证通过，告警暂未解除”等真实情况。

验证任务持久化是因为观察需要跨越多次轮询和进程重启；检查器与判定存储分离是因为外部 HTTP 读取不能占用数据库长事务。先采样、再锁内提交也有代价：提交前要重新检查现场，先前的健康结果可能失去适用性。

`inconclusive` 让系统承认信息不足，防止把“连不上健康接口”当作“重启无效”而连续重启。代价是部分流程需要人工判断。架构价值在于定义结果语义和控制后续影响，不能把它夸成因果诊断证明。

### 9. Trade-off

| 取舍 | 当前选择 | 得到什么 | 代价是什么 |
|---|---|---|---|
| 展示简单 vs 事实准确 | 分开执行、验证与告警状态 | 不误把工具成功当作故障恢复 | UI 和使用者需要理解多个状态 |
| 及时反馈 vs 请求持续占用 | 执行完成即记账，验证异步轮询 | 不因等待健康窗口阻塞执行器 | 观察结论有轮询间隔，暂时只显示处理中 |
| 自动闭环率 vs 错误重诊风险 | 引入无法判定，只有明确失败才重诊 | 信息不足时不自动放大动作 | 部分任务要人工核查 |
| 局部成功 vs 后续效果一致 | 结论、记忆与重诊在同事务提交 | 不出现失败状态已写但记忆未降级等半完成 | 任一关键写入失败，整组事务需重新完成 |

### 10. 异常场景和兜底

- **健康地址不可达或超时：** 产生 `unavailable`；窗口未结束可继续观察，截止后没有新鲜不健康证据则无法判定。不能从连接失败直接推导业务持续故障。
- **目标或成员在采样后变化：** 最终事务再次校验，失效则提交 `inconclusive`，不写成功记忆、不按原失败候选继续后续效果。
- **DB 查询/提交失败或进程关闭：** Worker 返回错误，不构造终态；留下的领取可以在租约过期后回收。回收不延长原 `deadline_at`。
- **旧 Worker 晚到：** `FinalizeVerification` 比较 `claimed_at`，旧领取不能完成新一轮任务。已经终结的任务不会因重放再次提交效果。
- **恢复窗口结束但最后观测过旧：** 不能判 `failed`；新鲜程度是失败判断的一部分。
- **动作执行结果未知：** 不进入“假装已执行”的健康验证链；执行恢复路径打开人工核查问题，不重放写动作。
- **明确失败但重诊预算耗尽：** 持久化升级/人工核查事实；并非无限重试。`inconclusive` 本身不触发自动重诊或坏记忆降级。
- **通知失败：** 通知在提交后，失败不能推翻已提交验证事实；本项没有证明所有外部通知可靠送达，不能写 exactly-once 通知。

没有 Redis/MQ 依赖，也没有自动撤销 Docker 重启的补偿。RAG 检索为空不属于这条链路；故障记忆写入是否合格与向量召回无关。

### 11. 这个亮点为什么值得写架构师简历

它体现的是领域事实建模和可靠闭环：哪些证据足够，谁负责裁决，重复/迟到结果怎样处理，未知状态是否能触发下一次副作用。比普通“调用接口后更新成功状态”深入，因为必须处理跨系统时间差、异步恢复及结果与后续任务的一致性。

AI 应用 JD 可对应“Agent 结果验证、反馈闭环、故障记忆治理”；运维平台 JD 可对应“自动化执行可观测性、恢复确认与人工升级”；后端 JD 可对应“持久任务和事务一致性”。存储岗位可以把其中的原子提交作为应用案例，模型底层优化岗位则没有直接对应能力。

### 12. 简历表述

| 版本 | 建议表述 |
|---|---|
| 克制专业版 | 参与核心设计与实现独立恢复验证任务，拆分工具执行、健康观测与告警恢复状态，支持有界检查、租约恢复及明确失败后的受控重诊。 |
| 更偏架构版 | 参与核心设计与实现三类事实分离的故障处置模型，将执行结果、验证裁决与告警生命周期分别持久化，并将验证结论和后续记忆/重诊效果原子提交。 |
| 更偏业务结果版 | 参与核心设计与实现执行后的恢复确认流程，使值班人员能区分“命令已执行”“服务健康已确认”和“告警已解除”，无法确认时保留人工核查。 |
| AI 应用架构师版 | 参与核心设计与实现 Agent 动作验证闭环，以独立健康观测控制成功记忆写回、错误记忆降级和有预算重诊，避免模型建议或工具返回直接充当恢复证据。 |
| 平台架构师版 | 参与核心设计与实现持久化恢复验证 Worker，结合固定截止窗口、领取身份校验和提交后通知，支持观察任务中断后的恢复与审计。 |
| 分布式 / 存储架构师版 | 可作为应用一致性案例：参与核心设计与实现验证终态、审计、记忆变更和重诊准入的 MySQL 原子提交，处理迟到结果与部分写入失败。 |
| AI 性能优化架构师版 | 不宜写成推理性能优化；仅能补充“执行与观察异步解耦”，且不附未经测量的吞吐或时延收益。 |

### 13. 面试三层追问准备

**第一层：你做了什么？**

“参与把执行结果和恢复确认拆开。真实动作结果与验证任务一起落库，独立 Worker 在原始时间窗内检查健康；Incident 是否恢复仍由告警成员状态决定。”

**第二层：为什么这么设计？**

“Docker 成功不代表应用已健康，应用健康也可能早于 Alertmanager resolved。把这些事实混在一起会污染成功记忆和自动重诊判断，所以只能让对应来源证明对应结果；证据不足时用无法判定。”

**第三层：窗口更长、任务更多或进程反复重启怎么办？**

“当前已保留原 deadline 并回收只读任务，旧领取结果被拒绝。更多任务需要先测队列等待和健康检查耗时，再评估并发度与依赖限流；多实例仍需重新设计所有 Worker 的持有权，不只扩大验证线程数。不能通过不断延长窗口把失败隐藏为处理中。”扩容部分是后续方案。

### 14. 面试官验证问题

- 能否用三条时间线说明 Docker 返回、HTTP 健康、Alertmanager resolved 的先后关系？是否明确它们不证明因果归属？
- 为什么 deadline 之后收到 200 也不能判 passed？应能找到 `evaluateVerification` 和最终存储复验两处防线。
- 为什么检查超时不是 failed？应能解释观测缺失与观察到故障的区别。
- 数据库在降级命中的故障记忆后、创建重诊 Run 时失败会怎样？应能指出这些效果在最终事务内一起回滚，而非 Worker 分别提交。
- 旧领取回来时靠什么识别？应能指出 `claimed_at` 相等条件，而不是只检查 `status=running`。
- 你做过真实模型验证吗？保守回答应区分模型效果测试、HTTP 规则测试、真实 MySQL 联测和已有隔离依赖验收；这些不能互相冒充。
- 有 MTTR 或恢复成功率吗？若没有，说明现有证据证明状态/失败边界，不证明生产指标提升。

### 15. 第三层追问最容易暴露的薄弱环节

容易混淆的是 `unhealthy` 这一条观测与 `failed` 这一最终裁决，或把 `passed` 当成自动关闭 Incident。还容易说不清为什么实际通知必须用 store 返回的结论：HTTP 采样到提交之间成员可能变化。对“健康恢复由哪次重启造成”也要保守，当前只验证观察与批准目标的绑定，不提供严格因果识别。

### 16. 新人接手阅读路线

1. 看[当前架构](</Users/zxy/oncall agent/docs/current-architecture.md:1>)与[验证配置](</Users/zxy/oncall agent/config.example.yaml:56>)，先画出执行器、验证 Worker、告警 Worker 三条时间线。
2. 看[告警入口](</Users/zxy/oncall agent/internal/api/alertmanager.go:45>)和[审批入口](</Users/zxy/oncall agent/internal/api/approval.go:41>)，理解“接收告警”和“接受批准”都不等于已恢复。
3. 看[执行结果提交](</Users/zxy/oncall agent/internal/store/execution.go:127>)，找到验证任务诞生的位置，解释为什么演练没有该任务。
4. 看[验证 Worker](</Users/zxy/oncall agent/internal/diagnose/verification_worker.go:81>)、[判断规则](</Users/zxy/oncall agent/internal/diagnose/verification_worker.go:166>)与[检查器](</Users/zxy/oncall agent/internal/diagnose/verify.go:24>)，理解采样、候选结果与最终结果三步。
5. 看[最终提交](</Users/zxy/oncall agent/internal/store/verification.go:102>)、[全部成员恢复](</Users/zxy/oncall agent/internal/store/incident.go:195>)，弄清哪个函数有权写哪一类事实。
6. 看[verify_task 表结构](</Users/zxy/oncall agent/migrations/010_verify_task.sql:1>)，理解 `next_check_at`、`deadline_at`、`claimed_at`、`last_checked_at` 分别解决什么问题。
7. 看[Worker 时间边界测试](</Users/zxy/oncall agent/internal/diagnose/verification_worker_test.go:145>)、[验证效果事务测试](</Users/zxy/oncall agent/internal/store/verification_effects_test.go:38>)及历史验收 T5–T7/T10–T13，再用红后绿、无观测、迟到健康三例复述规则。

### 17. 风险边界

- **可以说：** 执行、验证、告警事实分离，持久验证任务、领取回收、窗口约束、三态观测和后续效果原子提交已实现。
- **不能说：** 工具成功就代表业务恢复，passed 就自动关单，所有恢复都由 Agent 导致，所有通知一定送达。
- **补证后才能说：** 生产 MTTR、恢复成功率、人工升级减少比例、观察队列吞吐。
- **不适合写的夸大版本：** “实现全场景故障自愈，自动根因定位并保证恢复，响应从小时级下降到分钟级。”
- **不适配 JD：** 推理引擎、模型训练、分布式存储内核；本项主要是 AI 应用和可靠性工程。
- **面试主动降调：** 当前按绑定目标做有限健康验证；无法判定不会自动重诊；全流程仍有人工边界与单实例部署约束。

## S3 / H3：MySQL 持久工作流与关键事务一致性

### 1. 这个亮点一句话说人话

系统把待处理任务和处理记录放进数据库。进程中断后能知道做到哪里；重要结果与审计一起保存，核对故障范围时也要确保读到最新已提交现场，避免“拿到锁却仍看见旧数据”。

### 2. 业务背景，小白版

告警摄入、模型诊断、审批执行和恢复验证不是瞬间完成的一次请求。服务器可能在任意阶段退出，数据库可能在写入第二张表时失败，用户也可能在后台工作时发起重诊。值班人员需要看到可解释的任务状态，系统则需要知道哪些阶段可以重新执行、哪些外部动作必须交给人工确认。

当前项目用 MySQL 保存任务、状态和审计，没有引入独立消息中间件或通用工作流引擎。单个 Go 进程中的多个 Worker 与 HTTP 请求仍会并发，所以单实例不意味着没有事务竞争。

本项的架构价值不是“用了数据库队列”，而是识别了四个问题：如何持久化任务、如何一次性提交相关事实、如何在成员变化时读取正确现场、怎样划分可恢复的读取任务与结果未知的写动作。它适合资深后端和平台岗位，但没有足够证据证明分布式高可用、大流量或跨机房能力。

### 3. 核心术语解释

| 术语 | 小白解释 | 在代码中的体现 |
|---|---|---|
| 持久队列 | 待办事项存进数据库，不只放在进程内存 | `raw_event`、`agent_run`、`approval`、`verify_task` |
| Worker | 后台循环取任务并处理的程序 | ingest、diagnose、Executor、VerificationWorker |
| 事务 | 一组数据库操作要么一起保存，要么全部撤回 | `ApplyRawEvent`、`CompleteRun`、`FinishExecution`、`FinalizeVerification` |
| 行锁 | 处理一条关键记录时，让竞争者等当前事务结束 | `lockIncident` 使用 `FOR UPDATE` |
| 父 Incident 锁 | 以故障本身作为多个相关子记录的共同协调点 | 审批发布、动作领取、验证提交及成员写入协调 |
| 读视图 | 数据库为了让查询一致，给普通查询保留的可见数据版本 | REPEATABLE READ 下早期查询可能固定旧视图 |
| READ COMMITTED | 每次普通查询读取该语句开始时已提交的数据 | 关键事务使用 `sql.LevelReadCommitted` |
| CAS | 状态仍符合预期时才更新 | `pending→running`、`approved→executing` 的条件 SQL |
| 幂等提交 | 同一个已完成结果重复提交，不重复产生后续效果 | `FinishExecution` 比较规范化结果并保留已建验证任务 |
| 租约回收 | 旧处理者超时后，将任务恢复为可领取 | 验证任务 `claimed_at` 和过期回收 |
| 恢复边界 | 明确哪些步骤可重做，哪些不能猜测后重做 | 只读诊断/验证可恢复；未知写动作转人工 |

### 4. 代码入口在哪里

持久工作流从实际 HTTP 入口和后台轮询共同推进：

```text
POST /webhook/alertmanager
  → api.AlertmanagerWebhook.serveHTTP
  → store.CreateRawEvent → MySQL → HTTP 202
  → ingest.Worker.process
  → store.ApplyRawEvent（告警、关联、入队一起提交）
  → agent_run

POST /api/v1/incidents/{id}/diagnose
  → api.IncidentAPI.diagnoseIncident
  → store.RequestRun → agent_run → 返回入队结果

diagnose.Worker
  → ClaimAgentRun → Pipeline.Run → CompleteRun
  → approval → Executor → FinishExecution
  → verify_task → VerificationWorker → FinalizeVerification
```

HTTP 入队成功不会同步等待 LLM 或恢复观察完成。诊断调用外部模型及只读证据源，执行调用 Docker，验证调用健康 API；这些外部调用在关键数据库提交事务之外。MySQL 是本项目权威业务库；Redis/PostgreSQL 是被监控目标的依赖，不能写成系统采用多种业务存储或 Redis 消息队列。

### 5. 核心代码文件清单

| 文件 | 方法 / 对象 | 作用 | 小白理解 | 阅读顺序 |
|---|---|---|---|---|
| [告警入口](</Users/zxy/oncall agent/internal/api/alertmanager.go:45>) | `serveHTTP` | 先写原始事件，再响应已接收 | 待办已落库才告诉上游收到了 | 1 |
| [摄入事务](</Users/zxy/oncall agent/internal/store/rawevent.go:114>) | `ApplyRawEvent`、`applyAlert` | 去重、成员变化和后续 hook 处于同事务 | 不留半条告警处理结果 | 2 |
| [Run 队列](</Users/zxy/oncall agent/internal/store/agentrun.go:53>) | `ClaimAgentRun`、`RequeueStaleAgentRuns` | 条件领取、超时恢复及审计 | 任务开始和重新排队都有记录 | 3 |
| [准入与父锁](</Users/zxy/oncall agent/internal/store/runrequest.go:46>) | `RequestRun`、`lockIncident`、`requestRun` | 协调同一 Incident 的新处理周期 | 避免多个入口同时开启冲突工作 | 4 |
| [诊断提交](</Users/zxy/oncall agent/internal/store/runstep.go:75>) | `CompleteRun` | 诊断终态、末批步骤、事件、问题和审批原子提交 | 结论和可执行审批不能脱节 | 5 |
| [动作事务](</Users/zxy/oncall agent/internal/store/execution.go:39>) | `lockApproval`、`ClaimApprovalExecution`、`FinishExecution` | 范围复验、领取、结果与验证入队 | 检查正确现场并完整记账 | 6 |
| [验证事务](</Users/zxy/oncall agent/internal/store/verification.go:102>) | `FinalizeVerification` | 验证裁决与后续效果原子提交 | 最后一步也不允许半成功 | 7 |
| [真实锁等待测试](</Users/zxy/oncall agent/internal/store/execution_scope_race_test.go:44>) | `commitScopeChangeWhileWaiting` 等 | 重现锁前读视图与成员变更竞争 | 证明不是“加个锁就一定读新数据” | 8 |

### 6. 主链路逐步解释

1. **先存待办，再开始业务处理。** [Webhook](</Users/zxy/oncall agent/internal/api/alertmanager.go:45>)把报文写进 `raw_event` 才返回 202。唤醒信号只是加快消费，待办本身已在数据库；进程短暂退出不会仅因内存通知丢失而丢掉这条持久事件。
2. **摄入的相关写入放在一个事务。** [ApplyRawEvent](</Users/zxy/oncall agent/internal/store/rawevent.go:114>)锁定 `pending` 事件，应用告警去重和关联 hook，最后标记 `processed`。数据库失败整体回滚，报文保持待处理；无法解析的报文按错误路径标为失败。
3. **领取状态与审计一起提交。** [ClaimAgentRun](</Users/zxy/oncall agent/internal/store/agentrun.go:53>)用行锁和 `pending→running` 条件更新领取，并在同事务写开始事件。领取成功后再执行耗时诊断，避免拿着数据库事务等待外部模型。
4. **发布结果时统一相关事实。** [CompleteRun](</Users/zxy/oncall agent/internal/store/runstep.go:75>)把 Run 终态、最后一批步骤/事件/问题和可选审批一起提交；[FinishExecution](</Users/zxy/oncall agent/internal/store/execution.go:127>)将动作结果与验证任务一起提交；[FinalizeVerification](</Users/zxy/oncall agent/internal/store/verification.go:102>)将验证结论、记忆与重诊准入一起提交。这是不同阶段的短事务，不是一条覆盖所有外部系统的长事务。
5. **成员范围判断同时依赖父锁与正确读视图。** [lockApproval 注释与实现](</Users/zxy/oncall agent/internal/store/execution.go:39>)说明：为了找到父 Incident，事务会先普通查询审批身份；若采用 REPEATABLE READ，该查询可能先固定旧读视图。即使随后等到了父锁，普通成员查询仍可能读到等待之前的成员。关键事务改用 READ COMMITTED，后续成员查询才能看到等待期间已提交的变化。
6. **写方也必须遵守共同协调点。** [applyAlert](</Users/zxy/oncall agent/internal/store/rawevent.go:200>)在修改已归属告警的当前内容前锁定原 Incident；新增归并也在 Incident 关联事务中协调。只让读方锁父记录、写方任意改成员，不能建立范围一致性。
7. **恢复按副作用类型区分。** 过期诊断可重新入队，验证可回收领取并保持原截止窗口；[RecoverExecutingApprovals](</Users/zxy/oncall agent/internal/store/execution.go:225>)对中断写动作转人工核查。恢复不是把所有 `running/executing` 一律重新执行。

### 7. 数据是怎么流动的

| 阶段 | 输入 | 主要持久化对象 | 原子边界 |
|---|---|---|---|
| 接收 | Alertmanager 报文 | `raw_event` | 报文存储成功才接受 |
| 摄入 | `raw_event`、规范化告警 | `alert`、`last_alert`、`incident`、成员关联、可选 Run、事件 | 整个 `ApplyRawEvent` |
| 诊断 | `agent_run`、Incident 上下文 | Run 状态、步骤、事件、问题、可选审批 | 领取事务；步骤记录事务；最终 `CompleteRun` 各自独立 |
| 执行 | `approval` 快照与当前绑定 | 执行状态、结果、事件、命令历史、可选 `verify_task` | 领取事务与 `FinishExecution` 分开 |
| 验证 | 任务、HTTP 观测、候选效果 | 任务结论、事件、问题、可选记忆、重诊 Run | `FinalizeVerification` |

关联键是 `incident_id`、`run_id`、`approval_id`；并发判断依赖状态和领取/时间字段。`verify_task.approval_id` 为主键，表示一个审批对应一份验证状态，待办索引用于选择到期任务。整个系统没有以 MQ、Redis 或向量库作为这些事实的第二权威来源。

内部主要由 Handler、业务结构体和 store 模型传递数据，分层服务于规则和事务边界，不是形式化的多层 DTO 转换。耗时工具/LLM 输出先形成内存结果，再提交对应短事务；数据库无法回滚已经发生的外部动作。

### 8. 为什么这样设计

MySQL 已经承担本系统业务存储，在当前单实例规模下让它同时保存待办，可以减少独立中间件与双写一致性问题。关键结果与审计同库，也使“业务成功但关键审计没写”的情况能够整体回滚。代价是任务扫描与业务查询共享数据库资源，不能把这一选择外推为所有吞吐规模的最佳方案。

父锁与隔离级别解决不同问题：父锁使同一 Incident 的关键读写有共同顺序；READ COMMITTED 避免在等锁前固定的旧视图被等锁后的成员查询继续使用。可以把它理解为“轮到我处理时，重新看一眼已经提交的现场”，而不是一直看排队前拍的照片。

例如事务 A 先读审批确定父 ID，然后等待事务 B 持有的 Incident 锁；B 添加新成员并提交。A 获锁后，必须看到这个新成员才知道旧审批范围已不够。在 REPEATABLE READ 下，A 的普通成员查询可能仍用先前读视图；这里使用事务级 READ COMMITTED，而不是修改全局数据库设置或给每个查询再套一层自定义事务框架。

需要精确限定：并非所有 store 方法都采用同一锁顺序或同一隔离级别。例如 `ClaimAgentRun` 先锁 Run 再只读查父记录。这里讨论的父锁与新鲜范围约束主要针对准入、审批发布、执行领取和验证最终提交，不能笼统宣称“全系统统一父锁”。

### 9. Trade-off

| 取舍 | 当前选择 | 得到什么 | 代价是什么 |
|---|---|---|---|
| 运维简单 vs 独立队列扩展 | 用 MySQL 持久任务 | 少一套中间件，任务与业务可同事务 | 扫描、积压和热点增加数据库负载，需监控与索引 |
| 一致现场 vs 并发吞吐 | 同一 Incident 关键流程共享父锁 | 成员变化与执行/验证判断可协调 | 同一故障的热点事务会串行等待 |
| 最新已提交事实 vs 事务内重复读不变 | 关键事务使用 READ COMMITTED | 等锁后能读取新提交成员 | 不能再默认普通重复查询始终看到同一版本；仍需明确锁边界 |
| 完整审计 vs 局部可用 | 关键状态和审计同事务 | 不产生缺关键证据的业务成功 | 审计写失败也会阻止这次业务提交 |
| 全自动重试 vs 外部副作用安全 | 未知写动作转人工 | 不盲目执行可能已经发生的变更 | 人工恢复成本增加，不能保证自动完成所有中断工作 |

### 10. 异常场景和兜底

| 失败场景 | 已实现行为 | 未覆盖/不能扩大的结论 |
|---|---|---|
| 摄入事务中途失败 | 回滚告警、关联及处理状态，待下轮处理 | 不代表不合法报文会无限重试 |
| 诊断末批审计或审批发布失败 | `CompleteRun` 整体失败，不能留下已发布审批的半结果 | 完整诊断尝试可以重做，历史步骤可能保留多次尝试；不是逐步骤精确一次 |
| 执行结果提交失败/回包不明 | 只重试同一结果；内容相同的已提交结果被认可 | 不重新执行 Docker；内容冲突被拒绝 |
| 验证结论后续效果写入失败 | 结论、审计、记忆与重诊效果回滚 | 不把失败的事务计为成功恢复 |
| 成员在锁等待期间改变 | 关键事务读取最新已提交范围，再复验 | 仅靠行锁、不控制读视图仍可能读旧成员 |
| 诊断/只读验证中断 | 超时重入队或回收领取 | 现有诊断恢复策略依赖单实例，不能承诺多实例工作流 |
| 写动作执行中断 | 标记失败与人工核查 | 没有外部全局事务或自动动作回滚 |
| 数据库不可用 | 入口或 Worker 报错；不能伪造持久化成功 | 未发现数据库高可用切换、离线落盘队列等实现，不能写成已实现 |

不存在本系统 Redis/MQ 宕机降级链，因为这些不是业务任务存储。外部 LLM/工具失败由对应阶段分类，数据库只保证本地事务原子性，无法证明外部调用是否执行。

### 11. 这个亮点为什么值得写架构师简历

它能体现应用架构师对持久任务、事务粒度、锁协议、隔离级别和恢复语义的整体理解。尤其“身份普通查询先固定读视图，导致等到父锁后仍读旧成员”的问题，比泛泛说“用了行锁保证线程安全”更能区分实际工程经验。

后端/平台 JD 中的“事务一致性、异步任务、并发控制、失败恢复”高度匹配。AI 应用架构 JD 中也有价值：Agent 的任务不是只调用模型，模型之外的执行与审计必须保持一致。分布式/存储 JD 可用它说明 MySQL 应用层实践，但不能由此认定具备分片、复制、一致性协议或存储内核经验。AI 性能 JD 只会把它看成配套工程能力。

### 12. 简历表述

| 版本 | 建议表述 |
|---|---|
| 克制专业版 | 参与核心设计与实现基于 MySQL 的持久任务链，将诊断、审批执行和恢复验证的关键状态与审计原子提交，并按任务副作用划分中断恢复策略。 |
| 更偏架构版 | 参与核心设计与实现单体内的持久工作流，在关键成员范围事务中结合父 Incident 锁与 READ COMMITTED，处理锁等待旧读视图，并统一结果与后续任务的提交边界。 |
| 更偏业务结果版 | 参与核心设计与实现可恢复的告警处置链，使服务中断后能够继续只读诊断与验证，对结果未知的变更保留人工核查，减少半完成状态带来的排障歧义。 |
| AI 应用架构师版 | 参与核心设计与实现 Agent 运行控制面，以 MySQL 管理任务、结果与审计，保障诊断结论、可执行审批及验证反馈在关键事务中一致落库。 |
| 平台架构师版 | 参与核心设计与实现持久任务准入、状态领取和中断恢复，结合条件更新、父故障锁及原子审计，协调 HTTP 请求与后台 Worker 的并发处理。 |
| 分布式 / 存储架构师版 | 参与核心设计与实现 MySQL 应用事务一致性，处理 REPEATABLE READ 下锁前身份查询造成的旧成员视图，通过事务级 READ COMMITTED 与成员写锁约束确保关键范围复验有效。此句仅适用于实际参与该修复的人。 |
| AI 性能优化架构师版 | 不推荐单独写成性能优化；没有任务吞吐或模型时延测量时，只能作为 AI 任务可靠运行的工程基础。 |

### 13. 面试三层追问准备

**第一层：你做了什么？**

“参与将告警、诊断、执行和验证放到 MySQL 持久任务链中。关键状态和审计一起提交，范围判断通过父 Incident 锁协调，必要事务采用 READ COMMITTED，并区分可重做的读取任务和不能盲目重做的写动作。”

**第二层：为什么加锁后还要考虑隔离级别？**

“锁控制谁先处理，读视图控制普通查询能看到哪个版本。先用普通查询找审批的父 ID，可能已经在 REPEATABLE READ 中建立旧视图；等锁后普通查询成员仍旧。这里需要写方持有同一父锁，读方在获锁后用 READ COMMITTED 看见写方已提交的范围，两部分缺一不可。”

**第三层：任务量扩大到多实例如何演进？**

“先量化队列积压、查询耗时和锁等待。若需要多实例，必须审查每类 Worker 的持有者、租约、旧结果提交条件与启动恢复行为；尤其执行器启动会处理遗留 executing，不能直接重叠启动。是否迁出独立 MQ 要由负载和一致性需求决定，当前没有实现这一步，也没有证明精确一次外部动作。”

### 14. 面试官验证问题

- **读视图复现：** 画出两个事务的先后步骤：谁先普通查询、谁持父锁、谁添加成员、谁在获锁后查询。若只能答“隔离级别低一些就好了”，理解仍不充分。
- **写方约束：** 为什么仅在执行端 `FOR UPDATE` 不够？应能找到已关联告警版本变化前的父 Incident 锁，解释成员写入要遵守共同协调点。
- **事务边界：** `CompleteRun` 包含哪些写入，哪些步骤早已独立写过？应能说明只保证最后一批结果原子发布，不声称整个诊断只有一个事务。
- **外部动作：** Docker 成功而 MySQL 回包丢失，为什么只重试结果？应能说清结果内容身份与外部调用不可回滚。
- **恢复差异：** `agent_run` 过期重入队与验证 `claimed_at` 校验相同吗？不同；不能把验证拥有的领取令牌保证推广到所有队列。
- **测量问题：** 声称高吞吐需要哪些证据？至少需要负载、延迟分位数、锁等待、数据库资源与积压曲线。没有这些时只能说已实现事务和恢复机制。
- **岗位辨识：** 能解释本地事务并不等于做过分布式存储、共识协议；Eino Agent 只是上层业务，MySQL 事务不证明模型底层优化。

### 15. 第三层追问最容易暴露的薄弱环节

常见薄弱点是把单实例等同于无并发、把行锁等同于读取最新数据、把所有表的状态迁移都说成同一种租约协议。还要防止把回滚数据库说成回滚外部动作，把已有测试称为生产稳定性证明。若无法解释“读方父锁 + 写方父锁 + 关键查询使用新的已提交视图”各自的职责，就不宜把这项放成最核心的存储能力。

### 16. 新人接手阅读路线

1. 看[当前架构](</Users/zxy/oncall agent/docs/current-architecture.md:1>)与[数据库建表迁移](</Users/zxy/oncall agent/migrations/001_init.sql:1>)，先认清唯一业务库和几个任务表，而非先找 Kafka/Redis。
2. 看[告警入口](</Users/zxy/oncall agent/internal/api/alertmanager.go:45>)和[手工诊断入口](</Users/zxy/oncall agent/internal/api/incident.go:117>)，理解 HTTP 何时表示接受、何时创建新周期。
3. 看[摄入 Worker](</Users/zxy/oncall agent/internal/ingest/worker.go:136>)和[诊断 Worker](</Users/zxy/oncall agent/internal/diagnose/worker.go:88>)，区分轮询、领取、业务执行、失败处理。
4. 看[Run 准入](</Users/zxy/oncall agent/internal/store/runrequest.go:46>)、[完整结果提交](</Users/zxy/oncall agent/internal/store/runstep.go:75>)与[执行事务](</Users/zxy/oncall agent/internal/store/execution.go:39>)，用纸画出每个事务包含哪些表。
5. 看[成员写锁](</Users/zxy/oncall agent/internal/store/rawevent.go:200>)与[验证提交](</Users/zxy/oncall agent/internal/store/verification.go:102>)，理解锁等待期间变化为什么仍必须被看见。
6. 看[队列索引迁移](</Users/zxy/oncall agent/migrations/011_queue_admission_indexes.sql:1>)和[验证表](</Users/zxy/oncall agent/migrations/010_verify_task.sql:1>)，把轮询条件与索引列、领取字段对应起来。
7. 看[锁等待竞争测试](</Users/zxy/oncall agent/internal/store/execution_scope_race_test.go:44>)、[成员锁测试](</Users/zxy/oncall agent/internal/store/member_lock_test.go:13>)、[提交恢复计数测试](</Users/zxy/oncall agent/internal/store/recovery_commit_test.go:13>)及历史验收中的事务说明，掌握复现与验证的证据边界。不要对业务库直接运行故障注入测试。

### 17. 风险边界

- **可以说：** MySQL 持久任务、关键状态与审计原子提交、成员范围关键事务的父锁与 READ COMMITTED、不同副作用的恢复策略。
- **不能说：** 通用分布式工作流引擎、全部队列有统一 fencing、跨数据库事务、外部副作用 exactly-once、开箱即用多实例高可用。
- **补证后才能说：** QPS、队列吞吐、千万级任务、高可用切换时间、锁竞争降低比例、个人独立解决该竞争问题。
- **不适合写的夸大版本：** “主导大规模分布式存储架构，实现百万并发 Agent 工作流零丢失精确一次调度。”
- **不适配 JD：** 存储内核、共识算法、跨机房复制、RDMA 或 GPU 推理框架；本项是应用事务设计。
- **面试主动降调：** 当前同库单实例，诊断恢复与验证领取机制并不完全相同；数据库事务边界不包含 LLM、Docker 或健康 HTTP 请求。

## S4 / H4：证据驱动 Eino ReAct 与只读工具边界

### 1. 这个亮点一句话说人话

系统先收集故障现场，再让模型分析原因和建议。模型可以继续查询只读信息，但不能自己重启服务；是否允许变更，由固定规则、审批和执行程序决定。

### 2. 业务背景，小白版

值班人员面对告警时，通常要同时看告警快照、指标、应用健康和容器日志，才能判断是短暂异常、配置问题还是适合重启的故障。模型如果只有告警标题，容易缺少事实；如果直接获得写工具，则可能在证据不足时执行错误动作。

当前系统将证据收集、模型推理和执行控制分开：预先采集七类证据，Reasoner 使用 Eino ReAct 进行有限步数的只读查询，输出结构化 RCA 和 Plan，随后走 Go 代码中的 Guard、Policy、Approval、Executor、Verify。

这项能力位于诊断控制面的中心，区别于普通 CRUD 或一次聊天接口调用。对 5 年及以上 AI 应用架构岗位，重点是能否把不稳定的模型输出变成可观察、可约束的业务建议，而不是代码里 Agent 名字的数量。当前业务范围与模型评测证据仍有限，不能从链路完整推出根因定位准确率。

### 3. 核心术语解释

| 术语 | 小白解释 | 在代码中的体现 |
|---|---|---|
| Evidence / 证据 | 分析时能引用的现场信息，而不是模型自己猜的事实 | `EvidenceItem` 记录来源、时间、状态、正文和截断标记 |
| Collector / 采集器 | 读取一种证据来源的小模块 | 告警快照、Prometheus、应用、数据库、Redis、Docker 等 |
| RCA / 根因分析 | 对“为什么发生故障”的解释 | `DiagnoseResult.RCA` |
| Plan / 计划 | 建议做什么，包含对象、理由等数据 | `llm.Plan`，自身不触发动作 |
| ReAct | 模型在“思考下一步—调用工具—查看结果”中推进的推理方式 | 本项目调用 `react.NewAgent`，通过 `MaxStep` 控制步数 |
| Tool Calling / 工具调用 | 模型提出工具名和参数，程序执行对应已注册工具 | `registryTool.InvokableRun` |
| L1 只读工具 | 可查询状态、不能修改运行对象的一类工具 | `Registry.ForLLM` 只导出 `L1ReadOnly` |
| Guard / 固定规则 | 对已知错误建议做明确拦截或改写 | 缺目标、配置/镜像/凭据错误时阻止重启建议 |
| Policy / 执行策略 | 按工具等级、可信目标和开关决定后续流程 | `approval.Policy.Decide` |
| 结构化契约 | 模型最终回答必须符合程序可解析的字段约定 | `parseContract` 校验 JSON、RCA 非空及置信度枚举 |
| Token / 模型计费单元 | 模型输入输出所使用的文本单位，用于观察消耗 | Reasoner 的输入/输出用量计数，跨输出修复轮累计 |
| 故障记忆 | 按相同故障指纹复用以前验证过的结论/计划 | `fault_memory` 精确键查找；不是语义向量检索 |
| Prompt 注入 | 外部日志或文本试图伪装成模型应执行的指令 | 预采证据明确标为不可信数据；权限仍靠只读工具与执行控制 |

### 4. 代码入口在哪里

```text
POST /webhook/alertmanager，或 POST /api/v1/incidents/{id}/diagnose
  → 原始告警摄入 / store.RequestRun
  → MySQL agent_run
  → diagnose.Worker → Pipeline.Run
  → EvidenceBuilder → 七类 Collector → Evidence.Render
  → llm.Reasoner.Diagnose → Eino react.NewAgent
      → OpenAI-compatible 模型 API
      → 已导出的 L1 registryTool → Registry → 只读外部源
  → RCA + Plan
  → Guard → Policy → 审批准备 → CompleteRun
  → MySQL 结果与审计 → 控制台 / 通知
```

真实调用入口分别见[告警 Handler](</Users/zxy/oncall agent/internal/api/alertmanager.go:45>)和[手工诊断 Handler](</Users/zxy/oncall agent/internal/api/incident.go:117>)。动态工具注册通过 Registry，而非 MCP 协议网关。当前链路没有 VectorDB、Embedding、向量召回或 rerank；也没有独立 MQ。

`Questioner` 是另一条面向对话的只读 ReAct 实现，见[questioner.go](</Users/zxy/oncall agent/internal/llm/questioner.go:51>)。它与 Reasoner 存在不同用途，不足以证明两个 Agent 自主协作、分工会商或共享计划。因此不能据此写 Multi-Agent 协作，也不能把 Go 显式流程写成 Eino Plan-Execute-Replan 调度器。

### 5. 核心代码文件清单

| 文件 | 方法 / 对象 | 作用 | 小白理解 | 阅读顺序 |
|---|---|---|---|---|
| [诊断流水线](</Users/zxy/oncall agent/internal/diagnose/pipeline.go:107>) | `Pipeline.Run` | 显式连接记忆、证据、推理和控制阶段 | 先知道整个过程谁先谁后 | 1 |
| [采集器装配](</Users/zxy/oncall agent/cmd/server/main.go:121>) | `collectors` | 注册固定顺序的七类采集器 | 看项目实际收集了哪些证据 | 2 |
| [证据构建器](</Users/zxy/oncall agent/internal/diagnose/builder.go:29>) | `LoadTarget`、`BuildForIncident` | 载入故障上下文并发起采集 | 把现场资料准备齐 | 3 |
| [证据模型](</Users/zxy/oncall agent/internal/diagnose/evidence.go:65>) | `BuildEvidence`、`Render`、`finishItem` | 顺序采集、状态保留、预采脱敏截断 | 让模型知道哪些证据缺失或被裁剪 | 4 |
| [Reasoner](</Users/zxy/oncall agent/internal/llm/reasoner.go:94>) | `Diagnose`、`runOnce`、`agentTools` | Eino ReAct 调用、预算、输出解析 | 模型在允许的范围内分析 | 5 |
| [工具注册表](</Users/zxy/oncall agent/internal/tools/registry.go:108>) | `ForLLM`、`ExecuteWithMetadata` | 只读暴露、统一超时与截断 | 决定模型实际能查询什么 | 6 |
| [动态参数入口](</Users/zxy/oncall agent/internal/llm/tool_args.go:19>) | `executeLLMTool` | 规范模型工具参数并调用 Registry | 工具参数要先过程序校验 | 7 |
| [固定规则](</Users/zxy/oncall agent/internal/diagnose/guard.go:43>)与[执行策略](</Users/zxy/oncall agent/internal/approval/policy.go:69>) | `Guard`、`Policy.Decide` | 将建议约束为可处置的动作 | 模型说可以做，不等于系统准许做 | 8 |
| [Reasoner 测试](</Users/zxy/oncall agent/internal/llm/reasoner_test.go:158>) | 输出、步数、工具权限、解析修复 | 固化模型边界 | 看坏输出会被如何处理 | 9 |

### 6. 主链路逐步解释

1. **装配故障上下文，尝试精确记忆。** [Pipeline.Run](</Users/zxy/oncall agent/internal/diagnose/pipeline.go:107>)先读 Incident 与当前告警。非重诊可按故障指纹查记忆，命中时复用 RCA/Plan 并跳过本次证据采集和 LLM；重诊不查记忆，防止反复使用失败结论。命中仍必须走 Guard、Policy 和审批/验证。
2. **顺序采集七类证据。** [装配代码](</Users/zxy/oncall agent/cmd/server/main.go:121>)注册告警快照、PromQL 回放、黄金指标、Sub2API、PostgreSQL、Redis 和 Docker。`BuildEvidence` 逐个调用，不是并行采集。未配置与采集失败分别记录 `missing/error`，单项失败不抹掉其他来源。
3. **形成有界的预采上下文。** [finishItem](</Users/zxy/oncall agent/internal/diagnose/evidence.go:110>)对预采正文做安全文本处理、脱敏和单项截断；[Render](</Users/zxy/oncall agent/internal/diagnose/evidence.go:78>)带上来源、时间、状态并标注不可信数据。这里的“脱敏”有明确范围：指走该预采路径的证据，不包含所有动态工具输出。
4. **在只读工具与步数预算内推理。** [Reasoner.Diagnose](</Users/zxy/oncall agent/internal/llm/reasoner.go:94>)建立 ReAct Agent，只传入 `ForLLM` 暴露的 L1 工具。模式控制步数上限；工具经 Registry 统一超时、截断，并记录输入、输出、错误和时间。
5. **解析结果，限定修复重试。** [parseContract](</Users/zxy/oncall agent/internal/llm/reasoner.go:192>)解析 JSON 并检查非空 RCA、合法置信度；契约解析失败时，Reasoner 带原错误上下文再请求一次，仍失败即返回错误。置信度是模型输出枚举，不是经过标注集校准的概率，也不直接赋予动作权限。
6. **由确定性业务规则决定下一步。** [Guard](</Users/zxy/oncall agent/internal/diagnose/guard.go:43>)拦截缺目标和已知不可重启故障描述；[Policy](</Users/zxy/oncall agent/internal/approval/policy.go:69>)依据注册等级与可信目标建立执行决策。模型 `risk` 字段不能绕过策略，后续审批与执行仍遵守 S1/S2。
7. **持久化可审计结果。** [runReasonStep](</Users/zxy/oncall agent/internal/diagnose/pipeline.go:329>)和[recordToolSteps](</Users/zxy/oncall agent/internal/diagnose/pipeline.go:372>)记录推理和工具过程；最终经 `CompleteRun` 发布结果与审批。审计写入前的脱敏不等于模型收到工具结果之前已经脱敏。

### 7. 数据是怎么流动的

```text
Incident + Members + Alerts
  → Target
  → EvidenceItem[]（来源、采集时间、ok/missing/error、截断标记）
  → Render 文本
  → System Prompt + User Evidence
  → ReAct 模型消息
      ↔ Tool Call（已暴露的工具 + 参数）
      ↔ Tool Result（Registry 超时/截断后的结果）
  → JSON 契约
  → DiagnoseResult（RCA、Confidence、EvidenceRefs、Plan、Steps、Token 用量）
  → Guard 后的 Plan → Policy Decision
  → MySQL agent_run / agent_run_step / incident_event / incident_problem / approval
```

记忆旁路是 `故障指纹 → fault_memory 精确查找 → 已保存 RCA/Plan → Guard/Policy`。这是结果复用路径；没有分块、Embedding、向量召回、rerank，也不能写成 RAG。

必须区分两个数据卫生位置：预采证据在 `finishItem` 进入模型前脱敏；动态调用路径是[参数规范化](</Users/zxy/oncall agent/internal/llm/tool_args.go:19>)→[Registry 超时/截断](</Users/zxy/oncall agent/internal/tools/registry.go:130>)→[Reasoner 工具返回](</Users/zxy/oncall agent/internal/llm/reasoner.go:270>)，未发现所有动态结果在返回模型前统一脱敏的实现。后续[审计摘要脱敏](</Users/zxy/oncall agent/internal/diagnose/pipeline.go:550>)发生得更晚，不能替代前者。

### 8. 为什么这样设计

模型擅长综合多种文本证据，但输出不稳定；代码擅长执行明确规则。因此把“可能原因与建议”交给模型，把“允许什么动作、针对哪个目标、由谁批准、如何验收”交给确定性控制面。只读工具限制降低模型直接改变环境的能力，不能单靠 Prompt 中的“请勿执行危险操作”完成这个目标。

七类预采证据先提供共同基础，模型再按需查询，可以让诊断拥有明确来源与缺失记录；代价是固定采集会消耗时间，当前又采用顺序执行，并未证明优于所有按需采集方案。单项截断控制上下文体积，也可能丢失关键日志，截断标记因此需要保留。

精确故障记忆命中时跳过模型调用，是应用层调用减少机制；它只适用于相同指纹、满足现有记忆有效性条件的情况，不等于语义泛化或模型推理加速。记忆成功写回还依赖验证与高置信、原始诊断等条件，不能只因为模型自己说“high”就保存为成功案例。

### 9. Trade-off

| 取舍 | 当前选择 | 得到什么 | 代价是什么 |
|---|---|---|---|
| Agent 自主性 vs 动作可控性 | 模型只暴露 L1 工具，写动作走代码控制 | 模型不能直接绕过审批重启 | 复杂动作需新增明确契约和控制流程 |
| 证据完整性 vs 上下文成本 | 每项预采正文和工具结果有长度上限 | 输入大小与输出审计更可控 | 长日志可能被裁剪，模型看不到全部信息 |
| 模型探索能力 vs 调用消耗 | light/full 步数预算，解析只修复一次 | 限制无限工具循环和反复格式修复 | 复杂故障可能在预算内得不到充分结论 |
| 顺序可解释性 vs 采集时延 | 七类采集器固定顺序执行 | 流程简单、输出顺序稳定 | 多个慢数据源的等待可能累加，当前没有并行采集收益证明 |
| 调用节省 vs 复用适用范围 | 按精确指纹复用验证过的记忆 | 命中路径可跳过该次 LLM | 不是语义相似就能命中；旧记忆仍可能失效，必须继续安全校验与验证 |

### 10. 异常场景和兜底

| 场景 | 当前实现 | 不应扩大为 |
|---|---|---|
| 单个来源未配置或采集失败 | 保留 missing/error、来源和错误摘要，其他来源继续 | 已采集到完整现场 |
| 模型返回非法 JSON / 空 RCA / 非法置信度 | 契约解析失败，允许一次输出修复，再失败则终止诊断 | 自动修复所有模型幻觉 |
| 模型 API 超时或非解析错误 | 返回错误，业务链不产生可执行结论 | 自动切换所有模型供应商并保证成功 |
| 动态只读工具报错 | 作为 `tool error` 观测文本回给模型，记录失败步骤 | 失败结果被当成可信业务证据或工具已成功 |
| 模型尝试未知/写工具 | 只有 L1 适配器进入模型工具表；未知工具不具备对应调用入口 | 所有应用入口都只有 L1 权限；Executor 有独立写路径 |
| 模型建议缺目标或对配置/凭据问题重启 | Guard 将动作改为 none，必要时升级人工 | Guard 对所有语义错误都有完备检测 |
| 记忆未命中/不可用 | 继续采证与模型路径；重诊主动不查记忆 | RAG 检索兜底或语义相似召回 |
| 关键审计写失败 | 错误阻止关键审批发布或对应结果提交 | 日志永不丢失、所有失败的 token 都完整落库 |
| 动态工具返回敏感日志 | 当前统一入口只提供参数规范化、超时和截断，未覆盖统一脱敏 | 全链路敏感信息零泄露 |

外部证据可以含有误导性指令。预采标注与围栏处理有助于区分数据和指令，但不能证明模型完全免疫 Prompt 注入；安全动作边界最终仍依赖工具暴露和确定性执行链。该项未实现向量检索，Redis 是证据源之一，不是模型缓存；MQ 不属于这条调用链。

### 11. 这个亮点为什么值得写架构师简历

它能证明把模型能力嵌入真实业务控制链的工程设计：有输入证据、输出契约、工具权限、调用预算、错误处理、审计和后续执行边界。面试深度在于能说明模型什么地方可以自主决定，什么地方必须由代码决定，以及控制措施目前覆盖哪里、未覆盖哪里。

适配 AI 应用架构、Agent 工程、AIOps 与后端平台 JD。对“RAG 专家”岗位只能说明当前没有该链路，不能为了贴标签沿用旧简历中的向量知识库；对“Multi-Agent 编排”“MCP 平台”岗位也缺少对应实现证据。性能岗位可以讨论调用次数、步数与上下文预算，不能把它写成模型底层推理、量化或 GPU 优化。

### 12. 简历表述

| 版本 | 建议表述 |
|---|---|
| 克制专业版 | 参与核心设计与实现基于 Eino ReAct 的证据驱动诊断链，组织七类现场证据并输出结构化 RCA/Plan，通过只读工具暴露、步数预算和固定规则约束模型行为。 |
| 更偏架构版 | 参与核心设计与实现推理与执行分离的 Agent 控制面，以 Go 显式流程连接证据、ReAct、Guard、Policy 和审批验证，模型负责建议，确定性代码负责变更约束。 |
| 更偏业务结果版 | 参与核心设计与实现面向 Sub2API 故障的辅助诊断，将告警、指标、依赖健康和容器信息组织为可追踪证据，并为值班人员生成原因分析及受控处置建议。 |
| AI 应用架构师版 | 参与核心设计与实现 Eino ReAct 诊断与只读工具治理，结合结构化输出校验、有限解析重试、步骤审计和验证后故障记忆复用，形成可控的 AI 运维应用链。 |
| 平台架构师版 | 参与核心设计与实现工具注册和调用边界，统一只读工具暴露、超时、输出截断与调用审计，并通过确定性审批链隔离写操作。 |
| 分布式 / 存储架构师版 | 不推荐单独作为存储亮点；可说明“诊断过程与工具审计落 MySQL”，核心事务能力应使用 S3。 |
| AI 性能优化架构师版 | 参与模型调用治理，通过 light/full 步数预算、上下文截断和精确故障记忆复用减少不必要的模型调用；未测量总体成本降幅，不涉及模型底层推理优化。 |

### 13. 面试三层追问准备

**第一层：你做了什么？**

“参与基于 Eino ReAct 的诊断链。系统先采集七类证据，模型在有限步数内使用只读工具并给出结构化根因和计划，后续用固定规则、策略和审批控制动作。”

**第二层：为什么不让 Agent 自己把所有事情做完？**

“模型输出会受输入与采样影响，尤其日志本身是不可信外部文本。工具权限和变更条件不能仅靠 Prompt。我们让模型探索读取信息，动作是否成立则交给服务端规则，实际执行前再核对当前现场。”

**第三层：模型效果、数据量和成本进一步增长怎么办？**

“先补有标注故障样本和基线，评估 RCA 正确性、建议可执行性、错误动作拦截、工具失败、token 与端到端时延。采集耗时高再评估有界并发或按需采集，记忆复用则测命中率与失效比例。动态工具结果脱敏当前有边界，应先补齐再谈全链路数据治理。没有测量前不宣称成本降低多少。”这些是建议的后续工作，不是现有评测平台。

### 14. 面试官验证问题

- **框架使用：** Eino 在项目里负责什么？应能指出 `react.NewAgent` 及工具循环；Go `Pipeline.Run` 承担外部编排，而不是泛称框架自动完成全部工作流。
- **权限边界：** 在哪里决定模型可见工具？应能找到 `Registry.ForLLM` 的 L1 筛选与固定工具适配器；也要承认通用 Registry 支持 Executor 的写工具路径。
- **输出契约：** 解析器具体校验了什么？是 JSON、RCA 与置信度等现有检查，不是完整语义正确性校验，也不证明置信度经过校准。
- **数据卫生：** 预采证据和动态日志工具输出都在进入模型前脱敏了吗？应明确回答“不完全相同”，指出预采 `finishItem` 与晚发生的审计 `sanitizeForStep`。
- **成本真实性：** 记忆命中为何能少调模型？应能找到跳过证据/Reasoner 的条件分支；总体节省多少仍要实际命中率、基线成本与 token 记录。
- **术语辨识：** fault_memory 为什么不是 RAG？当前按精确指纹取结果，没有文档索引、Embedding、向量召回或 rerank；Reasoner 和 Questioner 并存也不是协作式 Multi-Agent。
- **评测真实性：** 准确率怎么得到？若没有标注集与统计，就回答目前有链路、契约和工具边界测试，模型业务效果需要另做评估；不能用历史隔离测试冒充线上准确率。

### 15. 第三层追问最容易暴露的薄弱环节

最容易暴露的问题是只会复述 ReAct，却说不清注册表如何限制实际可调用函数；其次是把 JSON 可解析当成答案正确，把模型自报置信度当成准确率，把精确结果记忆包装成 RAG，把两个独立角色包装成 Multi-Agent 协作。性能方向则要能区分“少调用一次 API”与“同样输入在模型底层算得更快”。数据安全方向必须承认动态工具结果未统一脱敏，不能以晚发生的审计处理替代模型输入治理。

### 16. 新人接手阅读路线

1. 看[当前架构](</Users/zxy/oncall agent/docs/current-architecture.md:1>)与[诊断预算配置](</Users/zxy/oncall agent/config.example.yaml:38>)，理解当前使用的是外部模型 API、显式业务流程和有限写动作。
2. 看[诊断接口](</Users/zxy/oncall agent/internal/api/incident.go:117>)与[诊断 Worker](</Users/zxy/oncall agent/internal/diagnose/worker.go:88>)，理解用户请求如何变成后台 Run。
3. 看[Pipeline.Run](</Users/zxy/oncall agent/internal/diagnose/pipeline.go:107>)，按记忆、采集、推理、Guard、Policy 的顺序画调用链，先不钻每个采集器细节。
4. 看[采集器装配](</Users/zxy/oncall agent/cmd/server/main.go:121>)和[Evidence](</Users/zxy/oncall agent/internal/diagnose/evidence.go:65>)，理解七类来源、顺序采集、缺失状态及预采脱敏范围。
5. 看[Reasoner](</Users/zxy/oncall agent/internal/llm/reasoner.go:94>)、[ForLLM](</Users/zxy/oncall agent/internal/tools/registry.go:108>)和[动态参数处理](</Users/zxy/oncall agent/internal/llm/tool_args.go:19>)，追一次“模型请求工具—程序执行—结果回模型”的完整回路。
6. 看[Guard](</Users/zxy/oncall agent/internal/diagnose/guard.go:43>)、[Policy](</Users/zxy/oncall agent/internal/approval/policy.go:69>)及 MySQL 的 Run/Step 表，理解建议如何成为审批，而不是直接执行；这里无需寻找向量库配置。
7. 看[结构化输出测试](</Users/zxy/oncall agent/internal/llm/reasoner_test.go:158>)、[LLM 无法到达 L2 工具测试](</Users/zxy/oncall agent/internal/llm/reasoner_test.go:329>)、[记忆命中测试](</Users/zxy/oncall agent/internal/diagnose/pipeline_test.go:486>)和[重诊跳过记忆测试](</Users/zxy/oncall agent/internal/diagnose/pipeline_test.go:536>)，学会区分框架调用事实、业务规则验证和未评估的模型效果。

### 17. 风险边界

- **可以说：** Eino ReAct、七类顺序预采证据、预采脱敏截断、只读工具暴露、模型步数预算、输出契约检查、有限解析重试、确定性执行控制和精确故障记忆旁路。
- **不能说：** 当前有向量 RAG、MCP 接入、协作式 Multi-Agent、Eino Plan-Execute-Replan、自主无限重规划、全部模型输入统一脱敏、Prompt 注入彻底解决。
- **补证后才能说：** RCA 准确率、建议采纳率、token/成本降低比例、P95 时延、线上调用规模、个人主导度。
- **不适合写的夸大版本：** “主导多 Agent + RAG + MCP 自治平台，实现高准确率自动排障和模型推理性能倍增。”
- **不适配 JD：** CUDA、量化、算子融合、KV Cache、分布式训练推理、向量检索算法；本项是模型应用层工程。
- **面试主动降调：** 记忆复用是精确结果查找；采集目前顺序执行；Questioner 独立只读；动态工具结果没有统一的入模前脱敏；真实模型业务效果与生产规模仍需专项证据。
