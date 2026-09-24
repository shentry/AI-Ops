# AI-Opus 系统优化专项：代码事实、简历边界与后续设计

本文基于 2026-09-14 当前工作区源码，只讨论 AI OnCall 应用的工程优化。用途是指导后续迭代、选择简历亮点和准备面试。没有运行外部 MySQL、真实 LLM、生产服务或性能压测；文中收益是机制解释或待验证目标，不是已经取得的线上数据。当前工作区含未提交变更，因此引用代表本次审阅时的源码，而非某个已发布版本。

当前最值得写的是：**SQL 持久队列隔离慢任务、诊断与审批的事务一致性、受控执行与独立恢复验证、带验证反馈的故障记忆，以及工具和上下文的有界治理。** 最值得先补的是：**离线效果评测与版本归档、应用性能基线、明确存在的读取放大。**

## 1. 判断口径与系统边界

### 1.1 三类证据如何使用

| 分类 | 本文简称 | 判定标准 | 简历使用方式 |
|---|---|---|---|
| 【当前代码已实现，可写入简历】 | 已实现 | 运行调用链有明确实现，能定位入口、处理和结果 | 写机制及解决的问题；无数据时不写提升比例 |
| 【当前代码有迹象，只能克制表达】 | 克制表达 | 存在局部机制，但缺少完整闭环或测量证据 | 写“复用客户端”“设置大小上限”等局部事实 |
| 【当前未实现，仅作为后续优化方案】 | 可扩展设计 | 当前运行链路未找到完整实现，或当前行为明确与目标不同 | 仅放在后续方案；实现并验证后才能迁入工作成果 |

“强”指能直接对应执行代码；“中”指存在局部基础；“未见”指在本次源码检索和关键调用链中未见对应实现，不能据此推断运行环境中绝不存在外部能力。S / A / B 表示本项目的简历价值或补齐优先级，不代表企业真实职级评定。

### 1.2 不能混淆的技术概念

- 当前是 Go 模块化单体，MySQL 同时保存业务状态、队列和审计；不是 Kafka/RabbitMQ 消息架构。后台 worker 独立运行不等于同一条证据链路已经并行。
- `fault_memory` 按故障指纹等值查找并复用 RCA/Plan。它不是向量检索，也不能写成 RAG、embedding、rerank 或文档知识库。
- Eino ReAct 承担模型推理与只读工具调用；外层 `Pipeline`、Policy、Approval、Executor、Verify 是代码控制的业务流程。当前没有通用 DAG/DSL 工作流引擎的证据。
- 工具分级、变更范围校验、审批快照和调用审计已经存在。不能把“Tool 权限与审计”整项列为从零建设。
- `verify_task` 已有到期调度、领取、恢复与终态事务。不能把“Workflow 状态与恢复”整项列为未实现；缺的是进一步调度优化和特定阶段的可重复实验。
- `Factory.Build` 缓存模型客户端，不等于缓存模型答案、Prompt prefix cache 或模型 KV Cache。
- SSE 是服务端事件推送协议，当前服务端通过周期性 SQL 查询取事件；它不是 Token streaming，也不是完全无轮询的事件总线。
- 当前执行恢复明确要求单实例，见 [RecoverExecutingApprovals](</Users/zxy/oncall agent/internal/store/execution.go:225>) 和 [Executor.Start](</Users/zxy/oncall agent/internal/approval/executor.go:45>)。数据库 CAS 不能单独证明支持多实例部署。

### 1.3 本文的源码依据

| 证据编号 | 源码与函数 | 支撑事实 |
|---|---|---|
| E01 | [AlertmanagerWebhook.serveHTTP](</Users/zxy/oncall agent/internal/api/alertmanager.go:45>)、[diagnose.Worker.drain](</Users/zxy/oncall agent/internal/diagnose/worker.go:88>) | Webhook 落库后返回 202；诊断由独立 worker 消费 |
| E02 | [DB.RequestRun](</Users/zxy/oncall agent/internal/store/runrequest.go:46>)、[requestRun](</Users/zxy/oncall agent/internal/store/runrequest.go:66>) | 父 Incident 行锁、活跃任务排斥、手动冷却、自动重诊预算 |
| E03 | [Pipeline.Run](</Users/zxy/oncall agent/internal/diagnose/pipeline.go:107>)、[memory.Store.Lookup](</Users/zxy/oncall agent/internal/memory/store.go:33>) | 先查故障记忆，命中跳过采集与 LLM，仍执行 Guard/Policy |
| E04 | [DB.GetFaultMemory](</Users/zxy/oncall agent/internal/store/memory.go:33>)、[DB.FinalizeVerification](</Users/zxy/oncall agent/internal/store/verification.go:102>) | 指纹等值召回、验证成功写回、失败降级与重诊同事务处理 |
| E05 | [Registry.ForLLM](</Users/zxy/oncall agent/internal/tools/registry.go:110>)、[ExecuteWithMetadata](</Users/zxy/oncall agent/internal/tools/registry.go:130>) | 只向模型暴露 L1，统一超时与输出截断 |
| E06 | [Policy.Decide](</Users/zxy/oncall agent/internal/approval/policy.go:69>)、[Executor.execute](</Users/zxy/oncall agent/internal/approval/executor.go:130>) | 变更范围快照、PlanHash 校验、执行动作与结果保存分离 |
| E07 | [Executor.persist](</Users/zxy/oncall agent/internal/approval/executor.go:187>)、[VerificationWorker.RunOnce](</Users/zxy/oncall agent/internal/diagnose/verification_worker.go:81>) | 仅重试结果保存；独立验证 worker 单次检查，不等待整个窗口 |
| E08 | [DB.ClaimVerificationTask](</Users/zxy/oncall agent/internal/store/verification.go:27>)、[RequeueStaleVerificationTasks](</Users/zxy/oncall agent/internal/store/verification.go:64>) | 持久化验证任务、领取时间校验、失联任务恢复 |
| E09 | [BuildEvidence](</Users/zxy/oncall agent/internal/diagnose/evidence.go:65>)、[EvidenceBuilder.LoadTarget](</Users/zxy/oncall agent/internal/diagnose/builder.go:29>) | Collector 顺序执行；目标快照组装需多次数据库读取 |
| E10 | [ContextAssembler.Build](</Users/zxy/oncall agent/internal/conversation/context.go:51>)、[listAllMessages](</Users/zxy/oncall agent/internal/conversation/context.go:216>) | 摘要字段限长，但消息先全量读取再截最后 20 条 |
| E11 | [DB.ListAgentRuns](</Users/zxy/oncall agent/internal/store/agentrun.go:23>) | `id > afterID ORDER BY id ASC LIMIT`，是增量列表接口，不是最新一条接口 |
| E12 | [Reasoner.Diagnose](</Users/zxy/oncall agent/internal/llm/reasoner.go:94>)、[Factory.Build](</Users/zxy/oncall agent/internal/llm/factory.go:79>) | ReAct 步数预算、JSON 契约纠错一次、模型客户端复用 |
| E13 | [StreamAPI.serveHTTP](</Users/zxy/oncall agent/internal/api/stream.go:73>)、[writeEvents](</Users/zxy/oncall agent/internal/api/stream.go:163>) | 每连接定时查持久事件、游标续传与连接数控制 |
| E14 | [metrics.Write](</Users/zxy/oncall agent/internal/metrics/metrics.go:31>)、[DB.Open](</Users/zxy/oncall agent/internal/store/db.go:21>) | 当前主要是 counter/gauge；共享 GORM DB，但未显式配置业务库连接池上限 |
| E15 | [verify_task 索引](</Users/zxy/oncall agent/migrations/010_verify_task.sql:12>)、[队列准入索引](</Users/zxy/oncall agent/migrations/011_queue_admission_indexes.sql:1>) | 按到期时间与状态查询的联合索引已经定义 |
| E16 | [systemPrompt](</Users/zxy/oncall agent/internal/llm/prompts.go:5>)、[TestReasonerAgainstRealLLM](</Users/zxy/oncall agent/internal/llm/reasoner_real_test.go:19>) | Prompt 为代码常量；真实模型测试依赖环境变量，属于验收烟测基础 |
| E17 | [PrometheusClient.do](</Users/zxy/oncall agent/internal/tools/prometheus.go:132>)、[seriesMeta](</Users/zxy/oncall agent/internal/tools/prometheus.go:276>) | 共享 HTTP 客户端、有响应体大小上限；元数据每次向上游请求 |

