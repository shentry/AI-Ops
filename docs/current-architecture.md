# 当前系统架构

> 本文按当前源码、数据库迁移和运行配置整理。源码是事实基准；部分早期设计文档仍描述“诊断、审批、执行尚未实现”，不应作为当前运行状态依据。
>
> 系统形态：**模块化单体 + MySQL 持久队列/审计 + React 嵌入式控制台**。

## 1. 架构结论

- 一个 `oncall-agent` Go 进程同时承载 HTTP、静态前端、SSE 和 5 类后台 worker。
- MySQL 是 AI-Opus 的唯一权威业务库，也是摄入、诊断、审批执行的持久队列。
- Prometheus、Alertmanager、blackbox-exporter 属于观测与告警侧。
- PostgreSQL、Redis、Sub2API 属于被监控目标及其依赖，不是 AI-Opus 自身的存储。
- React 控制台构建后通过 `go:embed` 嵌入 Go 服务，同源提供。
- LLM 只负责基于证据生成 RCA/Plan 或回答只读问题；变更动作必须经过 Guard、Policy、Approval、Executor、Verify。
- 控制台是**显式的匿名公开操作面**：没有登录、会话或 CSRF，`api.Console` 是唯一的身份来源，控制台上的所有读写一律记为 `anonymous`。曾经存在但从未接入路由的 OAuth/Session 实现已于 2026-08-24 删除。

## 2. 运行时部署拓扑

```mermaid
flowchart LR
    subgraph target["被监控目标：Sub2API 测试环境"]
        GW["Sub2API 网关<br/>HTTP /health"]
        PG[("PostgreSQL<br/>业务数据")]
        RD[("Redis<br/>缓存/队列依赖")]
        UP["上游 AI Provider"]

        GW --> PG
        GW --> RD
        GW --> UP
    end

    subgraph compose["Docker Compose：观测与基础设施"]
        NODE["node-exporter<br/>:9100"]
        BB["blackbox-exporter<br/>:9115"]
        PROM["Prometheus<br/>:9090<br/>5s scrape/evaluation"]
        AM["Alertmanager<br/>:9093<br/>分组、抑制、重复推送"]
        MYSQL[("MySQL 8<br/>:3306<br/>AI-Opus 权威库")]

        NODE --> PROM
        BB --> PROM
        PROM --> AM
    end

    BB -.->|"HTTP probe<br/>Sub2API /health"| GW
    BB -.->|"TCP probe<br/>PostgreSQL 隧道"| PG
    BB -.->|"TCP probe<br/>Redis 隧道"| RD

    subgraph mono["宿主机：oncall-agent 模块化单体"]
        HTTP["GoFrame HTTP 层<br/>API / SSE / /metrics<br/>当前端口：18080"]
        STATIC["web/embed.go<br/>嵌入 web/dist"]
        IW["ingest.Worker<br/>raw_event 消费"]
        DW["diagnose.Worker<br/>agent_run 消费"]
        EW["approval.ExpiryWorker<br/>审批 TTL 扫描"]
        EX["approval.Executor<br/>approved 审批执行"]
        CW["conversation.Worker<br/>对话队列消费"]
        REG["tools.Registry<br/>Prometheus / Docker 工具"]
        NOTIFY["notify.Notifier<br/>Noop / Webhook / Feishu"]

        HTTP --> STATIC
        HTTP --> MYSQL
        IW --> MYSQL
        DW --> MYSQL
        EW --> MYSQL
        EX --> MYSQL
        CW --> MYSQL
        DW --> REG
        EX --> REG
        DW --> NOTIFY
        EX --> NOTIFY
    end

    BROWSER["浏览器控制台"] -->|"same-origin HTTP"| HTTP
    AM -->|"POST /webhook/alertmanager<br/>Bearer Token"| HTTP

    DW -.->|"证据：/health"| GW
    DW -.->|"证据：PostgreSQL"| PG
    DW -.->|"证据：Redis"| RD
    DW -.->|"PromQL 查询"| PROM

    DOCKER["Docker Engine<br/>Unix Socket"]
    LLM["OpenAI-compatible LLM<br/>配置化模型服务"]
    FEISHU["Feishu Open API<br/>可选 feishu_app"]

    REG -.->|"inspect / logs / restart"| DOCKER
    DW -.->|"Reasoner / Questioner"| LLM
    NOTIFY -.->|"卡片、消息、线程回复"| FEISHU
    FEISHU -.->|"事件回调 / 卡片动作"| HTTP
```

### 运行边界

| 部分 | 当前实现 |
|---|---|
| `oncall-agent` | 跑在宿主机，不在 `docker-compose.dev.yml` 中 |
| HTTP | GoFrame，当前配置端口为 `18080` |
| MySQL | Compose 容器，AI-Opus 自身唯一业务数据库 |
| Prometheus | `:9090`，抓 node-exporter 和 blackbox |
| Alertmanager | `:9093`，向宿主机发送 Alertmanager webhook |
| blackbox | 探测目标 HTTP 健康状态和 PostgreSQL/Redis TCP 连通性 |
| PostgreSQL / Redis | 由 Sub2API 使用，AI-Opus 只读采集证据 |
| Docker | 通过 Unix socket 读取容器并执行受控动作；socket 缺失时能力降级 |
| LLM | OpenAI 兼容接口，凭据只来自服务端配置 |
| Feishu | 可选出向通知和入向回调 |

