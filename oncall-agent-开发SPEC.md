# Oncall Agent 开发 SPEC（按人日拆分）

> 上游文档：`oncall-agent-重构方案.md`（下称"方案"）。本文把方案 §10 的 5 个里程碑拆成 **14 个人日粒度的小需求**（D01-D14），每个人日 = 一个可独立验收、可独立 commit 的交付单元。
> 节奏映射：按方案 §10.1（每周 ≈ 3 人日），W1=D01-03，W2=D04-06，W3=D07-09，W4=D10-12，W5=D13-14+缓冲。
> 压缩空间：顺利时 D06 可并入 D07、D10 可并入 D11，下限 12 人日。
> 参考实现：每个人日带一行"**抄**"，指向文末**附录 A 抄法手册**的条目与 `refs/` 下两个蓝本仓库的具体 `file:line`（keep@ae402b48、aiops-multi-agent@0dac9bb6，克隆见 D0）。先读参考再动手。

## 全局约定（每个人日的 DoD）

1. `go build ./... && go vet ./...` 通过；
2. 当日"验收"栏全部满足——验收是命令和断言，不是自我感觉；
3. 当日一个 commit（消息带 `D0x:` 前缀），未完成不 commit 半成品到 main；
4. 方案 §7 硬性纪律（禁 Fatal/panic、连接进程级复用、密钥不进 prompt、循环硬上限）适用于每一天；
5. 遇到范围膨胀：砍的是当日范围（回本文档标注），不是验收标准。

## 前置准备（D0，不计人日，开工前一次性）

- [ ] 克隆参考仓库（本文与方案的行号锚定这两个 commit，HEAD 漂移时按哈希 checkout）：
  `git clone --depth 1 https://github.com/keephq/keep refs/keep`（@ae402b48）
  `git clone --depth 1 https://github.com/mumulizi/aiops-multi-agent refs/aiops-multi-agent`（@0dac9bb6）
- [ ] 本地 docker：MySQL 8、Prometheus、Alertmanager、node_exporter（可复用方案 §9 的 alertmanager.yml 片段；node_exporter 是 D07 黄金指标有真数据的前提）
- [ ] IM 群机器人 webhook 一枚（企微/飞书均可），记入环境变量 `IM_WEBHOOK`
- [ ] 生成一枚随机 `AUTH_TOKEN` 记入环境变量（webhook 与审批接口的 Bearer 鉴权用）
- [ ] 火山方舟 API key 可用性验证——curl 两次：chat/completions 一次，**带 `tools` 的 function-calling 往返一次**（D08 Eino ReAct 的硬前提，光 chat 通不算过）
- [ ] 造一条会持续 firing 的测试告警规则（如 `vector(1)` 表达式），作为全程联调素材

---

## M0 摄入与去重（D01-D03）

### D01 仓库奠基
**目标**：空仓库 → 可编译骨架 + 全部建表。
**任务**：
- `git init`；`go mod init oncall-agent`；按方案 §4 建目录（空包允许，占位文件不允许有假逻辑）
- `internal/config`：加载 `config.yaml` + `${ENV}` 展开 + 缺失必填项启动即失败（fail-fast）
- `migrations/001_init.sql`：方案 §5 全部 10 张表
- `internal/store`：GORM model（10 表映射）+ `Open(dsn)`；migration 用 `mysql < migrations/*.sql` 手动执行，不引 migrate 框架
- `.gitignore`、`config.example.yaml`、`docker-compose.dev.yml`（mysql+prometheus+alertmanager+node_exporter）
**产出**：可编译仓库、10 张表、开发环境一键起。
**抄**：表结构语义对照 → 附录 A1（AlertRaw）、A3（LastAlert）、A4（Incident）、A13（run/step 审计）。
**验收**：`go build ./...` 过；`mysql ... < migrations/001_init.sql` 后 `SHOW TABLES` = 10；`docker compose up -d` 四容器健康；config 缺 `MYSQL_DSN` 时进程启动报错退出（fail-fast 证明）。