## 2. 系统优化点总览

下表区分“已有机制”和“下一步优化”。已有索引、超时或计数器不意味着其所在领域已完成全部治理。

| 序号 | 优化方向 | 当前状态 | 证据强度 | 简历可写性 | 面试价值 | JD 适配 | 推荐级别 |
|---|---|---|---|---|---|---|---|
| 1 | 摄入与诊断、执行与验证异步解耦 | 已实现，E01/E07 | 强 | 可写 SQL 持久队列与阶段隔离 | 高 | AI 应用、平台后端 | S |
| 2 | 同 Incident 重复诊断准入控制 | 已实现，E02 | 强 | 可写行锁、活跃任务排斥、冷却；不是 SingleFlight | 高 | 平台、一致性治理 | S |
| 3 | 故障记忆复用减少模型调用 | 已实现，E03/E04 | 强 | 可写精确指纹、TTL、验证反馈；无总体节省比例 | 高 | AI 应用、成本控制 | S |
| 4 | Tool 分级、范围校验、超时、截断 | 已实现，E05/E06 | 强 | 可写具体机制；不写完整多租户权限体系 | 高 | Agent、稳定性平台 | S |
| 5 | 执行结果幂等保存与验证恢复 | 已实现，E07/E08 | 强 | 可写不重复动作、持久调度；不写外部 exactly-once | 高 | 平台、可靠性 | S |
| 6 | 预采证据、摘要脱敏与 Tool 输出大小控制 | 已实现，E05/E09/E10 | 强 | 预采证据/对话/审计可写脱敏限长；动态 Tool 回喂仅能写超时、限长与截断标记 | 高 | Agent 上下文治理 | A |
| 7 | 模型客户端、业务库对象复用 | 已实现，E12/E14 | 强 | 可写进程内复用；无“连接池调优完成”证据 | 中 | Go 后端、网络 IO | A |
| 8 | 对话端到端上下文预算 | 克制表达，E10 | 中 | 仅写字段限长；不能写 token 预算/语义压缩 | 高 | AI 应用 | A |
| 9 | 应用可观测性 | 克制表达，E14 | 中 | 有计数器、队列 gauge 与审计；缺阶段分位数基线 | 高 | 平台、可运维性 | A |
| 10 | 队列 SQL 索引治理 | 克制表达，E15 | 中 | 可写针对查询定义联合索引；未证明线上部署及收益 | 高 | 数据访问、平台 | A |
| 11 | 串行 Collector 改有界并行 | 可扩展设计，E09 | 强：当前串行 | 当前不能写已优化 | 高 | Go 并发、AI 延迟治理 | A |
| 12 | 请求折叠 / SingleFlight | 可扩展设计，E17 | 未见实现 | 当前不能写；先找重复请求证据 | 中 | 热点保护、网络 IO | B |
| 13 | 元数据短期缓存 | 可扩展设计，E17 | 未见实现 | 不得把客户端缓存替代为结果缓存 | 中 | 应用缓存、检索 IO | B |
| 14 | 对话尾部查询、重复快照读取收敛 | 可扩展设计，E09/E10/E11 | 强：当前读法可定位 | 当前写成具体优化方案 | 高 | 核心读路径、上下文正确性 | S |
| 15 | LLM 总期限、临时错误退避 | 可扩展设计，E12 | 中：已有步数/格式重试 | 不能称完整超时/重试/熔断体系 | 高 | AI 调用治理 | A |
| 16 | Workflow 到期任务有界批次调度 | 可扩展设计，E07/E08 | 强：已有状态机 | 写现有恢复，补充更细的调度实验 | 高 | 调度、可恢复流程 | A |
| 17 | SQL 连接池上限、等待指标 | 可扩展设计，E14 | 中：已有共享 DB | 当前不能写经过容量调优 | 中 | Go 后端、DB 性能 | A |
| 18 | SSE 重复轮询降低 | 可扩展设计，E13 | 强：每连接轮询 | 可写续传与限流；共享唤醒未实现 | 中 | 实时控制台 | B |
| 19 | Prompt 版本归档、离线诊断 Eval | 可扩展设计，E16 | 中：有常量及测试基础 | 不写完整效果评测平台 | 高 | AI 应用、质量工程 | S |
| 20 | 性能 Benchmark 与回归阈值 | 可扩展设计 | 未见 Go Benchmark/结果集 | 当前无吞吐、P95 改善结论 | 高 | 平台、应用性能 | S |
| 21 | 多租户、用户级 Tool 授权 | 可扩展设计 | 未见完整身份/租户边界 | 现有动作分级不是用户授权 | 中 | 企业 AI 平台 | B，部署需求触发 |
| 22 | RAG、向量召回、embedding 异步化 | 当前不适用 | 未见端到端链路 | 不写；不因简历关键词扩展系统 | 低于现有核心链路 | RAG JD 为岗位差距 | B，暂缓 |
| 23 | 模型 batching/KV Cache/量化/算子优化 | 当前不适用 | 未见模型服务内核 | 不写；应用调用优化不是模型推理优化 | 不适合本项目主线 | AI 性能 JD 为岗位差距 | B，暂缓 |

线程池应翻译为本项目真实的 goroutine 数量、worker 并发上限和取消传播；无需套用 JVM 线程池经历。MQ 消费幂等应翻译为当前 SQL 队列的领取和事务语义，不能增加不存在的 MQ 中间件。

## 3. 关键优化点详解

### 3.1 优化点一：SQL 持久队列隔离慢任务与重复准入

#### 1. 优化类型

稳定性、异步化、工程治理；适配 AI 应用、平台架构、Go 后端。状态为【当前代码已实现，可写入简历】，推荐 S。

#### 2. 原始问题

告警入口若同步等待模型，接入延迟就会受模型和外部工具影响。同一 Incident 的告警、人工重诊和恢复失败重试若各自直接插入任务，则可能形成重叠处理。这里描述机制要解决的问题，不宣称线上曾发生告警丢失或大量重复执行。

#### 3. 当前代码事实

`AlertmanagerWebhook.serveHTTP → CreateRawEvent → Notify → 202` 只负责持久化接入，见 [入口](</Users/zxy/oncall agent/internal/api/alertmanager.go:59>)。`diagnose.Worker.drain → ClaimAgentRun → Pipeline.Run` 在单独 worker 中执行，见 [消费](</Users/zxy/oncall agent/internal/diagnose/worker.go:88>)。

[requestRun](</Users/zxy/oncall agent/internal/store/runrequest.go:66>) 在事务中先锁 Incident，再检查 pending/running run、待处理审批和 verify_task；人工重诊检查一分钟冷却，自动重诊检查链深和归属。数据存储是 MySQL，无 Redis/MQ。

#### 4. 优化方案

当前已经采用“SQL 是事实源、进程通知只用于唤醒”的简单方案，并通过共同准入函数消除多入口各自维护任务规则。应继续复用这套入口。后续先测队列等待时间和每阶段耗时，再判断单 worker 是否需要并发；不直接引入 Kafka 或通用调度平台。

#### 5. Trade-off

持久队列减少额外依赖，但轮询和状态写入给 MySQL 带来负载。异步接收的 202 只代表已入库，不代表诊断完成。同一 worker 内仍顺序执行任务，慢诊断会影响其后续任务。当前单实例边界也限制横向扩展。

#### 6. 可写简历表述

“基于 MySQL 持久队列拆分告警摄入、诊断、执行与恢复验证，使用统一准入事务协调告警、人工重诊和自动重试，避免同一事件重叠处理。”

#### 7. 高频追问

- 为什么不用 MQ？当前单体规模下，业务事务和任务状态共库更容易保持一致，也减少部署成本；是否切换取决于测量后的吞吐和隔离需求。
- 如何防止两个入口同时创建任务？说明先锁共同父行，再在同一事务中检查和写入；“先查再写但不加锁”不足以解决竞争。
- 消费成功却回写失败怎么办？只读诊断可以恢复重跑，变更执行必须遵循执行器的保守恢复规则。

#### 8. 反吹牛审查

面试官可以要求指出 MQ 名称、消费确认机制及多实例租约。第三层最容易暴露的是把 SQL 状态机叫作 Kafka 消费、把 CAS 当作外部动作 exactly-once、把分 worker 说成无限并发。

#### 9. 能写 / 不要写