依据：`docker-compose.dev.yml`、`prometheus.yml`、`alertmanager.yml`、`blackbox.yml`、`config.yaml`、`cmd/server/main.go:42-239`。

## 3. 代码分层与模块依赖

```mermaid
flowchart TB
    subgraph entry["组合根与入口"]
        BOOT["cmd/server/main.go<br/>加载配置、连接 DB、组装依赖、启动 workers、绑定路由"]
        SIM["cmd/simulate/main.go<br/>生产格式 Alertmanager 测试注入"]
    end

    subgraph edge["边界层"]
        API["internal/api<br/>HTTP Handler、DTO、Bearer 校验、SSE、静态资源"]
        WEB["web/src<br/>React 控制台"]
    end

    subgraph workers["异步运行时"]
        INGW["internal/ingest.Worker<br/>raw_event"]
        DIAGW["internal/diagnose.Worker<br/>agent_run"]
        EXPW["internal/approval.ExpiryWorker<br/>审批 TTL"]
        EXECW["internal/approval.Executor<br/>approved approval"]
        CONVW["internal/conversation.Worker<br/>conversation_message"]
    end

    subgraph domain["业务控制面"]
        ING["internal/ingest<br/>解析、指纹、去重、归并"]
        INC["internal/incident<br/>严重度路由、run 初始化"]
        PIPE["internal/diagnose.Pipeline<br/>memory → evidence → LLM → Guard → Policy → notify"]
        VERIFY["internal/diagnose.Verifier<br/>独立恢复验证"]
        RETRY["internal/diagnose.RetryScheduler<br/>重诊与人工升级"]
        APPROVAL["internal/approval<br/>Policy、审批状态机、执行器"]
        MEMORY["internal/memory<br/>fault_memory 召回/写回/降级"]
        CONV["internal/conversation<br/>对话入队、上下文、回答持久化"]
    end

    subgraph adapters["适配器与安全工具"]
        LLM["internal/llm<br/>Factory、Reasoner、Questioner、ModelSwitcher"]
        TOOLS["internal/tools<br/>Registry、Prometheus、Docker"]
        NOTIFY["internal/notify<br/>Webhook / Feishu / Noop"]
    end

    subgraph persistence["持久化与观测"]
        STORE["internal/store<br/>唯一数据库访问边界"]
        DB[("MySQL<br/>状态、队列、审计、控制室")]
        EVENTS["internal/eventlog<br/>事件类型定义"]
        METRICS["internal/metrics<br/>进程计数器与 gauges"]
        MIG["migrations/*.sql<br/>手工 schema"]
    end

    BOOT --> API
    BOOT --> INGW
    BOOT --> DIAGW
    BOOT --> EXPW
    BOOT --> EXECW
    BOOT --> CONVW

    WEB --> API
    API --> STORE
    API --> APPROVAL
    API --> CONV
    API --> LLM
    API --> METRICS

    INGW --> ING
    ING --> INC
    ING --> STORE
    INC --> STORE

    DIAGW --> PIPE
    PIPE --> MEMORY
    PIPE --> LLM
    PIPE --> TOOLS
    PIPE --> APPROVAL
    PIPE --> NOTIFY
    PIPE --> STORE

    EXECW --> APPROVAL
    EXECW --> TOOLS
    EXECW --> VERIFY
    EXECW --> MEMORY
    EXECW --> RETRY
    EXECW --> STORE

    CONVW --> CONV
    CONVW --> LLM
    CONVW --> TOOLS
    CONVW --> NOTIFY
    CONVW --> STORE

    APPROVAL --> STORE
    MEMORY --> STORE
    VERIFY --> STORE
    RETRY --> STORE
    NOTIFY --> STORE
    EVENTS -.-> STORE

    STORE --> DB
    MIG -.手工执行.-> DB
    ING -.-> METRICS
    DIAGW -.-> METRICS
    EXECW -.-> METRICS
```

### 模块职责

| 模块 | 当前职责 |
|---|---|
| `cmd/server` | 唯一组合根；所有依赖在这里组装 |
| `internal/config` | YAML 读取、环境变量展开、启动前校验、默认值 |
| `internal/api` | GoFrame 适配器、HTTP 路由、DTO 脱敏、Bearer/匿名鉴权、SSE |
| `internal/ingest` | Alertmanager v4 解析、fingerprint、alert hash、severity、去重、归并 |
| `internal/incident` | severity → `full/light/skip` 路由；构造诊断队列行 |
| `internal/diagnose` | Evidence、Pipeline、Guard、Verify、Retry |
| `internal/llm` | OpenAI 兼容模型工厂、Eino ReAct、只读 Questioner、模型切换 |
| `internal/tools` | 工具注册、等级控制、统一超时、输出截断、Prometheus/Docker 适配 |
| `internal/approval` | Policy 判定、审批 CAS、审批 TTL、执行队列、执行结果 |
| `internal/memory` | 精确故障指纹记忆；高置信 TTL 召回、写回和降级 |
| `internal/conversation` | Web/Feishu 共用的对话队列、上下文组装、回答持久化 |
| `internal/notify` | Provider 无关通知接口；Noop、传统 webhook、Feishu |
| `internal/store` | 唯一 GORM/MySQL 边界；所有业务状态和审计写入 |
| `internal/eventlog` | 事件类型常量；事件实际由各业务模块通过 store 写入 |
| `internal/metrics` | 进程内 counter/gauge，暴露 `/metrics` |
| `web` | Vite 产物通过 `go:embed` 嵌入 Go 服务；产物不入库，需先 `npm run build` |
| `cmd/simulate` | 走同一个 webhook 入口注入测试告警 |