### D02 归一化与指纹（纯函数日）
**目标**：Alertmanager payload → `NormalizedAlert`，指纹/哈希/severity 全部纯函数 + 单测。
**任务**（全部在 `internal/ingest`，不碰 DB）：
- `ParseWebhook([]byte) ([]NormalizedAlert, error)`：Alertmanager v4 格式（status/labels/annotations/startsAt/endsAt/generatorURL）
- `Fingerprint(labels, fields []string) string`：sha256；`fields` 空 = 全部 labels 按 key 排序后拼接（方案 D3）
- `FullHash(a) string`：md5，排除 `starts_at/ends_at/received_at`
- severity 映射：`labels[severity_label]` → 5..1，未知默认 3（warning）
**产出**：`ingest` 包 + table-driven 测试。
**抄**：附录 A2（alert_hash 算法与"比较对象是上一条"语义）；Alertmanager 格式解析对照 `refs/keep/keep/providers/prometheus_provider/prometheus_provider.py:172-230`（`_format_alert`：labels 小写化、status/severity 映射表）。
**验收**：`go test ./internal/ingest` 绿，用例必须覆盖：label 顺序无关性、firing/resolved 同指纹、未知 severity 默认值、labels 缺失、非法 payload 报错不 panic。

### D03 webhook → 落库 → 去重 worker + simulate
**目标**：M0 端到端跑通。
**任务**：
- `POST /webhook/alertmanager`：校验 `Authorization: Bearer ${AUTH_TOKEN}`，原文落 `raw_event(pending)` 后立即 202（keep `alerts.py:602-655` 语义）
- worker：进程内 channel 消费 + **启动时扫描 pending 补账**；两级去重——full dup 丢弃仅计数（日志）但**仍刷新 last_seen**（方案 D3：防 repeat_interval 断窗），partial 同指纹 `last_alert` upsert（`firing_count++`、status 翻转、时间刷新），新指纹插 `alert` + `last_alert`
- `cmd/simulate`：`-n` 条数、`-dup` 重复率、`-resolved` 发解除批次，严格 Alertmanager v4 格式；`generatorURL` 必须是指向本地 Prometheus 的合法 expr（如 `g0.expr=vector(1)`，D07 回放依赖它）
**产出**：摄入链路 + 造数工具。
**抄**：附录 A1（落库先行+202）、A2（两级去重判定）、A3（last_alert 同事务维护）、A12（simulate 参数设计）。
**验收**（= 方案 M0 验收）：`simulate -n 100 -dup 0.6` → `last_alert` 行数=去重后指纹数、`firing_count` 正确；`-resolved` → status 翻转；处理中 `kill -9` 重启 → pending 补账无丢失；带错 token 的请求 401；打 tag `v0.1-m0`。

---

## M1 聚合与分流（D04-D05）

### D04 Correlator
**目标**：告警归并进 incident。
**任务**：
- `GroupKey(alert, cfg)` 纯函数：按 `correlate.group_by` 取 label 值拼接，缺失 label 时回退 `name` 前缀
- `Correlator.Assign`：查 `idx_group_open` 上开放（candidate/firing）且 `last_seen_at` 在窗口内的 incident → 归入（`incident_alert` 插入、`alerts_count++`、`severity=max(成员)`、刷新 `last_seen_at`）；否则新建 `candidate`，`title = "{group_key}: {首条告警 name}"`（代码生成）
- 阈值判定：`alerts_count >= min_alerts` → candidate 转 firing，返回"促发"事件给上层
**产出**：`internal/ingest/correlate.go` + 单测（窗口内/外、阈值促发、severity 取 max、group label 缺失回退）。
**抄**：附录 A4（rule_fingerprint≈group_key、候选/阈值、开放 incident 查找）。
**验收**：单测绿；`simulate` 打 3 条同 service 告警 → DB 恰好 1 个 incident 且 `alerts_count=3`。