能写：异步持久队列、统一准入、短事务、冷却与重试预算。不要写：分布式消息平台、千万告警吞吐、多实例高可用、保证外部动作精确一次。

### 3.2 优化点二：故障记忆命中跳过采集与模型调用

#### 1. 优化类型

AI 应用成本、重复计算治理、存储检索；适配 Agent/AI 应用岗位。状态为【当前代码已实现，可写入简历】，推荐 S。

#### 2. 原始问题

相同类型故障重复发生时，重复采集和推理会消耗外部 IO 与模型调用。另一方面，未经验证的旧建议不能直接变成执行权限。当前没有总体命中率、平均节省 Token 或延迟改善比例，不能据此编造收益。

#### 3. 当前代码事实

[Pipeline.Run](</Users/zxy/oncall agent/internal/diagnose/pipeline.go:119>) 在非重诊路径按故障指纹查记忆。命中后标记 `memory_hit`，跳过证据采集和 `runReasonStep`，随后继续 Guard、Policy、审批和验证。

[Lookup](</Users/zxy/oncall agent/internal/memory/store.go:33>) 检查置信度开关及基于 `LastSuccess` 的 TTL，命中还要更新计数；[GetFaultMemory](</Users/zxy/oncall agent/internal/store/memory.go:33>) 是 SQL 等值查找。验证成功写回和失败降级在 [FinalizeVerification](</Users/zxy/oncall agent/internal/store/verification.go:189>) 的事务中完成。故障恢复不是由模型自己声称成功。

#### 4. 优化方案

当前方案把“已验证经验复用”放在昂贵链路之前，又把安全判断保留在命中之后。进一步优化时优先记录命中后的验证结果、误命中案例和过期原因，而不是立即给 `fault_memory` 再套 Redis。记忆本身来自业务结果，写回和降级时机比缓存命中率更重要。

#### 5. Trade-off

精确指纹召回解释性好、实现简单，但对不同表述或邻近故障没有语义泛化；指纹相同也不保证故障原因永远相同。TTL 和失败降级降低陈旧风险，但不能替代执行前范围校验。命中省去模型调用，却不等于整个流程无 IO、无审批或必然更快。

#### 6. 可写简历表述

“设计基于故障指纹、置信度和有效期的历史处置复用机制，命中后跳过证据采集与 LLM 诊断，仍保留安全校验和恢复验证，并根据验证结果写回或降级记忆。”

#### 7. 高频追问

- 缓存什么、key 是什么？缓存的是 MySQL 中的历史 RCA/Plan 条目，按代码定义的故障指纹召回，不是全文 Prompt 响应缓存。
- 为什么自动重诊绕过记忆？上一轮恢复失败后，再命中同一旧计划可能重复失败。
- 能节省多少？单次命中路径可证明不调用诊断 LLM；全局节省取决于真实命中率和原始调用量，目前无测量报告。

#### 8. 反吹牛审查

验证问题：向量库在哪里，embedding 模型是什么，召回阈值如何评估？当前都没有对应实现。第三层风险是把 SQL 精确匹配包装为 RAG，或把“零次 LLM 调用”扩张成“零成本自动自愈”。

#### 9. 能写 / 不要写

能写：故障经验复用、验证反馈闭环、命中分支跳过 LLM。不要写：语义检索、RAG Recall@K、知识库问答、Prompt Cache 命中、成本下降某个百分比。

### 3.3 优化点三：受控 Tool 执行与恢复验证解耦

#### 1. 优化类型

稳定性、工作流可靠性、Tool 治理；适配 Agent、平台和可靠性岗位。状态为【当前代码已实现，可写入简历】，推荐 S。

#### 2. 原始问题

模型生成计划不应自动获得写权限；动作返回不等于服务恢复；动作执行后保存失败时盲目重试会扩大副作用。若执行器在一个循环中等待完整健康窗口，后续动作也会等待。以上是已实现机制对应的工程问题，不是已测量的事故复盘。

#### 3. 当前代码事实

[ForLLM](</Users/zxy/oncall agent/internal/tools/registry.go:110>) 只导出 L1 工具。Policy 构造并校验动作范围、验证地址、成员指纹等执行快照；[Executor.execute](</Users/zxy/oncall agent/internal/approval/executor.go:130>) 校验 PlanHash 和当前绑定后才执行。

[Executor.persist](</Users/zxy/oncall agent/internal/approval/executor.go:187>) 在保存失败时只重试保存结果，不再次调用工具。[VerificationWorker.RunOnce](</Users/zxy/oncall agent/internal/diagnose/verification_worker.go:81>) 独立领取到期的只读检查，未结束时保存下一次检查时间；[FinalizeVerification](</Users/zxy/oncall agent/internal/store/verification.go:124>) 验证领取时间，避免旧领取者覆盖新状态。

#### 4. 优化方案

当前实现将动作与效果验证拆开，并按副作用区分恢复策略：只读验证可重新领取；结果写入可幂等重试；中断变更不自动重放。后续应在这一状态机上测到期任务排队延迟，优化有界批次消费，而不是重写成通用 Workflow 框架。

#### 5. Trade-off

分阶段后需要维护持久化任务和状态关联，但避免长时间占用执行循环。外部动作与 MySQL 提交无法形成一个普通数据库事务，因此保守恢复可能需要人工确认。`executing` 恢复只适用于单实例，不能无条件迁移到多实例。

#### 6. 可写简历表述

“构建 L1–L4 工具分级、审批快照和 PlanHash 校验，拆分变更执行与持久化恢复验证；结果保存失败仅重试提交，中断动作转人工核验，避免恢复流程重复触发变更。”

#### 7. 高频追问

- 为什么不自动重试重启？请求超时不代表重启未发生，必须区分调用结果未知和明确未执行。
- 为什么验证还需要重新检查范围？审批后的配置、成员状态可能变化，验证结果必须绑定当时批准的目标。
- `passed/failed/inconclusive` 如何区分？有健康观测才通过；窗口结束时需要新鲜的不健康观测才能认定失败；信息不足不能伪装为成功或明确失败。

#### 8. 反吹牛审查

验证问题：进程在 Docker 动作结束、数据库提交之前崩溃怎么办？需要坦诚说明当前会留下需人工核验的中断执行，不能说存在跨 Docker/MySQL 的 exactly-once。第三层风险是把工作流恢复等价于任意节点自动重放。

#### 9. 能写 / 不要写

能写：动作安全边界、事务审计、持久验证、保守恢复。不要写：通用分布式工作流、任意节点断点续跑、零人工、零重复副作用保证、多实例执行协调。

### 3.4 优化点四：输出限长已有基础，端到端上下文与读路径仍可收敛

#### 1. 优化类型

AI 成本、内存控制、存储访问。现有字段限长为【当前代码已实现，可写入简历】；端到端预算为【当前代码有迹象，只能克制表达】；尾部查询为【可扩展设计】。适配 AI 应用、平台后端，推荐 A/S。

#### 2. 原始问题

工具响应、历史消息、运行步骤可以随事件持续增长。只在组装 Prompt 时截断，不一定减少数据库读取和 Go 对象分配。这里有明确静态读取路径，但尚未用大样本实验复现其性能影响。

#### 3. 当前代码事实

[finishItem](</Users/zxy/oncall agent/internal/diagnose/evidence.go:110>) 对单证据正文脱敏并按 2048 rune 截断；[ExecuteWithMetadata](</Users/zxy/oncall agent/internal/tools/registry.go:130>) 记录截断状态；[ContextAssembler.Build](</Users/zxy/oncall agent/internal/conversation/context.go:51>) 对摘要字段限长，步骤最终保留 32 条、消息保留 20 条。

必须区分预采证据和模型动态调用工具：后者从 [executeLLMTool](</Users/zxy/oncall agent/internal/llm/tool_args.go:19>) 经过参数规范化与 Registry 超时/截断，再由 [registryTool.InvokableRun](</Users/zxy/oncall agent/internal/llm/reasoner.go:270>) 回喂模型；这一统一路径没有输出脱敏处理。例如 [Docker 日志返回](</Users/zxy/oncall agent/internal/tools/docker.go:314>) 直接输出解帧后的文本。[sanitizeForStep](</Users/zxy/oncall agent/internal/diagnose/pipeline.go:550>) 的审计脱敏发生在模型已经接收工具结果之后，不能证明模型直接接收的所有工具输出已脱敏。