## 4. 告警接入、去重与 Incident 归并

```mermaid
sequenceDiagram
    participant AM as Alertmanager
    participant API as AlertmanagerWebhook API
    participant DB as MySQL
    participant IW as ingest.Worker
    participant ING as Parse/Fingerprint/Dedup
    participant COR as Correlator
    participant DW as diagnose.Worker

    AM->>API: POST /webhook/alertmanager<br/>Authorization: Bearer
    API->>DB: INSERT raw_event(status=pending)
    DB-->>API: raw_event id
    API-)IW: 非阻塞 Notify()
    API-->>AM: 202 Accepted

    IW->>DB: 读取最早 pending raw_event
    IW->>ING: ParseWebhook(payload)
    ING->>ING: 规范化 labels/status/time
    ING->>ING: 重算 fingerprint SHA-256
    ING->>ING: 重算 severity
    ING->>ING: 重算 alert_hash MD5

    IW->>DB: ApplyRawEvent（单事务）

    alt full duplicate
        DB->>DB: 不追加 alert<br/>刷新 last_alert.last_seen
        DB->>DB: TouchIncident（若已有 incident）
    else new / partial
        DB->>DB: 追加 alert
        DB->>DB: 更新 last_alert
        IW->>COR: Assign(group_key, window, min_alerts)
        COR->>DB: 查开放 candidate/firing incident
        alt 窗口内找到
            DB->>DB: 挂载 incident_alert<br/>刷新 severity/last_seen
        else 找不到
            DB->>DB: 创建 candidate incident
        end

        alt 达到 min_alerts 且首次 promote
            DB->>DB: candidate → firing
            DB->>DB: 创建 agent_run
            alt mode=skip
                DB->>DB: agent_run 直接 succeeded
            else mode=full/light
                DB->>DB: agent_run=pending
            end
        end
    end

    DB->>DB: raw_event → processed
    DW->>DB: 轮询 pending agent_run
```

### 三层身份模型

| 身份 | 用途 | 算法/来源 |
|---|---|---|
| `fingerprint` | 判断是不是同一个告警对象 | 选定 labels 排序后 SHA-256 |
| `alert_hash` | 判断同一对象内容是否完全重复 | 状态、labels、annotations 规范化后 MD5 |
| `group_key` | 判断多条告警是否属于同一故障 | `correlate.group_by` labels + 时间窗口 |

### `resolved` 路径

```mermaid
flowchart LR
    RES["Alertmanager resolved"] --> PARSE["解析 + 重算 fingerprint"]
    PARSE --> SNAP["更新 last_alert.status=resolved"]
    SNAP --> FIND["查该 fingerprint 所属 incident"]
    FIND --> ALL{"所有成员都 resolved?"}
    ALL -->|否| KEEP["incident 继续开放"]
    ALL -->|是| CLOSE["incident → resolved<br/>写 incident.resolved 事件"]
```

当前行为：

- `firing` 和 `resolved` 共用同一个 fingerprint。
- 完全重复的 firing 不生成新的诊断。
- 完全重复仍刷新 `last_seen`，避免持续告警因重复推送被切成多个 Incident。
- 数据库事务失败时 `raw_event` 保持 `pending`，等待下一轮重试。
- 输入本身不可恢复时，`raw_event` 标记 `failed`，不会永久堵住队首。

依据：`internal/ingest/worker.go:78-319`、`internal/ingest/webhook.go:27-133`、`internal/ingest/fingerprint.go:12-90`、`internal/ingest/correlate.go:47-101`、`internal/store/rawevent.go:114-220`。

## 5. 诊断、Guard、Policy、审批、执行、验证闭环