### D05 incident 状态机 + severity 分流
**目标**：incident 生命周期闭合，诊断入口就位（还不接 LLM）。
**任务**：
- resolved 传播：成员 alert 全部 resolved → incident 自动 resolved（方案 D4，resolve_on=ALL）
- 促发时按 `diagnose.severity_route` 落 `agent_run(mode, status=pending)`；skip 直接落 succeeded（也落库，供统计）。**agent_run 表即诊断队列**：诊断与摄入分属两个 worker（方案 D1/§7 修订），本日只落队列不消费
- `GET /api/v1/incidents?status=`、`GET /api/v1/incidents/{id}`（含成员告警）查询接口；`POST /api/v1/incidents/{id}/diagnose` 手动重诊兜底（severity 升级/失败 run 后人工触发，落一行 pending）
**产出**：incident 包 + 查询 API。
**抄**：附录 A4（resolved 传播 `is_all_alerts_resolved`）；severity 分流对照 `refs/aiops-multi-agent/graph.py:49-53`。
**验收**（= 方案 M1 验收）：窗口内 3 条 → 1 incident；窗口外 → 新 incident；全 resolved → 自动 resolved；low 告警 → `agent_run.mode=skip`；打 tag `v0.2-m1`。

---

## M2 诊断流水线（D06-D09）

### D06 工具注册表 + Prometheus 三件
**目标**：安全工具层地基 + 主力查询工具。
**任务**：
- `internal/tools`：`ToolSpec{Name,Description,Level,Timeout,MaxOutput,Handler}`、`Registry.Register/ForLLM()`（只导出 L1，适配成 Eino `tool.BaseTool`）、超时 wrapper、输出截断 wrapper
- `prom_instant_query{query,time?}`、`prom_range_query{query,start,end}`（step 自适应、`max_points` 裁剪）、`prom_series_meta{match[],limit}`；http client 进程级复用
**产出**：registry + 三个工具。
**抄**：注册表/分级是本项目自己的设计，参考两处思想——`refs/aiops-multi-agent/tools/remediation_actions.py:695-727`（L2 灰名单 + `run_action` 统一入口白名单校验，"不在名单即拒绝"）；`tools/safety_guards.py:38`（同 target+action 每小时限频，v1 可选后置）。
**验收**：对本地 Prometheus 各真调一次返回正常 JSON；构造超大响应 → 截断到 `MaxOutput`；构造慢查询 → 超时报错而非挂死（单测覆盖截断与超时）。

### D07 EvidenceCollector
**目标**：诊断前置证据全部由代码收集（方案 D2，0 次 LLM）。
**任务**（`internal/diagnose`）：
- `AlertSnapshot`：incident 全部成员的 last_alert + annotations
- `PromReplay`：解析 `generatorURL` 里的 expr，firing 时刻 ±15min range 回放
- `GoldenMetrics`：按 instance/job 套写死的 PromQL 模板（CPU/内存/磁盘/网络）
- `Evidence.Render()` 生成 prompt 文本，每个 collector 独立截断预算；单 collector 失败记 step 继续（容错）
**产出**：三个 collector + Evidence 渲染。
**抄**：附录 A5（0 次 LLM 的证据收集哲学 + aiops 踩过的"inspector 里塞 LLM 纯浪费"坑）。
**验收**：对 simulate 促发的 incident，`GET /debug/evidence/{id}`（临时调试端点）输出包含三段证据的文本；停掉 Prometheus 再请求 → 缺该段但不报 500。