但消息先调用 [listAllMessages](</Users/zxy/oncall agent/internal/conversation/context.go:216>) 分页读完再截断；run 读取最早 100 条后取最后一条，而 [ListAgentRuns](</Users/zxy/oncall agent/internal/store/agentrun.go:23>) 明确升序分页。因此，同一 Incident 超过 100 个 run 时，“latest”在静态逻辑上会指向首批最后一条；这不是已经复现的线上故障。Prompt 最终没有统一 token 总预算。

#### 4. 优化方案

直接在 store 增加面向业务的最近消息和最新 run 查询：消息 `id <= 当前问题 ID`、`ORDER BY id DESC LIMIT 20` 后反转展示；最新 run 使用 `ORDER BY id DESC LIMIT 1`。明确上下文快照取组装时刻还是问题入队时刻，再添加对应边界测试，避免把未来消息泄入旧问题。

保留字段摘要，并在序列化前按结构删减低优先级历史，确保仍是有效 JSON。若仅用 rune/byte 近似 token，必须标明估算，不能宣称精确 Token 预算。

#### 5. Trade-off

更小的上下文会丢失部分历史，因此保留事件、当前问题、关键失败证据和必要动作状态；完整历史仍可通过数据库审计查询。查询方向改变要配合明确排序契约。不要直接截整个 JSON 字符串，否则会破坏结构和回答输入。

#### 6. 可写简历表述

当前可写：“对预采证据、对话摘要和审计记录实施脱敏与限长；对模型直接调用的工具输出实施超时、限长和截断标记。”

后续方案写法：“识别对话上下文先全量读取后截断的访问模式，设计尾部查询和结构化总预算。”未实施前不能写成已降低读取量。

#### 7. 高频追问

- 消息保留 20 条是否等于只查 20 条？当前不是，这是最容易被追问的关键区别。
- 2048 rune 等于多少 token？没有固定换算，语言、模型和内容都会影响分词。
- 如何证明最新 run 正确？准备超过 100 条 run 的回归样本，断言选择最新 ID，而不是第一页尾部。

#### 8. 反吹牛审查

面试官可以要求现场查看 SQL 日志、长会话的分配量和 Prompt 长度。第三层风险是把“单字段截断”包装为语义压缩、自动摘要、检索增强或完整上下文预算系统。

#### 9. 能写 / 不要写

能写：预采证据/对话摘要/审计的脱敏限长、动态工具回喂的超时与截断可见性、具体读取优化方案。不要写：所有模型输入已统一脱敏、Token 成本降低比例、无损上下文压缩、长期记忆、多轮摘要体系已上线。

### 3.5 优化点五：独立证据采集的有界并行

#### 1. 优化类型

性能、网络 IO、Go 并发。状态为【当前未实现，仅作为后续优化方案】，适配 Go 后端和 AI 应用性能岗位，推荐 A。

#### 2. 原始问题

[BuildEvidence](</Users/zxy/oncall agent/internal/diagnose/evidence.go:65>) 明确按循环依次采集；[sub2apiCollector.collectMetrics](</Users/zxy/oncall agent/internal/diagnose/collector_sub2api.go:79>) 也按顺序查询三类指标。外部 IO 时延会累加，但实际哪个数据源最慢需要基准证明，不能说线上已存在某个 P99 瓶颈。

#### 3. 当前代码事实

Collector 共享只读 `Target` 输入，单项失败写入 `EvidenceItem`，不返回全局错误；当前采集顺序同时承担输出稳定性的作用。`Pipeline.Run` 先 `LoadTarget`，未命中记忆时 `BuildForIncident` 又调用一次 `LoadTarget`，存在可定位的重复读取。

#### 4. 优化方案

先让一次诊断使用同一份明确的证据目标快照，再对独立 Collector 设置小固定并发上限，按注册下标保存结果、统一等待后按原顺序渲染。每个 Collector 仍产生成功/失败/缺失项，单项失败不取消全部证据。共享快照仅用于诊断；执行前事务校验仍必须读取最新范围。

```go
// 可扩展设计示意，非当前接口；先验证各 Collector 只读 Target。
items := make([]EvidenceItem, len(collectors))
parallelism := 3 // 起点假设，后续由基准调整
runBounded(ctx, parallelism, len(collectors), func(i int) {
    items[i] = collectors[i].Collect(ctx, target)
})
return Evidence{IncidentID: target.Incident.ID, Items: items}
```

`runBounded` 是伪代码，不建议为此引入通用并发框架；实现可直接使用标准库 WaitGroup 和固定 worker。不要让多个 goroutine 同时写同一个 `strings.Builder`，也不要同时改 Eino 内部工具调度。

#### 5. Trade-off

总体墙钟时间可能下降，但同一时刻的上游负载、DB 连接和 goroutine 数量上升。并发过大可能把本系统的低延迟换成被监控系统的额外压力。限时、取消、固定结果顺序和资源清理必须一起验证。

#### 6. 可写简历表述

当前仅可写：“针对独立证据采集设计有界并行和稳定结果聚合方案。”实施并完成对比实验后，才能写“将独立采集改为有界并行”；比例必须来自同条件基准。

#### 7. 高频追问

- 为什么能并行？检查依赖、共享可变状态和上游容量，而不是看到多个函数就并发。
- 某一路失败是否取消其他任务？当前 Collector 的语义是记录失败后继续，改造需保留。
- 为什么结果按注册顺序写入？稳定输出有助于对照、审计和复现实验。

#### 8. 反吹牛审查

验证问题：哪几个请求真的被并行，如何运行 `-race`，最大同时请求数是多少？第三层风险是仅启动 goroutine 就宣称线程池治理，或把理论串行耗时之和当真实改善数据。

#### 9. 能写 / 不要写

能写：待实施方案、依赖判断、并发上限和验收设计。不要写：当前已经并行、QPS 提升若干倍、所有工具都可并发、必须加大线程池。

### 3.6 优化点六：客户端复用不等于完整 LLM 调用治理

#### 1. 优化类型

网络 IO、稳定性、LLM 成本。客户端复用和契约纠错为【当前代码已实现，可写入简历】；整体调用治理为【当前代码有迹象，只能克制表达】。适配 AI 应用岗位，推荐 A。

#### 2. 原始问题

重复创建客户端会增加初始化成本；无边界的模型工具循环会扩大调用量；模型偶发返回不合法 JSON 会破坏后续流程。另一方面，步数上限不限制单次网络请求可能等待多久。

#### 3. 当前代码事实

[Factory.Build](</Users/zxy/oncall agent/internal/llm/factory.go:79>) 在互斥锁内缓存模型客户端；[SelectModel](</Users/zxy/oncall agent/internal/llm/factory.go:32>) 清空缓存供后续调用使用。[Reasoner.Diagnose](</Users/zxy/oncall agent/internal/llm/reasoner.go:94>) 配置 ReAct 最大步数，只有输出契约解析失败才显式纠错一次。

[Worker.drain](</Users/zxy/oncall agent/internal/diagnose/worker.go:88>) 传入进程上下文；[conversation.Worker.process](</Users/zxy/oncall agent/internal/conversation/worker.go:204>) 也直接调用 Questioner。本项目业务代码没有为每次 run/问答建立完整总期限。不能从源码未配置推断 SDK 一定永不超时，SDK 默认行为需另按当前依赖版本验证。

#### 4. 优化方案

在业务调用边界建立总期限，预算覆盖工具调用、模型轮次和格式纠错。对明确的临时网络/服务错误设置有上限的退避；非法鉴权、参数错误和取消不重试。先确认 SDK 本身的重试策略，避免双层重试乘法放大。期限结束时保存失败事实，交回人工，不用未经验证的低质量模型结果触发动作。

#### 5. Trade-off

期限过短会误伤合理复杂诊断，过长会拖住单 worker。重试可能增加成本和队列等待；格式纠错与网络重试应共享总预算。复用客户端减少初始化，但并不表示已调优连接池、自动熔断或自动选择最佳模型。

#### 6. 可写简历表述

当前可写：“通过模型客户端复用、分级 ReAct 步数预算和一次结构化输出纠错，限制调用复杂度并稳定模型输出契约。”

不要把这句话扩张成“实现全链路熔断降级、自动模型路由与 Prompt 缓存”。

#### 7. 高频追问