```mermaid
flowchart TB
    Q["agent_run(status=pending)"] --> CLAIM["diagnose.Worker<br/>claim pending → running"]
    CLAIM --> MODE{"诊断模式"}

    MODE -->|"skip"| SKIP["摄入事务中已直接 succeeded"]
    MODE -->|"full / light"| MEM{"retry_of 为空<br/>且故障记忆高置信、未过 TTL?"}

    MEM -->|命中| HIT["mode=memory_hit<br/>复用 RCA + Plan<br/>0 次 LLM"]
    MEM -->|未命中| EVID["EvidenceBuilder"]

    subgraph collectors["代码采集证据：不调用 LLM"]
        SNAP["alert_snapshot<br/>Incident/成员/当前告警"]
        REPLAY["prom_replay<br/>generatorURL 中的 g0.expr 回放"]
        GOLD["golden_metrics<br/>CPU/内存/磁盘/网络"]
        SUB["sub2api<br/>/health + 网关 Prom 指标"]
        PGCOL["postgres<br/>SELECT 1 + pg_stat_activity"]
        RDCOL["redis<br/>PING + INFO memory/clients"]
        DOCCOL["docker<br/>inspect + 限量日志"]
    end

    EVID --> SNAP
    EVID --> REPLAY
    EVID --> GOLD
    EVID --> SUB
    EVID --> PGCOL
    EVID --> RDCOL
    EVID --> DOCCOL

    SNAP --> COLLECTED["Evidence items"]
    REPLAY --> COLLECTED
    GOLD --> COLLECTED
    SUB --> COLLECTED
    PGCOL --> COLLECTED
    RDCOL --> COLLECTED
    DOCCOL --> COLLECTED
    COLLECTED --> RENDER["Evidence.Render<br/>脱敏、截断、防 Prompt Injection"]
    RENDER --> REASON["llm.Reasoner<br/>Eino ReAct<br/>full≤8步 / light≤3步"]

    subgraph llmtools["LLM 可见工具面"]
        L1["Registry.ForLLM()<br/>仅 L1 只读工具"]
        PROMTOOL["Prometheus instant/range/series"]
        DOCTOOL["Docker inspect/logs<br/>若 socket 可用"]
        L1 --> PROMTOOL
        L1 --> DOCTOOL
    end

    REASON -.-> L1
    REASON --> GUARD["Guard<br/>确定性规则纠偏"]
    HIT --> GUARD

    GUARD --> POLICY["Policy<br/>工具等级 + plan_hash + L2 护栏"]

    POLICY -->|"none / denied"| REPORT["诊断报告 + 审计"]
    POLICY -->|"L1 read-only"| REPORT
    POLICY -->|"L2 自动条件满足"| SYSAPP["系统批准 approval<br/>统一进入执行队列"]
    POLICY -->|"L3 或 L2 护栏失败"| PENDING["approval=pending"]

    PENDING --> HUMAN["Web 匿名控制台<br/>或 Feishu 卡片"]
    HUMAN -->|approve| APPROVED["approval=approved"]
    HUMAN -->|deny| DENIED["approval=denied"]
    PENDING -->|"TTL 到期"| EXPIRED["approval=expired"]

    SYSAPP --> APPROVED
    APPROVED --> EXEC["approval.Executor<br/>claim approved → executing"]
    EXEC --> HASH["重算 tool + args + plan_hash"]
    HASH --> REGEXEC["Registry.Execute<br/>统一超时 + 输出截断"]
    REGEXEC --> ACTION["Docker / 外部运行时动作"]
    ACTION --> VERIFY["独立 Verify<br/>延迟复查 Incident 成员状态"]

    VERIFY -->|passed| SUCCESS["approval=executed<br/>写 fault_cmd_history"]
    SUCCESS --> COMMIT["仅 high + 干净案例<br/>写 fault_memory"]
    VERIFY -->|failed| RETRY{"retry_of 链<br/>是否仍在上限内?"}
    RETRY -->|是| NEWRUN["创建 full retry agent_run<br/>注入上一轮失败上下文"]
    RETRY -->|否| ESCALATE["通知人工升级"]
    VERIFY -->|inconclusive| MANUAL["只留审计<br/>等待人工核查"]

    NEWRUN --> Q
    REPORT --> AUDIT["agent_run_step<br/>incident_event<br/>incident_problem"]
    DENIED --> AUDIT
    EXPIRED --> AUDIT
    EXEC --> AUDIT
    VERIFY --> AUDIT
```

### Pipeline 实际阶段

`diagnose.Pipeline.Run` 的当前顺序：

1. `LoadTarget`：读取 Incident、成员、当前告警。
2. `memory.Lookup`：仅首次诊断查记忆，重诊不查。
3. `EvidenceBuilder.BuildForIncident`：顺序执行 7 类 collector。
4. `Evidence.Render`：统一脱敏、截断、引用隔离。
5. `Reasoner.Diagnose`：Eino ReAct，非法 JSON 最多重试一次。
6. `Guard`：确定性规则覆盖 LLM 计划。
7. `Policy`：决定 `none / denied / approval / auto_l2`。
8. `NotifyReporter`：通知失败单独记录，不回滚诊断终态。
9. `CompleteRun`：run 状态和最终事件短事务提交。

依据：`internal/diagnose/pipeline.go:110-283`、`internal/diagnose/evidence.go:15-152`、`internal/diagnose/guard.go:26-95`、`internal/approval/policy.go:92-184`。

### 安全闸门

```mermaid
flowchart LR
    PLAN["LLM / Memory Plan"] --> GUARD["Guard"]
    GUARD --> POLICY["Policy"]
    POLICY --> TOOL["Registry 工具目录"]
    TOOL --> EXEC["Executor"]
    EXEC --> VERIFY["Verify"]
```

- LLM 只能看到 `Registry.ForLLM()` 导出的 L1 工具。
- L2/L3/L4 变更动作不会直接暴露给 ReAct。
- `Guard` 会拦截无真实 target 的动作。
- 配置错误、镜像不存在、凭据错误等根因会禁止重启类动作并升级人工。
- L2 自动执行必须同时满足全局开关、非 dry-run、目标白名单、可信 target 来源、单对象影响范围、可验证结果和限频条件。
- L4 永远拒绝，审批不能解除。
- 执行前重新计算 `plan_hash`，审批单被篡改时拒绝执行。
- Verify 不把“命令调用成功”当作“故障恢复成功”。

## 6. Web 控制室、SSE 与 Feishu 协同