### D08 LLM 工厂 + Reasoner
**目标**：Eino 推理节点就位。
**任务**（`internal/llm`）：
- `Build(role)`：角色级配置（reasoner/summarizer），进程内缓存，启动时校验配置完整
- `Reasoner.Diagnose(ctx, evidence, mode) (rca, plan, stepLogs, err)`：Eino `react.Agent`，MaxStep full=8/light=3，只挂 `Registry.ForLLM()`；系统 prompt 模板：角色设定 + 输出 JSON 契约 `Plan{action,target,reason,confidence}` + "先 series_meta 后写 PromQL"指令
- Plan 解析容错（```json 剥壳），解析失败重试 1 次后放弃报错
- 每次调用把 usage（prompt/completion tokens）累计回 `agent_run.tokens_in/tokens_out`（Eino callback 或响应 usage 字段）——D13 的 `tokens_in=0` 断言以此为据
**产出**：可独立调用的推理单元。
**抄**：附录 A6（步数上限外置、JSON 契约剥壳、角色化模型工厂三级配置）。
**验收**：真调一次：喂 D07 的 Evidence → 返回非空 RCA + 合法 Plan JSON，且落库后 `tokens_in > 0`；mode=light 时工具调用次数 ≤3（从 stepLogs 断言）。

### D09 Pipeline 串联 + guard + IM 报告
**目标**：M2 端到端。
**任务**：
- 诊断 worker：独立 goroutine 消费 `agent_run(status=pending)`，启动时扫描 pending/超时 running 补账（对齐 raw_event 补账语义）
- `Pipeline.Run`：mode 判定 → evidence → reason → guard → 报告，每阶段 `step()` 落 `agent_run_step`；任何阶段失败 run 标 failed，进程不死
- guard 首版 2-3 条（方案 D5）：RCA 命中"配置错误/镜像不存在"类关键词 → 强制 escalate；plan 无 target → 强制 none
- `internal/notify`：IM webhook（markdown 卡片：RCA/证据摘要/建议/run 链接）
- 轻工具补齐：`mysql_select`（语句类型校验 + 强制 LIMIT）、`get_runbook`（新增 `migrations/002_runbook.sql`：id/title/content/FULLTEXT 索引）——**本日可砍项**，砍则顺延到 W5 缓冲
**产出**：完整诊断链路。
**抄**：附录 A7（guard 规则原型，R3 就是我们首版关键词规则的出处）、A8（IM 通知抽象与卡片文案模板）、A13（step 审计粒度）。
**验收**（= 方案 M2 验收）：simulate 触发 critical → IM 收到真报告；`agent_run_step` 可完整回放；断 Prometheus → run failed 进程存活；guard 命中写入 `kind=guard` step；诊断进行中 simulate 再打一批告警 → 摄入不被阻塞（两 worker 分离证明）；打 tag `v0.3-m2`。

---

## M3 审批执行闭环（D10-D12）

### D10 审批单生命周期
**目标**：L2 动作的申请-决策通道。
**任务**（`internal/approval`）：
- plan 含 L2 动作 → 落 `approval(pending, run_id)` + IM 审批卡片（附 approve/deny 的 curl 命令，curl 自带 Bearer token）
- `POST /api/v1/approvals/{id}/approve|deny`：要求 `Authorization: Bearer ${AUTH_TOKEN}`；幂等，已决返回 409
- 过期 worker：超 `ttl_minutes` → expired（TTL 可配置，测试环境设 1min）
**抄**：附录 A9（状态机幂等 UPDATE 写法；aiops 是惰性过期，我们的主动过期 worker 是增强）。
**验收**：创建 → IM 收到卡片 → approve 状态翻转；重复 approve 409；无 token 401；不处理 → 到期自动 expired。

### D11 L2 执行 + verify
**目标**：批准的动作真执行、结果可验证。
**任务**：
- L2 样板工具一个：`exec_change_sql`（走审批的 UPDATE/DELETE，独立读写账号 DSN 来自配置）
- 执行 worker：approved → 调 handler → `result_json` 回写 → executed/failed
- verify 阶段：执行后延时复查 **`last_alert` 状态**（v1 定死这一种——配合 `simulate -resolved` 可控演示成功/失败两条路径；PromQL 复测告警表达式留 v2）→ `agent_run_step(kind=verify)`，run_id 取自 approval.run_id
**抄**：附录 A9（执行 worker：分派→回写→写命令历史）、A10（验证判定的写法）。
**验收**：审批通过 → SQL 真执行且结果回写 approval 行；verify 结果落 step；执行失败 → status=failed 不静默。

### D12 重诊闭环
**目标**：M3 收口。
**任务**：
- verify 失败且 `retry_of` 链 <2 → 新建 `agent_run(retry_of=上次)`，prompt 注入 `last_failed_plan + failure_reason`（方案 D7）
- 链 ≥2 → IM 标红升级人工，run 终态 failed
**抄**：附录 A10（重诊路由判定 + 重诊 prompt 的注入文案模板可直接翻译）。
**验收**（= 方案 M3 验收）：全流程演练一遍含"验证失败自动重诊一次、第二次失败升级人工"；30min（测试 1min）不批 → expired；打 tag `v0.4-m3`。

---

## M4 记忆闭环（D13-D14）

### D13 fault_memory 读写（方案 D8 门槛 1/2）
**任务**（`internal/memory`）：
- `FaultFingerprint(groupKey, alertName)` 纯函数 + 单测——**RCA 不进 key**（Lookup 发生在诊断前，算不出 RCA；aiops 的第三分量是诊断前的稳定摘要，见附录 A11 坑 1）
- `Store.Lookup`：high + TTL 内才命中，命中 `hits++`、刷新 `last_used`
- `Store.Commit`：仅 verify 通过且 confidence=high 时由 Pipeline 调用；guard 改写过或重诊成功的 case 降 medium 不入库
- Pipeline 接入：mode 判定后先 Lookup（**重诊 run 不查**，防坏记忆循环命中），命中 → `mode=memory_hit`，复用 plan 直接进执行+验证，0 次 LLM；复用 plan 含 L2 动作仍走审批，不提权
**抄**：附录 A11（lookup/commit 门槛逐行对照 + 两个关键坑）。
**验收**：同指纹故障第二次触发 → `agent_run.mode=memory_hit`、`tokens_in=0`；IM 报告标注"记忆命中(hits=N)"。

### D14 降级回收 + 命令历史 + v1.0 收尾（方案 D8 门槛 3/4）
**任务**：
- `Store.Demote`：memory_hit 的 plan 验证失败 → confidence 降 low（拉黑）→ 自动转完整重诊
- `Store.RecordCmd`：L2 executed 后按故障指纹写 `fault_cmd_history`；`RecentCmds(fp, 5)` 在记忆 miss 但历史存在时注入 prompt（执行仍按 L1/L2 分级）
- 收尾：README（quickstart：docker compose + simulate 三步跑通）、config.example 与实现比对校准
**抄**：附录 A11（命令历史三段：DDL/写入/读取注入；Demote 是对 aiops 人工 forget 的自动化增强）。
**验收**（= 方案 M4 验收）：三条全过（memory_hit 0 LLM / 降级路径 / step 里可见历史命令注入）；README 步骤在干净环境复现成功；打 tag `v1.0`。

---

## 可砍与顺延规则（对齐方案 §10.1 执行规则 3/4）

| 卡住的日 | 先砍什么 |
|---|---|
| D06/D07 | 合并为一日：series_meta 后置、GoldenMetrics 只留 CPU/内存两条模板 |
| D09 | `mysql_select`/`get_runbook` 整体后置到 W5 缓冲 |
| D10/D11 | 合并为一日：deny/expired 后置，只走 approve 主路径（补回时间：W5 缓冲） |
| D14 | `RecentCmds` 注入后置，Demote 不可砍（安全属性） |

砍完必须回本文档在对应日标注"已砍 + 补回计划"。

---

## 附录 A：参考实现抄法手册（按机制）

两个蓝本仓库浅克隆在项目根 `refs/` 下（D0 完成），**本文全部行号锚定这两个 commit**，仓库 HEAD 漂移时按哈希 checkout：

```bash
git clone --depth 1 https://github.com/keephq/keep refs/keep && git -C refs/keep checkout ae402b48
git clone --depth 1 https://github.com/mumulizi/aiops-multi-agent refs/aiops-multi-agent && git -C refs/aiops-multi-agent checkout 0dac9bb6
```

**读法约定**：keep 是多租户 SaaS（Python/SQLModel/FastAPI），只读它的**表结构与数据平面语义**，tenant_id/enrichment/facet/preset 一律无视；aiops 是 K8s 场景单机脚本（Python/LangGraph/SQLite），只读它的**控制平面流程与门槛规则**，K8s 名词按 namespace→group_key、pod→instance 换算。两边都不抄代码——抄语义、抄门槛数值、抄它们踩过的坑。下文路径省略 `refs/` 前缀。

### A1 raw_event 落库先行 + 202（D03）
- **看**：`keep/keep/api/routes/alerts.py:602-655`（`receive_generic_event`：入队后立刻 202）；后台主流程 `keep/keep/api/tasks/process_event_task.py:662`（`process_event`）、原文落库 `:148` 与 `:881`（AlertRaw 对象）；表定义 `keep/keep/api/models/db/alert.py:291`。
- **机制**：HTTP handler 只做两件事——原文落 AlertRaw、把处理任务丢进池子（keep 用 ThreadPoolExecutor，`alerts.py:89`）。解析/去重/关联全在后台；崩溃恢复靠 raw 表重放。
- **Go 落法**：线程池 → 进程内 channel + 单消费 goroutine；补账 = 启动时 `SELECT ... WHERE status='pending'` 重新入队（keep 靠任务队列重试机制，我们扫表，更简单且够用）。

### A2 两级去重（D02/D03）
- **看**：`keep/keep/api/alert_deduplicator/alert_deduplicator.py:45-116`（`_apply_deduplication_rule`：`:63` 循环删 ignore_fields、`:67` 对剩余字段整体 sha256、`:80` 起判 full、`:98` 判 partial）；入口 `:118-178`（`apply_deduplication`）；默认规则 `:260-280`（只 ignore `lastReceived`）。
- **机制**：`alert_hash = sha256(去掉 ignore_fields 后的整个 event)`。hash 等于**该指纹上一条**的 hash → full dup（丢弃只计数）；仅 fingerprint 相同 → partial（更新 last_alert）。"上一条"从 LastAlert 查，绝不扫 alert 历史。
- **Go 落法**：md5/sha256 无所谓，抄两点就够——①比较对象是 last_alert 里存的上一条 hash，不是历史任意一条；②ignore 字段可配置（方案 §9 `full_dedup_ignore`）。注意 keep 的 fingerprint 由 provider 侧给出，我们自算 sha256(labels 排序拼接)对齐 Alertmanager 语义。

### A3 last_alert 快照表（D03）
- **看**：模型 `keep/keep/api/models/db/alert.py:43-64`（LastAlert：fingerprint 主键 + alert_hash 列）；`:126-129` 的注释就是设计精髓——"firing 和 resolved 指纹相同但 alert_hash 不同"；维护 `keep/keep/api/core/db.py:5708`（`set_last_alert`，每次新 alert 落库后 upsert）；读路径 `db.py:1755`（`get_last_alerts`）。
- **机制**：alert 表 append-only 永不 UPDATE，"当前状态"全部由 last_alert 承载；resolved 到达 = 同指纹 partial 更新，status 翻转。
- **Go 落法**："插 alert → upsert last_alert" 必须同一个事务；`firing_count++` 只在 partial 时做。

### A4 incident 候选/阈值/自动 resolve（D04/D05）
- **看**：`keep/keep/api/models/db/incident.py:78-131`（`is_candidate:107`、`alerts_count:110`、`rule_fingerprint:123`、`resolve_on:131` 默认 ALL）与状态枚举 `:51-76`；关联引擎 `keep/keep/rulesengine/rulesengine.py:73-178`（`:116` 算 rule_fingerprint、`:157` require_approve 决定是否候选）、`:249-352`（`_get_or_create_incident`：查同 rule_fingerprint 的开放 incident，没有才建）、`:566-600`（`_calc_rule_fingerprint`：分组字段取值拼接，≈ 我们的 group_key）；resolved 传播 `keep/keep/api/core/db.py:5306`（`is_all_alerts_resolved`）。
- **机制**：group_key 相同且 incident 开放 → 归入，否则新建候选；候选达到阈值才可见/促发；alert 状态变化时检查"成员是否全部 resolved"。
- **Go 落法**：CEL 引擎整个不要，group_key 直接 `join(labels[...])`；候选语义我们用 status 枚举（candidate/firing）而不是 keep 的 `is_candidate` bool——少一个字段，状态机更清晰。

### A5 证据收集 0 次 LLM（D07）
- **看**：`aiops-multi-agent/agents/inspector.py` 整个文件，重点是头部注释：v2.7 把原有的两处 LLM 调用（Top5 深入预览、整体摘要）全删了，"LLM 推理全部留给 Investigator"；纯代码 severity 分级在 `:23-63`（含"不会自愈的卡死状态即使 restarts=0 也要提级"的经验规则）。
- **机制**：诊断前的现场快照全部由代码直接调 API 收集（它调 K8s API，我们调 Prometheus HTTP API），LLM 一次都不碰。
- **Go 落法**：EvidenceCollector 三件（AlertSnapshot/PromReplay/GoldenMetrics）对应它的 overview+collect 两步；每个 collector 独立容错、独立截断预算是我们比它多做的（它是单机脚本，挂了就挂了）。

### A6 ReAct 推理节点（D08）
- **看**：`aiops-multi-agent/agents/investigator.py:569-660`（`investigator_node`：`:575` light 模式 3 步上限、`:548-559` 工具调用循环与结果截断——result 2000 字、回填 prompt 3000 字）；模型工厂 `aiops-multi-agent/tools/llm_factory.py:41-73`（`_resolve`：角色专属 env > 全局 > 默认 三级解析）。
- **机制**：aiops 手写 ReAct 循环（JSON action 协议 + `_extract_json` 剥壳容错）。我们用 Eino `react.Agent` 替代循环本身，但要自己抄三样：①步数上限外置配置；②输出 JSON 契约 + 剥壳（`_extract_json` 同款容错）；③角色化模型工厂（三级优先搬进 config.yaml 的 `llm.roles`）。
- **注意**：它给工具结果设了截断预算——这个纪律由我们的 D06 `MaxOutput` wrapper 承担，别在 prompt 层再做一遍。

### A7 guard 规则纠偏（D09）
- **看**：`aiops-multi-agent/agents/remediator.py:163-254`（`_post_process_plan` 全文：`:181-184` 清 "action=" 前缀、`:197-202` target 校正回真实值、`:204-211` R1 无控制器禁动、`:213-229` R2 强制升级 L2 人审、`:231-252` R3 "重启无救"→ 强制 none + escalate_human）；黑名单关键词与判定 `:257-320`（`_is_non_restartable_failure:307`）。
- **机制**：**代码优先于 LLM**——已知幻觉模式硬改写，且把改写原因写回 plan（`_overridden` 字段），通知层能看到"这条被规则改过"。D09 首版的"配置错误/镜像不存在 → 禁 restart 改 escalate"就是 R3 的直译。
- **Go 落法**：`[]GuardRule`，每条 = 谓词 + 改写 + 原因文本；改写原因落 `agent_run_step(kind=guard)`（对应 `_overridden`）。

### A8 IM 通知抽象（D09/D10）
- **看**：`aiops-multi-agent/tools/im_notify.py:78-121`（`send_message(text)` 唯一入口，上层永远只拼文本）；`:31-63`（provider→payload 构造函数表：wecom/feishu/dingtalk 各一个小函数）；`:225`（`format_approval_message`：审批卡片文案模板，结构直接抄）；`:69`（`_write_local_alert`：webhook 失败落本地文件兜底）。
- **Go 落法**：`notify.Notifier` 接口 + wecom/feishu 两实现；发送失败至少 error 日志不吞（本地文件兜底可选）。

### A9 审批单生命周期（D10/D11）
- **看**：`aiops-multi-agent/tools/approval_store.py:70-98`（`create_pending`）、`:185-209`（`mark_approved`：`:197` 非 pending 直接拒绝=幂等、`:199-200` 过期惰性标记）、`:27`（TTL 默认 1800s）；执行侧 `aiops-multi-agent/agents/approval_exec_worker.py:148-220`（approved → 按 kind 分派执行 → 结果回写 → `:200-219` 写命令历史）。
- **机制**：审批就是一行状态机记录 + 带条件的 UPDATE。aiops 的过期是**惰性**的（approve 时才检查标 expired）；我们加主动过期 worker 是增强——IM 上能看到"已过期"而不是永远 pending。
- **Go 落法**：`mark_approved` 的"SELECT 状态 → 条件 UPDATE"合成单条 `UPDATE ... SET status='approved' WHERE id=? AND status='pending'` 看 RowsAffected，天然幂等，已决返回 409。

### A10 验证 + 失败重诊闭环（D11/D12）
- **看**：`aiops-multi-agent/agents/validator.py:288-317`（执行后延时复查快照，按差值判 success/partial/failed/pending 四态）、`:344-349`（failed → 存 `last_failed_plan`/`last_failure_reason`）；路由 `aiops-multi-agent/graph.py:42-47`（重试上限=2，一个故障最多 3 次诊断）与 `:56-72`（`_route_after_validator`：failed && retry<上限 → 回 Investigator，否则通知）；重诊 prompt 注入 `agents/investigator.py:632-649`（"上次 action/target/失败原因 + 请换角度"的文案模板，直接翻译成中文 prompt 用）。
- **Go 落法**：它的 state dict 字段 → 我们的 `agent_run(retry_of)` 链；它验证看 Pod restarts 差值，我们看 `last_alert` 状态（D11 已定死）。

### A11 fault_memory 全套（D13/D14）
- **看**：`aiops-multi-agent/tools/fault_memory.py:76-86`（`generate_fingerprint`）、`:89-136`（`lookup`：TTL 从 **last_success** 起算 `:114-117`、置信度过滤 `:119-121`）、`:139-176`（`record_success`：upsert 保留 hits）、`:179-194`（`record_hit`）、`:218`（`forget`，人工拉黑）；写入门槛在 `agents/validator.py:322-342`（**success && !from_memory** 才写；置信度从 RCA 文本解析）；召回时机 `agents/investigator.py:577-602`。
- **两个关键坑（本项目文档已修正，读代码时对照）**：
  1. **指纹签名必须在诊断前可得。** aiops 的第三分量是 event_summary 而非 RCA——`investigator.py:584` 原话："用 summary 当 RCA 签名（此刻还没诊断，没 RCA），这个签名要稳定"。方案早期版本误抄成 RCA 进指纹，会导致 Lookup 永远 miss；现已改为 `md5(group_key + alertname)[:12]`。
  2. **重诊不查记忆。** `investigator.py:578` 的 `if retry_count == 0` 门 + `:595` 命中后置 retry_count=1——否则 memory_hit 失败转重诊会再次命中同一条坏记忆死循环。我们的 Demote（命中失败即降 low）从根上解决；aiops 只有人工 `forget`，自动 Demote 是我们的增强。
- **命令历史三段**：DDL `fault_memory.py:258-281`；写入 `:284-315`（stdout/stderr 各截 4KB 防膨胀）；读取注入 `:318-352` + `investigator.py:604-614`（miss 但有历史 → 最近 5 条进 prompt，"曾审批执行过这些命令及结果"）。

### A12 simulate 造数（D03）
- **看**：`keep/scripts/simulate_alerts.py`（argparse 参数设计 `:23-38`：`--num`/`--full-demo`/`--rps`/`--workers`）。
- **Go 落法**：只抄"造数工具与生产走同一入口、同一格式、重复率可控"的思想；payload 不照抄（keep 面向多 provider），严格按我们的 Alertmanager v4 格式造。

### A13 agent_run/step 审计（D09）
- **看**：`keep/keep/api/models/db/workflow.py:77`（WorkflowExecution：status/started/execution_time/results/error）、`:197`（WorkflowExecutionLog：每步一行，message + context）。
- **Go 落法**：我们的 agent_run/agent_run_step 就是这两张表砍掉 workflow 概念后的同构物；step 的 input/output 存 JSON 且沿用工具层 `MaxOutput` 截断预算，别存全量大文本。