- 最大步数和最大工具调用数完全等价吗？应按当前 Eino 版本的计数语义解释，不能凭名称认定。
- 网络失败会不会重试？本项目显式逻辑只展示契约纠错；SDK 默认机制需独立核验。
- 模型切换时正在执行的调用怎么办？现有客户端引用继续执行，新建调用获取切换后的客户端。

#### 8. 反吹牛审查

验证问题：整体超时在哪里，429 与 401 是否区别处理，重试一共可能发生几次？第三层风险是把工具 `context.WithTimeout` 误说成所有模型请求都有业务期限。

#### 9. 能写 / 不要写

能写：客户端缓存、最大步数、一次契约纠错。不要写：模型结果缓存、智能降级路由、分布式限流、模型内核推理优化。

### 3.7 优化点七：SSE 与队列索引已有控制，性能闭环尚需补齐

#### 1. 优化类型

实时链路、数据库读取、可观测性。已有机制为【当前代码已实现，可写入简历】；性能治理整体为【当前代码有迹象，只能克制表达】。适配平台、后端岗位，推荐 A。

#### 2. 原始问题

多个浏览器同时看同一 Incident 时，周期性拉取会重复查相似事件；积累队列状态又需要合适索引。只知道接口功能正确，不知道查询耗时、连接等待和读取放大，就无法判断瓶颈。

#### 3. 当前代码事实

[NewStreamAPI](</Users/zxy/oncall agent/internal/api/stream.go:43>) 默认一秒轮询、十分钟连接寿命和 100 个连接上限；[writeEvents](</Users/zxy/oncall agent/internal/api/stream.go:163>) 每次查询至多 100 条 `id > cursor` 的持久事件，并输出心跳。

[migrations/011](</Users/zxy/oncall agent/migrations/011_queue_admission_indexes.sql:1>) 为审批待执行查询、Incident 活跃任务检查等定义联合索引；[migrations/010](</Users/zxy/oncall agent/migrations/010_verify_task.sql:12>) 为到期验证和过期领取定义索引。[metrics.Write](</Users/zxy/oncall agent/internal/metrics/metrics.go:31>) 输出 counter/gauge，没有阶段耗时直方图实现。

#### 4. 优化方案

先在测试数据集上记录每连接 SQL 数、事件投递延迟和 DB 等待时间，再决定是否做同 Incident 共享唤醒。共享信号只表示“可能有新事件”，每个客户端仍用持久化游标补读。数据库索引只按真实查询和 EXPLAIN 结果调整；迁移文件存在不能证明运行库已应用，也不能当慢 SQL 优化效果报告。

#### 5. Trade-off

共享订阅状态会增加进程生命周期和断线清理逻辑，因此只有确实存在多观察者读放大时才值得做。索引增加写入与存储成本，不能给每个字段都建索引。将事件存入内存后完全跳过数据库，会破坏当前恢复和续传语义。

#### 6. 可写简历表述

“基于持久事件和游标构建 SSE 控制台更新，设置连接数与生命周期边界，并为任务准入、审批消费和到期验证设计针对性联合索引。”

#### 7. 高频追问

- SSE 为什么还轮询？协议负责对浏览器持续输出，服务端数据来源仍是 MySQL。
- 100 个空闲连接会查多少次库？按默认每秒每连接一次推导约 100 次查询/秒，仅是结构计算，不是压测结果，还不含初始查询与其他业务请求。
- 索引是否有效？需要实际数据分布、执行计划和读写成本数据。

#### 8. 反吹牛审查

面试官会查 P95/P99 指标、SQL 执行计划和浏览器负载脚本。第三层风险是把被监控 Sub2API 的延迟指标说成 AI-Opus 自身延迟基线，或把计数器叫作完整 APM 链路。

#### 9. 能写 / 不要写

能写：SSE 游标恢复、连接限流、联合索引设计、基础指标。不要写：无轮询实时总线、低延迟收益比例、完善的分布式追踪、海量连接承载能力。

## 4. 当前还缺、最值得补充的 10 条设计

以下**恰好十条**均为【可扩展设计】。已有基础不会重复列为未实现。顺序按照当前项目价值和最小改动优先排列；第 7、9、10 项需要重复请求、访问范围或多观察者需求作为触发条件。伪代码仅用于说明设计，不是当前可调用 API。

### 4.1 建议补齐 1：建立离线诊断与对话效果 Eval

- **当前状态：**【可扩展设计】。
- **建议补充模块：** `internal/llm` 的独立评测用例和脱敏 `testdata`，优先复用现有 Reasoner/Questioner 接口。
- **当前代码已有基础：** [Reasoner.Diagnose](</Users/zxy/oncall agent/internal/llm/reasoner.go:94>) 结构化输出、[真实模型烟测](</Users/zxy/oncall agent/internal/llm/reasoner_real_test.go:19>)、现有假模型 HTTP 测试。测试基础不能直接称为效果评测体系。
- **当前缺口：** 没有按故障分类的固定数据集、标签、质量指标、版本对比报告和效果回归门槛。
- **为什么值得补：** 简历中最需要回答的是“答案是否正确、安全边界是否稳定”，而不是只证明模型返回非空字符串。当前本就有故障诊断和只读问答，评测无需扩展新产品能力。
- **补完后的简历亮点：** “围绕诊断结论、证据引用和动作安全边界建立离线评测集，实现 Prompt/模型变更的可重复回归验证。”
- **适配目标公司：** 按用户提供的参考口径，适合字节 AI 应用、腾讯工程质量、Amazon AI 后端；这是能力映射，不是实际招聘公告核验。
- **适配 JD：** AI 应用架构、Agent/Workflow 的效果优化、Eval 和失败兜底；不适用于宣称 RAG Eval。
- **设计草图：** 脱敏故障样本 → 固定只读工具响应 → Reasoner/Questioner → 契约/引用/安全断言 → 按模型与 Prompt 版本汇总。首先做不访问外部服务的确定性测试；真实模型质量批跑在另行授权的实验环境中执行并保存结果。
- **伪代码位置：** 新增评测文件，调用现有 `Diagnose`，例如 `score(caseID, result.RCA, result.EvidenceRefs, result.Plan)`；评分项包括契约合法、证据引用存在、错误目标拒绝、缺证据降级、禁止动作拒绝。需要语义判断的 RCA 正确性由人工标注规范和复核处理，不能仅字符串相等。
- **验收指标：** 每类故障/缺证据/注入/错误目标均有样本；确定性安全断言零违反；报告样本数量、失败明细、模型版本、Token 和耗时。准确率以固定分母和实测结果报告，不预填改善比例。
- **风险和代价：** 样本标注需时间，真实模型结果有随机性；小数据集不可证明线上整体准确率。敏感日志先脱敏，回放工具禁止产生变更。
- **不能现在写成已实现的原因：** 当前只有测试和烟测基础，没有上述数据与评测报告。

### 4.2 建议补齐 2：归档 Prompt、模型与推理配置版本

- **当前状态：**【可扩展设计】。
- **建议补充模块：** `internal/llm` 调用元数据、`internal/store` 的 run 审计字段；不建设独立 Prompt 管理平台。
- **当前代码已有基础：** [systemPrompt](</Users/zxy/oncall agent/internal/llm/prompts.go:5>) 是常量；[Factory.SelectModel](</Users/zxy/oncall agent/internal/llm/factory.go:32>) 可切模型；步骤记录已有输入输出摘要。
- **当前缺口：** 单次诊断不能完整追溯自己使用的 Prompt hash、模型 profile、预算配置及代码版本；当前选择记录不能替代历史 run 使用版本。
- **为什么值得补：** 回答发生变化时，只有可比的版本与输入才能判断是模型、Prompt、配置还是证据变化；也是第 1 项 Eval 的必要标识。
- **补完后的简历亮点：** “将 Prompt、模型和预算版本纳入运行审计，支持诊断效果回归对照与版本归因。”
- **适配目标公司：** 字节 AI 应用、腾讯质量治理、Google Senior 工程设计口径。
- **适配 JD：** Prompt 工程、AI 产品迭代、可观测与可复现系统设计。
- **设计草图：** 调用开始时固定 `promptHash/modelID/profileHash/budget/codeRevision` → 与 run 元数据一起落库 → 评测报告引用同一组标识。实际客户端与版本元数据必须来自同一次锁保护的选择快照，避免切换模型时记错版本。
- **伪代码位置：** `Factory.Build` 的调用处获取不可变元数据快照；`runReasonStep` 附加摘要。示意：`client, meta := BuildWithMetadata(); result := DiagnoseWith(client); persistRunMeta(meta)`。这是拟议接口，不是当前方法。
- **验收指标：** 每个模型调用均可定位版本；切换时正在执行的 run 仍记录旧模型；相同版本可从 Git/配置归档还原；脱敏审计不含密钥、完整 DSN 或未裁剪输入。
- **风险和代价：** 增加少量存储字段和迁移；只存 hash 却不保留可还原版本，仍不能复现实验。避免为此引入新配置中心。
- **不能现在写成已实现的原因：** 代码常量与运行模型选择存在，但缺少每次调用的不可变版本归档闭环。