```mermaid
flowchart LR
    subgraph browser["浏览器：React 控制台"]
        APP["web/src/main.tsx<br/>按 pathname 区分页面"]
        PICK["IncidentPicker<br/>Incident 列表"]
        ROOM["IncidentRoom<br/>聚合作战台状态"]
        FLOW["FlowGraph<br/>8 个固定阶段"]
        TIMELINE["EventTimeline<br/>事件审计流"]
        PROBLEM["ProblemPanel<br/>当前问题"]
        APPROVAL["ApprovalPanel<br/>批准/拒绝/请求证据"]
        STEP["StepInspector<br/>安全展示 step JSON"]
        CONV["ConversationPanel<br/>Incident 问答"]
        MODEL["ModelSwitcher<br/>模型 allowlist 切换"]

        APP --> PICK
        APP --> ROOM
        ROOM --> FLOW
        ROOM --> TIMELINE
        ROOM --> PROBLEM
        ROOM --> APPROVAL
        ROOM --> STEP
        ROOM --> CONV
        APP --> MODEL
    end

    subgraph http["Go HTTP 控制室边界"]
        STATIC["StaticAPI<br/>embedded web/dist"]
        CONTROL["ControlRoomAPI<br/>首屏聚合 DTO"]
        STREAM["StreamAPI<br/>MySQL id-after 轮询"]
        RUNAPI["RunAPI<br/>ownership-safe steps"]
        CONVAPI["ConversationAPI<br/>只入队，不同步跑 LLM"]
        APPAPI["ApprovalAPI<br/>审批 CAS"]
        MODELAPI["ModelAPI<br/>GET public / PUT Bearer"]
    end

    subgraph db["MySQL 控制室数据"]
        INCIDENTS[("incident / incident_alert")]
        EVENTS[("incident_event")]
        PROBLEMS[("incident_problem")]
        MSG[("conversation_message")]
        RUNS[("agent_run / agent_run_step")]
        APPS[("approval")]
        MODELSEL[("llm_model_selection")]
    end

    BROWSER["浏览器请求"] --> STATIC
    STATIC --> APP

    PICK -->|"GET /api/v1/control-room/incidents"| CONTROL
    ROOM -->|"GET /api/v1/incidents/:id/control-room"| CONTROL
    ROOM -->|"GET /api/v1/incidents/:id/runs/.../steps"| RUNAPI
    ROOM -->|"GET /api/v1/incidents/:id/conversation"| CONVAPI
    ROOM -->|"EventSource /api/v1/incidents/:id/stream"| STREAM

    CONTROL --> INCIDENTS
    CONTROL --> EVENTS
    CONTROL --> PROBLEMS
    CONTROL --> RUNS
    CONTROL --> APPS
    RUNAPI --> RUNS
    CONVAPI --> MSG
    STREAM -->|"每 1 秒按 id > cursor 查询"| EVENTS
    EVENTS --> STREAM
    STREAM --> ROOM
    ROOM -->|"合并事件 + debounce refresh"| CONTROL

    APPROVAL -->|"POST approve/deny"| APPAPI
    APPAPI --> APPS
    MODEL -->|"PUT /api/v1/admin/model<br/>Bearer Token"| MODELAPI
    MODELAPI --> MODELSEL
```

### Web 当前行为

- `web/src/main.tsx` 没有 React Router，只用 pathname 判断：
  - `/` → `IncidentPicker`
  - `/incidents/<id>` → `IncidentRoom`
- `IncidentRoom` 首屏读取控制室聚合、run steps 和对话历史。
- `EventSource` 连接后端 SSE，后端按 MySQL `incident_event.id` 做游标轮询。
- 前端收到事件后合并去重，并触发延迟刷新。
- 重诊、补充证据、提问、审批都是异步入队。
- `request-evidence` 复用 conversation 队列，不在 HTTP handler 中直接采证据或调用 Docker。
- `FlowGraph` 固定展示：

```text
alert → evidence → reasoner → guard → policy → approval → execute → verify
```

### Feishu 链路

```mermaid
sequenceDiagram
    participant P as Pipeline/Executor
    participant N as Feishu Notifier
    participant F as Feishu API
    participant CB as /integrations/feishu/events
    participant SDK as Lark SDK
    participant BIZ as CallbackBusiness
    participant DB as MySQL
    participant CW as ConversationWorker

    P->>N: Diagnosis / Approval / Escalation Notification
    N->>F: 发送 Card 2.0
    F-->>N: message_id
    N->>DB: 写 im_binding + notification event

    F->>CB: card.action.trigger / message.receive
    CB->>SDK: 验签、解密、challenge/event decode
    SDK->>BIZ: CardAction / MessageReceive
    BIZ->>DB: integration_event_receipt 去重

    alt 卡片批准/拒绝
        BIZ->>DB: 读取 approval
        BIZ->>DB: CAS 决策 pending → approved/denied
        BIZ-->>F: Toast
        BIZ-)F: 异步 patch 卡片
    else Incident 对话
        BIZ->>DB: 查 im_binding
        BIZ->>DB: conversation_message queued
        BIZ-->>F: 快速返回
        CW->>DB: claim queued → running
        CW->>DB: 组装 Incident 上下文
        CW->>F: 可选回复原线程
    end
```

当前 Feishu 回调只有在 `notify.im.provider == "feishu_app"` 时才由 `main.go` 绑定。

## 7. API 面

| 路径 | 当前鉴权 | 作用 |
|---|---|---|
| `POST /webhook/alertmanager` | Bearer | Alertmanager v4 告警入队 |
| `GET /api/v1/incidents` | Bearer | 自动化 Incident 列表 |
| `GET /api/v1/incidents/:id` | Bearer | Incident 详情和成员 |
| `POST /api/v1/incidents/:id/diagnose` | Bearer | 手动创建诊断 run |
| `GET /debug/evidence/:id` | Bearer | 查看渲染后的证据 |
| `GET /metrics` | 无鉴权 | Prometheus 文本指标 |
| `/api/v1/approvals...` | Bearer 或匿名控制台 | 审批列表、approve、deny |
| `/api/v1/admin/model` | Bearer | 管理端切换模型 |
| `/api/v1/control-room/model` | 控制室公开读 | 当前模型和 allowlist |
| `/api/v1/control-room/incidents` | 匿名控制台 | 控制室 Incident 列表 |
| `/api/v1/incidents/:id/control-room` | 匿名控制台 | 作战台首屏聚合 |
| `/api/v1/incidents/:id/events` | 匿名控制台 | 事件分页 |
| `/api/v1/incidents/:id/problems` | 匿名控制台 | 当前问题 |
| `/api/v1/incidents/:id/runs...` | 匿名控制台 | run/step 历史 |
| `/api/v1/incidents/:id/stream` | 匿名控制台 | SSE |
| `/api/v1/incidents/:id/conversation` | 匿名控制台 | 对话历史 |
| `/api/v1/incidents/:id/questions` | 匿名控制台 | 提问入队 |
| `/api/v1/incidents/:id/rediagnose` | 匿名控制台 | 重诊入队 |
| `/api/v1/incidents/:id/request-evidence` | 匿名控制台 | 补充证据请求入队 |
| `POST /integrations/feishu/events` | Lark SDK 验签解密 | Feishu 事件和卡片回调 |
| `/`、`/*path` | 无 | 嵌入式 React 静态资源 |

路由绑定集中在 `cmd/server/main.go:242-287`。

## 8. Worker 与队列

| Worker | 持久队列/状态 | 运行方式 | 恢复行为 |
|---|---|---|---|
| `ingest.Worker` | `raw_event.status=pending` | 非阻塞唤醒 + 1 秒 ticker | 启动扫描 pending；单 goroutine 按 ID 消费 |
| `diagnose.Worker` | `agent_run.status=pending` | 定时轮询 | 启动和运行中将超时 `running` 重新放回 pending |
| `approval.ExpiryWorker` | `approval.status=pending/approved` | 默认每分钟扫描 | 过期审批转 `expired` 并写事件/问题 |
| `approval.Executor` | `approval.status=approved` | 默认每秒轮询 | 启动时回收中断的 `executing`；不自动重放外部动作 |
| `conversation.Worker` | `conversation_message.status=queued` | 定时轮询 | 超时 `running` 消息重新入队 |

系统不使用 RabbitMQ、Kafka、Redis Streams 或独立任务服务。MySQL 行锁、条件更新和幂等状态迁移承担队列可靠性。

## 9. MySQL 数据模型

当前 migration 逻辑上共有 18 张表：

- `001_init.sql`：10 张核心表
- `005_control_room.sql`：7 张控制室/会话表
- `007_llm_model_selection.sql`：1 张模型选择表

下面关系是**业务逻辑关系**。当前 SQL 没有声明外键，图中的关系不是数据库 FK 约束。

### 9.1 告警、Incident、诊断、审批核心表

```mermaid
erDiagram
    raw_event {
        BIGINT id PK
        VARCHAR source
        JSON payload
        ENUM status
        TEXT error
        DATETIME created_at
        DATETIME processed_at
    }

    alert {
        BIGINT id PK
        CHAR64 fingerprint
        CHAR32 alert_hash
        VARCHAR source
        VARCHAR name
        TINYINT severity
        ENUM status
        JSON labels
        JSON annotations
        TEXT generator_url
        DATETIME starts_at
        DATETIME received_at
    }

    last_alert {
        CHAR64 fingerprint PK
        BIGINT alert_id
        CHAR32 alert_hash
        ENUM status
        TINYINT severity
        DATETIME first_seen
        DATETIME last_seen
        INT firing_count
        BIGINT incident_id
    }

    incident {
        BIGINT id PK
        VARCHAR group_key
        ENUM status
        TINYINT severity
        INT alerts_count
        VARCHAR title
        DATETIME started_at
        DATETIME last_seen_at
        DATETIME resolved_at
    }

    incident_alert {
        BIGINT incident_id PK
        CHAR64 fingerprint PK
        DATETIME linked_at
    }

    agent_run {
        BIGINT id PK
        BIGINT incident_id
        ENUM mode
        ENUM status
        BIGINT retry_of
        TEXT rca_text
        JSON plan_json
        INT tokens_in
        INT tokens_out
        DATETIME started_at
        DATETIME finished_at
    }

    agent_run_step {
        BIGINT id PK
        BIGINT run_id
        INT seq
        ENUM kind
        VARCHAR name
        JSON input_json
        JSON output_json
        TEXT error
        DATETIME started_at
        DATETIME finished_at
    }

    approval {
        BIGINT id PK
        BIGINT incident_id
        BIGINT run_id
        VARCHAR tool_name
        JSON args_json
        CHAR64 plan_hash
        TEXT reason
        ENUM status
        DATETIME expires_at
        VARCHAR decided_by
        JSON result_json
        DATETIME created_at
        DATETIME decided_at
        TEXT decision_reason
        VARCHAR decision_source
    }

    fault_memory {
        CHAR12 fingerprint PK
        VARCHAR group_key
        VARCHAR alert_name
        TEXT rca_text
        JSON plan_json
        ENUM confidence
        INT hits
        DATETIME first_seen
        DATETIME last_used
        DATETIME last_success
        INT ttl_sec
    }

    fault_cmd_history {
        BIGINT id PK
        CHAR12 fingerprint
        VARCHAR tool_name
        JSON args_json
        VARCHAR result_brief
        BIGINT approval_id
        DATETIME created_at
    }

    raw_event ||--o{ alert : "逻辑处理产生"
    alert }o--|| last_alert : "fingerprint 当前快照"
    last_alert ||--o{ incident_alert : "成员快照"
    incident ||--o{ incident_alert : "Incident 成员"
    incident ||--o{ agent_run : "诊断队列"
    agent_run ||--o{ agent_run_step : "步骤审计"
    agent_run ||--o{ approval : "动作申请"
    approval ||--o{ fault_cmd_history : "执行结果"
    fault_memory ||--o{ fault_cmd_history : "同故障指纹"
```