### 4.3 建议补齐 3：建立应用 Benchmark、阶段指标与连接池基线

- **当前状态：**【可扩展设计】。
- **建议补充模块：** `internal/metrics`、诊断/对话 worker、`internal/store/db.go`、本地基准用例。
- **当前代码已有基础：** [metrics](</Users/zxy/oncall agent/internal/metrics/metrics.go:19>) 的 counter/gauge、run/step 时间字段、[共享 DB](</Users/zxy/oncall agent/internal/store/db.go:21>)。
- **当前缺口：** 未见阶段耗时分布、队列等待、SQL 数量/扫描量、分配量、连接池等待统计、受控负载下的基准报告。`DB.Open` 没有显式业务库池上限设置。
- **为什么值得补：** 为并行、缓存、池上限和批次大小提供依据，先回答“时间花在哪、容量受哪层约束”。这比一次引入多个优化机制更便于归因。
- **补完后的简历亮点：** “建立诊断与对话链路基准，结合阶段耗时、队列等待和数据库池指标定位瓶颈，并以可重复实验验证优化。”
- **适配目标公司：** 阿里核心链路、腾讯稳定性、Amazon operational excellence、Binance Go/AI 后端口径。
- **适配 JD：** 应用性能、后台服务治理；不是 GPU inference benchmark。
- **设计草图：** 固定事件数/历史规模/假工具延迟 → 记录排队与执行时间 → 输出 P50/P95/P99、成功率、SQL 次数和分配量；上线指标先添加少数低基数阶段名。SQL 池从 `database/sql.Stats()` 读取 `InUse/WaitCount/WaitDuration`，实际池上限按负载与数据库连接预算选取。
- **伪代码位置：** `Worker.drain` 记录排队等待，`Pipeline.withStep` 周围记录阶段耗时；本地 `BenchmarkContextBuild`、`BenchmarkBuildEvidence` 使用固定假依赖。`sqlDB.SetMaxOpenConns(n)` 仅在完成容量实验后设置，`n` 不在方案中拍脑袋填写。
- **验收指标：** 报告包含代码版本、硬件/环境、输入规模、并发、暖机、样本量、错误率与方差；相同条件可复跑；观察指标不使用 Incident ID 或 Prompt 文本作标签，避免无界基数。
- **风险和代价：** 指标本身有开销；连接池过小会排队，过大会压垮 MySQL。测试结果不能外推为生产吞吐。是否采用额外指标库应单独评估，不因文档建议自动增加依赖。
- **不能现在写成已实现的原因：** 当前指标和测试没有构成上述性能基线与回归报告。

### 4.4 建议补齐 4：对话上下文有界读取，并复用诊断目标快照

- **当前状态：**【可扩展设计】。
- **建议补充模块：** `internal/conversation/context.go`、`internal/store/conversation.go`、`internal/store/agentrun.go`、`internal/diagnose/builder.go`。
- **当前代码已有基础：** [ContextAssembler.Build](</Users/zxy/oncall agent/internal/conversation/context.go:51>) 和 [EvidenceBuilder.LoadTarget](</Users/zxy/oncall agent/internal/diagnose/builder.go:29>) 已有明确装配入口；当前字段限长继续复用。
- **当前缺口：** 对话全量历史读取后裁剪；最新 run 从最早 100 条中选择；诊断 memory miss 路径重复装载 Target。静态路径明确，实际影响尚待实验。
- **为什么值得补：** 同时提升输入正确性、DB 访问效率和可理解性，减少已有逻辑的读放大，不需要新依赖。
- **补完后的简历亮点：** “将对话上下文改为有界尾部查询，统一诊断证据快照，减少历史数据增长导致的读取放大，并用边界样本验证上下文正确性。”
- **适配目标公司：** 阿里核心读链路、字节 Agent 上下文、腾讯工程质量、Binance 后端。
- **适配 JD：** 数据访问与检索路径优化、AI 上下文管理；不包装为分布式存储引擎优化。
- **设计草图：** 当前问题 ID 定界 → SQL 只取最近需要的消息 → 倒序结果反转 → 最新 run 专用查询 → 结构化裁剪；诊断 `LoadTarget` 一次 → memory miss 时同一 Target 交给 Collector。执行范围检查继续从事务中读取新状态。
- **伪代码位置：** `ListRecentConversationMessages(incidentID, throughID, 20)` 使用 `WHERE incident_id=? AND id<=? ORDER BY id DESC LIMIT 20`；`GetLatestAgentRun(incidentID)` 使用降序取一条。伪方法替代原调用后删除仅供该路径使用的全量遍历，避免留两套实现。
- **验收指标：** 0/20/21/1000 条消息和 101 条 run 样本；所选最新 run 正确，旧问题不含入队后的消息；消息读取行数有固定上限；比较变更前后 SQL 次数与分配量；Prompt 是有效 JSON。
- **风险和代价：** 需要确定入队时与处理时快照语义；近 20 条可能不足以覆盖长历史关键结论，先保留最新 run 和未解决问题作为结构化事实，不立刻增加模型摘要器。
- **不能现在写成已实现的原因：** 当前源码仍使用全量消息遍历及第一页 run 选择，本次未修改代码或运行复现实验。

### 4.5 建议补齐 5：独立 Collector 的有界并行与稳定聚合

- **当前状态：**【可扩展设计】。
- **建议补充模块：** `internal/diagnose/evidence.go`，只处理明确独立的 Collector，不改变动作执行器。
- **当前代码已有基础：** [BuildEvidence](</Users/zxy/oncall agent/internal/diagnose/evidence.go:65>) 统一入口、Collector 接口和 `EvidenceItem` 失败表示。
- **当前缺口：** 当前所有 Collector 按顺序等待，没有采集层并发上限与整体采集期限。
- **为什么值得补：** 第 3 项基准如果确认外部 IO 为主要耗时，可在不增加服务的情况下缩短采集等待。
- **补完后的简历亮点：** “基于依赖分析将独立证据采集改为有界并行，保持稳定结果顺序、取消传播和单项失败留痕。”
- **适配目标公司：** 字节 AI 应用、阿里后台链路、Binance Go 性能方向。
- **适配 JD：** Go 并发与 IO、Agent 应用延迟治理。
- **设计草图：** 不变 Target → 固定数量 worker → 按索引保存 `EvidenceItem` → 统一汇总。每项读取后端各自期限，同时受整体采集上下文约束。
- **伪代码位置：** 将 `BuildEvidence` 的顺序循环替换为局部固定 worker；每个 goroutine 只写自己下标。详细示意见 3.5，禁止共享写 `strings.Builder`。
- **验收指标：** 假 Collector 可统计最大活跃数且不超过上限；结果顺序与注册顺序一致；失败项不丢失；取消后任务退出；运行 race 检查；以相同延迟分布比较耗时与资源开销。
- **风险和代价：** 瞬时上游压力增加，需调节并发；部分 Collector 若含隐式共享状态需先整改。只对已证明独立的调用并行，后端查询依赖仍串行。
- **不能现在写成已实现的原因：** 当前 `BuildEvidence` 是显式顺序循环。

### 4.6 建议补齐 6：为诊断与问答建立业务总期限和有限重试预算

- **当前状态：**【可扩展设计】。
- **建议补充模块：** `internal/diagnose/worker.go`、`internal/conversation/worker.go`、`internal/llm` 调用边界及预算配置。
- **当前代码已有基础：** [工具超时](</Users/zxy/oncall agent/internal/tools/registry.go:135>)、[ReAct 最大步数和契约纠错](</Users/zxy/oncall agent/internal/llm/reasoner.go:108>)；失败能留步骤和问题记录。
- **当前缺口：** 业务 run/问答缺少覆盖所有轮次和重试的总期限；临时错误与永久错误的应用层处理策略尚不完整。
- **为什么值得补：** 单 worker 顺序消费时，异常慢的模型请求会增加后续任务等待。先用总期限控制边界，比直接叠加熔断器和备用模型更简单。
- **补完后的简历亮点：** “建立跨模型轮次与只读工具的统一时间预算，分类处理临时失败并限制重试放大，保留可审计的失败状态。”
- **适配目标公司：** 腾讯稳定性、Amazon 运维质量、字节 AI 工程化。
- **适配 JD：** LLM 应用稳定性、后台超时与重试治理。
- **设计草图：** 领取任务 → 创建 run deadline → 执行模型与工具 → 仅在剩余预算足够时重试可恢复错误 → 预算耗尽记失败/人工处置。写库需要与处理取消明确区分，但不能使用无限制 detached context 绕过停机语义。
- **伪代码位置：** `callCtx, cancel := context.WithTimeout(ctx, runBudget)` 包住一次诊断/问答；网络重试先读取当前 SDK 实际策略，只对可判定的临时错误执行有限退避，并与一次格式纠错共享预算。
- **验收指标：** 假服务长时间不响应时业务调用在预算内结束；401/参数错误不重试；取消及时传播；总尝试数可精确断言；没有任何失败路径绕过审批执行动作。
- **风险和代价：** 预算过紧会降低复杂诊断完成率；双层重试可能扩大成本；过期后的审计保存需有明确有界策略。暂不引入自动模型降级，避免用未经 Eval 的模型改变处置质量。
- **不能现在写成已实现的原因：** 现有工具超时与格式重试没有覆盖整个业务调用期限。

### 4.7 建议补齐 7：为重复 Prometheus 元数据查询加入短期缓存和请求折叠

- **当前状态：**【可扩展设计】，由重复请求观测触发，非立即必做。
- **建议补充模块：** `internal/tools/prometheus.go` 的 `seriesMeta` 入口；不缓存变更动作或恢复健康结论。
- **当前代码已有基础：** [seriesMeta](</Users/zxy/oncall agent/internal/tools/prometheus.go:276>) 统一读取元数据；客户端和响应体上限已存在。
- **当前缺口：** 未见结果缓存与在途请求合并；重复元数据请求会独立到达上游，但当前实际重复率未知。
- **为什么值得补：** 若不同诊断/问答反复读取相同指标结构，小范围缓存能减少冗余 IO；不需要 Redis。读取当前健康状态和执行许可则不应靠这种缓存决定。
- **补完后的简历亮点：** “针对稳定元数据引入有界短期缓存与同键请求折叠，减少重复外部查询，并保持故障观测和权限判断的新鲜度。”
- **适配目标公司：** 阿里核心链路、字节 AI 应用、Binance 后端。
- **适配 JD：** 网络 IO、缓存一致性、热点保护，不是 RAG 检索缓存。
- **设计草图：** `配置版本 + endpoint + 规范化 selectors + limit` 作为 key → 命中 TTL 缓存 → 否则等待同键在途请求 → 成功后更新有大小上限缓存。错误不长期缓存；变更 endpoint 配置使 key 失效。
- **伪代码位置：** `seriesMeta` 解析参数后 `lookupOrLoad(key, loadSeries)`；请求折叠复用当前依赖树已锁定的 `golang.org/x/sync v0.14.0`，不手写通用折叠框架。已核对该版本 [Group.DoChan](</Users/zxy/go/pkg/mod/golang.org/x/sync@v0.14.0/singleflight/singleflight.go:121>)：它返回单次结果 channel，不负责请求 context，也不关闭结果 channel。等待者用自身 `ctx.Done()` 取消等待，共享加载用服务生命周期派生的有界上下文，不能继承第一个等待者的取消；完成后结果由局部有界 TTL 缓存保存。
- **验收指标：** 同键并发 N 个调用只触发一次有效上游加载；等待者取消不影响其他调用；超时后无永久在途条目；TTL 到期可刷新；不同 endpoint/selector 不串结果；记录命中率和实际削减请求数。
- **风险和代价：** 首个请求慢会拖住等待者；缓存可能短暂陈旧，TTL 与 key 正确性比命中率重要。若重复率低，收益不足以覆盖新状态维护成本，应取消此项。
- **不能现在写成已实现的原因：** 现有客户端缓存不等于元数据响应缓存，当前每次 `seriesMeta` 都调用上游。

### 4.8 建议补齐 8：在现有验证恢复状态机上优化到期任务调度

- **当前状态：**【可扩展设计】；**持久状态、领取与失败恢复已经实现**。
- **建议补充模块：** `internal/diagnose/verification_worker.go`；必要时仅增加有界批次参数和等待指标，不重建 Workflow 引擎。
- **当前代码已有基础：** [VerificationWorker.RunOnce](</Users/zxy/oncall agent/internal/diagnose/verification_worker.go:81>)、[ClaimVerificationTask](</Users/zxy/oncall agent/internal/store/verification.go:27>)、[FinalizeVerification](</Users/zxy/oncall agent/internal/store/verification.go:102>)、[到期索引](</Users/zxy/oncall agent/migrations/010_verify_task.sql:12>)。
- **当前缺口：** 启动循环每次只处理一个到期任务后等 ticker；在多个任务同时到期时存在额外排队空间。诊断 run 仍是整轮恢复，没有任意节点通用 checkpoint，本文不把后者设为必须建设。
- **为什么值得补：** 健康验证受时间窗口约束，调度延迟会消耗可用观测窗口；当前安全语义比“任意节点自动续跑”更值得保留。
- **补完后的简历亮点：** “基于持久化到期任务与领取标识优化验证调度，在保持原子终态和保守动作恢复的前提下，降低多任务同时到期时的排队延迟。”
- **适配目标公司：** 腾讯稳定性、Amazon 平台工程、阿里后台架构。
- **适配 JD：** Workflow 可靠性、任务调度、事务一致性。
- **设计草图：** 每次唤醒处理最多 B 个到期任务或至本批耗时预算结束 → 每个任务独立领取和单次检查 → 更新 `next_check_at` 或终态 → 暂无工作才等待。保持单实例和动作执行单独语义，不启动多个执行器。
- **伪代码位置：** 提取现有 `RunOnce` 的“处理一条”逻辑并返回是否有工作；外层 `for processed < batchLimit && withinBudget { processOneDue() }`，不用一次长事务锁整批。
- **验收指标：** 多任务同时到期的等待分布；每个任务均在明确预算内得到调度；旧 `claimed_at` 不可覆盖新结果；取消后可恢复；故障注入覆盖提交失败，验证副作用仍全有或全无。
- **风险和代价：** 大批次可能延迟取消响应或集中压迫健康端点；DB、上游与时间窗共同决定批次大小。原子提交和成员范围复核不能因“性能优化”被删除。
- **不能现在写成已实现的原因：** 当前仅单次消费一条；不存在经过基准验证的有界批次优化。

### 4.9 建议补齐 9：在现有 Tool 权限之上补请求资源范围与入口身份边界