### 9.2 控制室、对话、集成与模型表

```mermaid
erDiagram
    incident_event {
        BIGINT id PK
        BIGINT incident_id
        BIGINT run_id
        BIGINT approval_id
        VARCHAR event_type
        VARCHAR phase
        VARCHAR status
        VARCHAR summary
        JSON payload_json
        DATETIME created_at
    }

    incident_problem {
        BIGINT id PK
        BIGINT incident_id
        BIGINT run_id
        VARCHAR code
        VARCHAR severity
        VARCHAR status
        VARCHAR summary
        JSON detail_json
        DATETIME first_seen_at
        DATETIME last_seen_at
        DATETIME resolved_at
    }

    conversation_message {
        BIGINT id PK
        BIGINT incident_id
        BIGINT run_id
        BIGINT reply_to_id
        VARCHAR channel
        VARCHAR role
        VARCHAR actor_id
        VARCHAR actor_name
        TEXT content
        VARCHAR tool_name
        VARCHAR tool_call_id
        VARCHAR status
        JSON metadata_json
        DATETIME created_at
        DATETIME finished_at
        DATETIME claimed_at
    }

    im_binding {
        BIGINT id PK
        VARCHAR provider
        VARCHAR chat_id
        VARCHAR message_id
        VARCHAR root_message_id
        VARCHAR thread_id
        BIGINT incident_id
        BIGINT run_id
        BIGINT approval_id
        VARCHAR message_kind
        DATETIME created_at
    }

    integration_event_receipt {
        VARCHAR event_id PK
        VARCHAR provider
        VARCHAR event_type
        DATETIME processed_at
        VARCHAR result
    }

    llm_model_selection {
        TINYINT singleton_id PK
        VARCHAR current_model
        DATETIME updated_at
    }

    incident ||--o{ incident_event : "事实事件"
    agent_run ||--o{ incident_event : "run 事件"
    approval ||--o{ incident_event : "审批/执行事件"
    incident ||--o{ incident_problem : "当前问题"
    agent_run ||--o{ incident_problem : "问题来源"
    incident ||--o{ conversation_message : "Incident 对话"
    agent_run ||--o{ conversation_message : "回答关联 run"
    conversation_message }o--o| conversation_message : "reply_to"
    incident ||--o{ im_binding : "外部消息绑定"
    agent_run ||--o{ im_binding : "外部 run 绑定"
    approval ||--o{ im_binding : "外部审批卡片绑定"
```

### 数据模型中的实际缺口

当前 `raw_event` 没有 `incident_id`，`incident_event` 也没有 `raw_event_id`。因此数据库能稳定回放：

```text
incident → run → step → approval → execution → verify
```

但不能在 schema 层直接保存：

```text
raw_event → incident
```

这与部分设计文档中“按 raw_event_id 贯穿全链路”的描述不一致。

依据：`migrations/001_init.sql`、`migrations/005_control_room.sql`、`migrations/007_llm_model_selection.sql`、`internal/store/models.go`。

## 10. 状态机

| 对象 | 当前状态路径 |
|---|---|
| `raw_event` | `pending → processed` 或 `pending → failed` |
| `incident` | 枚举包含 `candidate/firing/acknowledged/resolved`；主链主要为 `candidate → firing → resolved` |
| `agent_run` | `pending → running → succeeded/failed` |
| 重诊 run | 通过 `retry_of` 串成 run 链，自动重诊上限为 2 |
| `approval` | `pending → approved/denied/expired`；`approved → executing → executed/failed` |
| `conversation_message` | `queued → running → completed/failed`；租约超时可重新入队 |
| `fault_memory` | `high` 命中验证失败后降为 `low`，后续不再召回 |
| `llm_model_selection` | 单例行保存当前模型 ID，不保存 URL、密钥或 token |

## 11. 当前配置下的启用状态

| 能力 | 当前配置 | 实际效果 |
|---|---|---|
| MySQL | `mysql.dsn: ${MYSQL_DSN}` | 必须配置，否则启动失败 |
| LLM Reasoner | 已配置 `glm-5` | `diagnose.Worker` 会启动 |
| Web 控制台 | `web.base_url` 非空，或 provider 为 `feishu_app` | `webEnabled=true`，组装 `api.Console` |
| Web 鉴权 | 无 | 控制台读写使用固定 `anonymous` 身份；没有登录、会话或 CSRF |
| Feishu | `notify.im.provider` 未启用 | 不绑定 Feishu callback；通知通常回退 Noop |
| `approval.dry_run` | 未配置，默认 `true` | 默认不执行真实 Docker 变更 |
| `approval.auto_execute_l2` | 未配置，默认 `false` | L2 自动路径默认关闭 |
| Docker allowlist | 未配置，默认为空 | 没有默认可自动重启目标 |
| Prometheus | 配置为本机 Prometheus | 证据和黄金指标查询使用该地址 |
| Docker socket | 配置为独立 Unix socket | socket 不存在时跳过 Docker 工具和证据 |
| Web 前端产物 | `web/dist/` 不入库 | 未跑 `npm run build` 时后端照常启动，静态资源全 404 |

## 12. 安全与可靠性不变量

### 12.1 入站与队列

- webhook 先写 `raw_event(pending)`，再返回 202；内存 channel 只负责唤醒。
- 输入错误标记 `failed`，数据库错误保留 `pending`。
- worker 启动时扫描 pending，进程重启后可补账。
- 领取使用行锁和条件更新，避免重复消费。

### 12.2 LLM 边界

- LLM 只接收脱敏、截断后的证据。
- LLM 只能调用 L1 只读工具。
- LLM 输出不是权限结论。
- 非法 JSON 最多重试一次。
- 工具统一设置超时和最大输出。

### 12.3 变更动作

- 所有动作必须存在于 Registry。
- L2 自动执行需要完整护栏；任一条件失败则降级审批。
- 审批绑定 `tool_name + args_json + plan_hash`。
- 执行前重新计算 hash，内容变化即拒绝。
- 执行成功后还必须独立 Verify。
- Verify 不可判定时不写成功记忆，也不自动重诊。

### 12.4 敏感数据

- DSN、token、API key、Cookie 和 Authorization 不应进入日志、数据库摘要或 Prompt。
- Web DTO 不直接暴露完整 `PlanJSON`、工具参数和外部执行结果。
- Feishu 卡片只携带有限引用和展示字段，不传可执行参数和凭据。

## 13. 当前漂移与风险点

1. **控制台是公开操作面**：`webEnabled` 后无任何鉴权；能访问服务端口的请求方可以查看、提问、重诊、请求证据和审批，审计一律记为 `anonymous`。这是明确的设计选择（见 `docs/design-review.md` B1 补记），要鉴权需要重新实现。
2. **Alertmanager 凭据需要对齐**：Alertmanager 配置文件中的固定凭据必须与服务端环境变量一致，否则 webhook 会返回 401。
3. **当前 Compose 未抓取 oncall-agent `/metrics`**：Prometheus 配置只有 node-exporter 和 blackbox job。
4. **默认是演练模式**：`dry_run=true`、`auto_execute_l2=false`，且 Docker allowlist 为空。
5. **原始告警到 Incident 缺少数据库直接关联**：`raw_event_id` 没有进入 `incident_event` 或后续状态表。
6. **部分旧文档已过时**：当前实现已经包含诊断、审批、执行、Verify、Memory、控制室、SSE 和对话队列。

## 14. 代码依据索引

| 主题 | 主要文件 |
|---|---|
| 服务启动与依赖组装 | `cmd/server/main.go` |
| HTTP 路由 | `cmd/server/main.go:242-287` |
| 配置与默认值 | `internal/config/config.go`、`config.yaml` |
| 告警解析/指纹/归并 | `internal/ingest/*.go` |
| 摄入事务与队列 | `internal/ingest/worker.go`、`internal/store/rawevent.go`、`internal/store/incident.go` |
| 诊断 Pipeline | `internal/diagnose/pipeline.go` |
| Evidence collectors | `internal/diagnose/collector_*.go`、`internal/diagnose/evidence.go` |
| LLM 与工具权限 | `internal/llm/*.go`、`internal/tools/*.go` |
| Guard/Policy/审批/执行 | `internal/diagnose/guard.go`、`internal/approval/*.go` |
| Verify/重诊 | `internal/diagnose/verify.go`、`internal/diagnose/retry.go` |
| 控制室/SSE/对话 | `internal/api/controlroom.go`、`internal/api/stream.go`、`internal/conversation/*.go` |
| Feishu 回调 | `internal/notify/feishu/*.go`、`internal/api/feishu_callback.go` |
| 数据模型 | `migrations/*.sql`、`internal/store/models.go` |
| 前端控制台 | `web/src/main.tsx`、`web/src/pages/IncidentRoom.tsx`、`web/src/api.ts` |
| 部署与告警 | `docker-compose.dev.yml`、`prometheus.yml`、`alerts.yml`、`alertmanager.yml`、`blackbox.yml` |

## 15. 核验记录

已核验：

```text
go list ./...
```

识别到 18 个 Go 包，覆盖 server、simulate、全部 internal 模块和 `web`。
（2026-08-24 起 `internal/auth` 已删除，包数由 19 降为 18。）

已运行核心模块单元测试：

```text
go test ./internal/ingest ./internal/incident ./internal/approval \
  ./internal/diagnose ./internal/tools ./internal/conversation ./internal/api
```

结果：全部通过。