- **当前状态：**【可扩展设计】；入口身份与跨团队范围在系统面向更多使用者时触发，动态工具回喂脱敏可独立优先补齐；**工具分级和动作审批已实现**。
- **建议补充模块：** `internal/api` 的统一 actor 入口、对话 `QuestionInput`、只读 Tool 参数范围校验及模型回喂边界；不立即引入通用 RBAC 平台或全库多租户字段。
- **当前代码已有基础：** [ForLLM](</Users/zxy/oncall agent/internal/tools/registry.go:110>) 限制工具等级；[Policy.Decide](</Users/zxy/oncall agent/internal/approval/policy.go:69>) 和 [Executor.execute](</Users/zxy/oncall agent/internal/approval/executor.go:130>) 校验变更目标；QuestionInput 已绑定 Incident。
- **当前缺口：** [Console.Actor](</Users/zxy/oncall agent/internal/api/dto.go:41>) 返回固定匿名身份；等级控制不等于用户身份授权，Incident 上下文绑定也不自动证明所有只读查询均受 Incident 资源范围限制。动态工具回喂缺少统一输出脱敏（见 3.4），多租户隔离没有完整实现证据。
- **为什么值得补：** 当一个实例面向多个团队或环境时，只读工具也可能访问不相关资源；权限必须由可信服务端上下文决定，不能由模型参数声称。
- **补完后的简历亮点：** 完成后可写“将工具等级权限扩展为可信调用者与资源范围校验，统一审计拒绝原因和实际查询范围”。若仅单团队受限部署，则不写企业多租户平台。
- **适配目标公司：** 腾讯平台安全治理、字节企业 AI 应用、Amazon 服务边界设计。
- **适配 JD：** AI 应用治理、平台权限；多租户 JD 仍需后续完整数据隔离证据。
- **设计草图：** 可信入口产生 actor/scope → 加载 Incident 范围 → 根据工具类型校验参数 → 执行 L1 查询 → 回喂前统一输出脱敏/限长 → 模型与审计使用明确的安全表示。PromQL 范围不能靠简单字符串拼接或正则保证；最小方案优先暴露已批准的查询模板，确需任意查询时再评估可靠解析策略。
- **伪代码位置：** `Questioner.agentTools` 适配层传入由服务端构建的不可变范围；执行前 `authorize(scope, tool, parsedArgs)`，拒绝也生成工具审计。Reasoner/Questioner 共用的 `executeLLMTool` 返回前复用一份输出卫生规则，避免仅在事后审计脱敏；工具错误文本也需覆盖。前端提交的 actor/tenant 字段不能作为身份依据。
- **验收指标：** 跨 Incident、跨容器、跨查询目标负例被拒绝；拒绝原因可审计；模型不能修改 actor/scope；用含测试凭据的假日志和工具错误断言模型收到的结果已脱敏；现有 L1–L4、PlanHash 和审批规则保持有效。若仍没有可信身份接入，只能声称资源范围校验，不能声称用户级授权已完成。
- **风险和代价：** 身份来源与组织权限是独立业务决策，未经真实需求不能为简历强行改造成多租户产品。现有匿名 Web 不是被遗漏的既有登录模块，不应恢复废弃代码充数。
- **不能现在写成已实现的原因：** 当前动作安全控制不覆盖完整的人员身份、团队资源授权和租户数据隔离；动态工具输出统一脱敏也尚未实现。

### 4.10 建议补齐 10：减少同 Incident 多观察者的 SSE 重复轮询

- **当前状态：**【可扩展设计】，第 3 项基准确认多浏览器重复读取明显后实施。
- **建议补充模块：** `internal/api/stream.go` 和事件提交后的进程内通知入口。
- **当前代码已有基础：** [StreamAPI](</Users/zxy/oncall agent/internal/api/stream.go:43>) 已有连接上限、生命周期、持久事件游标和重连续传。
- **当前缺口：** 每连接都有定时器，空闲时也每秒查一次库；当前没有同 Incident 共享唤醒或查询折叠。
- **为什么值得补：** 同一个控制室被多名值班人员打开时可减少重复空查询，且仍保持数据库为事实源；没有这类使用场景时保留当前实现最简单。
- **补完后的简历亮点：** “为事件控制台引入共享唤醒与持久游标补读，减少多观察者的重复轮询，同时保持断线恢复与事件顺序。”
- **适配目标公司：** Binance 实时后端、腾讯平台支撑、阿里运维控制台。
- **适配 JD：** 实时通知、后台 IO 优化、可观测控制台。
- **设计草图：** 事件事务提交成功 → 非阻塞进程内唤醒 → 对应 Incident 订阅者按各自游标补读 → 低频兜底轮询。通知丢失只增加延迟，不允许丢失持久事件；首次连接和重连始终读数据库。
- **伪代码位置：** 保留 `writeEvents`，替换固定每连接每秒唤醒为 `select { case <-incidentWake: readAfterCursor(); case <-fallbackTick: readAfterCursor() }`。通知注册/注销与请求 context 同生命周期，避免断线泄漏。
- **验收指标：** 比较 1/10/100 个观察者的 SQL 数和投递延迟；断网重连从 `Last-Event-ID` 补齐；通知故意丢弃仍由兜底读出；事务回滚不广播“已发生”事件；断开后订阅与 goroutine 回收。
- **风险和代价：** 增加订阅表和唤醒维护；内存通知不跨进程，当前单实例场景可接受，但不能宣称分布式事件总线。不要再新增独立 SSE 数据存储或消息中间件。
- **不能现在写成已实现的原因：** 当前 `serveHTTP` 为每连接建立固定 ticker，尚无共享唤醒。

## 5. 落地顺序、暂缓内容与成果门槛

### 5.1 最优先的三个工作包

| 顺序 | 工作包 | 对应十条建议 | 最小交付 | 停止条件 |
|---|---|---|---|---|
| 第一 | 离线效果基线与版本归档 | 1、2 | 固定样本、断言、版本元数据、失败报告 | 可复现并能解释效果差异；不建独立平台 |
| 第二 | 应用性能基线与必要指标 | 3 | 固定负载、阶段/排队/SQL/分配报告 | 能指出主瓶颈；不为指标数量堆系统 |
| 第三 | 收敛读取放大与快照语义 | 4 | 尾部查询、最新 run 查询、Target 复用及回归样本 | 正确性与访问上界有证据；无额外模型调用 |

第 5、6、8 项根据基准和真实失败继续推进。第 7、9、10 项需要明确使用场景。这里的分组是实施依赖，不是增加第十一条建议。

### 5.2 当前不建议为简历补的方向

| 方向 | 当前事实与取舍 | 合理的岗位表达 |
|---|---|---|
| RAG、文档解析/embedding/异步建索引 | 当前没有文档知识库链路；直接添加会改变项目产品范围、依赖和运维负担 | AI 应用/Agent 匹配较强，RAG 是另需真实项目证明的差距 |
| 多智能体平台、通用 DAG/DSL | 现有固定诊断和执行链路有清晰职责；先完善实际流程质量 | 写受控 Agent 工作流，不声称通用 Multi-Agent 编排平台 |
| Kafka/MQ、大规模集群 | 当前 SQL 队列和单实例恢复是显式设计；没有容量数据证明需替换 | 写持久队列和事务一致性，不写分布式消息平台 |
| 向量数据库、分布式存储引擎 | 当前是 MySQL 应用存储，优化重点在读取、索引、事务和恢复 | 可以写核心数据访问治理，不能主打分布式存储内核 |
| batching/continuous batching | 项目调用外部模型 API，未管理推理服务请求批处理 | 属于模型推理优化 JD 差距 |
| KV Cache/prefix cache/speculative decoding | 模型客户端缓存不等于模型前缀、注意力缓存或推测解码 | 写客户端复用和模型调用治理 |
| INT4/INT8/FP8、Sparse Attention、MoE routing | 无模型权重、量化流程或专家调度代码 | 不写已实现，不建议在 OnCall 应用中强行加 GPU 栈 |
| RDMA/NCCL/通信 overlap/CUDA/Triton | 没有训练/推理集群通信或算子实现 | 不主打通信、计算或模型性能架构师 |

以上暂缓项不是“本项目必须补齐”的要求。若未来岗位目标确实转向模型内核或 RAG，需要独立的真实需求、代码和实验，不能用当前 API 调用经历代替。

### 5.3 从方案变成简历成果的门槛

每条优化至少要有：修改前可定位的行为、单一明确改动、与风险匹配的测试、同条件实验或正确性证据、取舍和未覆盖边界。具体数值只能来自保存的报告。

简历用词分三步升级：

1. 只有设计：写“针对某链路提出某方案”，放在扩展思路中。
2. 已实现并验证机制：写“实现某机制，解决某类重复读取/恢复/边界问题”。
3. 有可复现测量：写“在某负载与环境下，将某指标从 A 改善到 B”，同时保留样本量和报告供面试解释。

不能把架构潜力、代码注释、迁移文件、测试名称或供应商承诺转换成已经发生的业务收益。公司映射只按用户提供的参考能力模型组织；团队规模、负责人身份、跨团队影响、用户覆盖和线上运营成果仍需本人补充事实。

## 6. 本文校验与证据限制

本次执行了源码静态读取、关键符号定位和文档引用/层级检查；没有修改业务代码、安装依赖、运行真实模型或外部数据库。针对最新 run 选择、读放大、串行等待和 SSE 查询次数，本文给出的是可定位的静态行为与复现实验设计，不是已经完成的性能测试或线上问题定性。

文档中的数字分为源码上限（如每页 100、最近消息 20、证据 2048 rune）、设计样例（如测试样本数和并行起点）以及推导示例。只有第一类代表当前配置/实现；设计值和推导值均不能当成已取得的项目指标。
