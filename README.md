# oncall-agent（AI-Ops）

面向 Prometheus/Alertmanager 告警的自动处置系统：告警接入 → 去重 → incident 归并 → 证据采集 → LLM 诊断 → Guard 校验 → 规则授权（observe / manual / auto）→ 受控执行 → 恢复验证与复发观察 → 故障记忆与效果评估，全链路可审计、可冻结回放。

目标是 Docker Compose 部署的 sub2api（网关 + PostgreSQL + Redis + 宿主机），方案见[生产自动处置实施方案](docs/production-auto-remediation-plan.md)，生产部署见 [deploy/README.md](deploy/README.md)。oncall-agent 自身只依赖 MySQL，不引入 Redis/MQ/向量数据库。

## 功能概览

- **摄入**：Alertmanager v4 webhook，先落 `raw_event(pending)` 再异步处理，崩溃可重放；
- **归并**：fingerprint 两级去重 + group key 时间窗归并，candidate 攒够阈值促发 firing；
- **生命周期**：全部成员 resolved 时 incident 自动关单；
- **诊断**：agent_run 队列驱动独立诊断 worker（摄入不阻塞）；证据由代码采集（0 次 LLM），ReAct 按每次请求的上下文预算压缩旧工具结果，步数与超时作为兜底；
- **授权**：写操作只能由版本化的处置规则授权——`observe` 只记录本会采取的动作，`manual` 由操作人确认冻结快照，`auto` 在事实、范围、预算和健康条件满足时自动执行；Guard 依据结构化证据校验动作前提；`incident.PlanHash` 绑定动作、参数和版本 3 的 `ExecutionContext`（规则版本、目标身份与修订、执行前状态、验证标准、补偿、有效期）；
- **动作**：`docker_restart`（进程恢复）、`deployment_rollback`（经部署入口回到人工确认健康的发布）、`upstream_quarantine`（停调度异常上游账号，可补偿）；模型只提议动作和声明的参数，目标、digest 和阈值由可信配置与准备阶段确定；
- **准入与发布**：告警、人工重诊、验证失败重诊共用 `RequestRun`；`CompleteRun` 同事务发布诊断结论、审批与审计；
- **执行与验证**：同一服务同时只有一个执行或验证中的处置；领取时复验授权、急停、预算和对象修订，执行留下结构化回执（written / not_written / unknown），进程中断后按目标实际状态对账、不重放；验证要求连续通过，失败或不可判定时执行预先冻结的补偿并阻断规则，恢复后在观察窗口内识别复发；
- **反馈与评测**：控制台“自动处置”页展示规则与阻断、急停/复位、发布记录和效果报表（错误执行率、无人介入恢复率、恢复耗时、复发率、人工占比、根因准确率，均附原始计数）；Incident 复盘标注；诊断输入完整落库，可用 `cmd/replay` 冻结回放；
- **记忆**：验证成功的高置信案例入库；同类故障命中时 0 次 LLM（memory_hit），命中失败自动降级拉黑；
- **可观测**：`/metrics`（Prometheus 文本格式）+ `agent_run_step` 逐步审计。

诊断上下文按 `llm.models[].context_window_tokens` 配置；示例中的 DeepSeek 窗口为 1000000，第三方网关容量需单独验证。每次请求预留输出空间，接近输入额度的 80% 才压缩旧工具结果。Token 使用启发式估算与单次请求 usage 校准；旧 `diagnose.budget.context_tokens` 已移除。

## 快速开始

### 环境要求

- Go 1.24+
- Docker（Compose 提供 MySQL/Prometheus/Alertmanager/blackbox/node_exporter）
- Node.js 20+（构建时需要：先构建前端，再编译带 `go:embed` 的 Go 服务；运行二进制不需要 Node.js）

### 三步跑通

以下用于**空库的本地 Compose 联调**，oncall-agent 跑在宿主机。已有数据库不要重跑全部迁移：先按[升级说明](docs/execution-trust-upgrade.md)停机、备份并退役旧审批。显式监听 `0.0.0.0:18080` 前，必须用宿主防火墙限制整个 listener 到可信来源。

```bash
# 1. 起依赖 + 空库按文件名顺序执行全部迁移
docker compose -f docker-compose.dev.yml up -d
for f in migrations/*.sql; do
  docker compose -f docker-compose.dev.yml exec -T mysql \
    mysql -uoncall -poncall-pass oncall < "$f" || exit 1
done

# 2. 最小配置并启动（已有 config.yaml 时请合并，不要覆盖）
cat > config.yaml <<'YAML'
server:
  listen_addr: "0.0.0.0"  # 显式允许 Compose 容器访问宿主机；不是安全默认值
  port: 18080
  auth_token: ${AUTH_TOKEN}
mysql:
  dsn: ${MYSQL_DSN}
YAML
# 仅本地联调，与 alertmanager.yml 对齐；其他部署须同时替换两侧凭据。
export AUTH_TOKEN='devtoken'
export MYSQL_DSN='oncall:oncall-pass@tcp(127.0.0.1:3306)/oncall?parseTime=true&loc=UTC'
(cd web && npm ci && npm run build)
CONFIG_FILE=config.yaml go run ./cmd/server

# 3. 在另一终端注入模拟告警（仅摄入/生命周期，不是真实修复验收）
export AUTH_TOKEN='devtoken'
go run ./cmd/simulate -url http://127.0.0.1:18080/webhook/alertmanager -n 3 -dup 0
go run ./cmd/simulate -url http://127.0.0.1:18080/webhook/alertmanager -n 3 -dup 0 -resolved
```

观察：

```bash
# incident 列表与详情（含成员）
curl -H "Authorization: Bearer $AUTH_TOKEN" 'http://127.0.0.1:18080/api/v1/incidents?status=firing'
# 证据调试端点
curl -H "Authorization: Bearer $AUTH_TOKEN" 'http://127.0.0.1:18080/debug/evidence/<incident_id>'
# 进程指标
curl 'http://127.0.0.1:18080/metrics'
```
Compose 的 Alertmanager 指向 `host.docker.internal:18080`（见 `alertmanager.yml`），因此需显式覆盖监听地址；独立运行省略 `server.listen_addr` 时安全默认是 `127.0.0.1`，未配置端口时默认 `8080`。本例统一显式使用 `18080`，改端口必须同步所有调用方。被监控 Sub2API 的示例地址 `http://127.0.0.1:8080` 是**另一服务**，不是本例的 oncall-agent listener。

配置在 YAML 环境变量展开后严格解码，未知/已删除键会在启动期报错。恢复验证在 `remediation.verification`，默认间隔 10 秒、窗口 300 秒、单次超时 5 秒、连续 3 次通过、恢复后观察 1800 秒；要求 `0 < timeout < interval < window` 且 `timeout < 30` 秒。这些是待演练校准的初值，不是实测恢复耗时。最小配置没有处置规则，不会产生任何写操作。

### 诊断、授权与执行（可选）

配置 LLM（OpenAI 兼容）后，critical/high 告警促发会自动进入诊断：

```yaml
llm:
  roles:
    reasoner:
      base_url: "https://your-openai-compatible-endpoint"
      api_key: ${ARK_KEY}
      model: "your-model"
```

- 被监控服务只在 `service` 中描述一次（容器、健康地址、管理密钥、发布入口）；动作能否执行由 `remediation.rules` 决定，每条规则列出动作、覆盖的告警、模式和预算（见 [config.example.yaml](config.example.yaml)）。没有规则覆盖、firing 告警超出规则范围或 Guard 否定前提时，保留诊断并转人工，不产生写操作。
- `manual` 规则落 `approval(pending)`。先读取审批快照（目标及身份、规则与模式、验证检查、补偿、`plan_hash`），再用操作人令牌裁决并回传该 Hash；机器令牌 `server.auth_token` 不能审批：

```bash
curl -H "Authorization: Bearer $OPERATOR_TOKEN" 'http://127.0.0.1:18080/api/v1/approvals/<id>'
curl -X POST -H "Authorization: Bearer $OPERATOR_TOKEN" -H 'Content-Type: application/json' \
  -d '{"plan_hash":"<读取到的 plan_hash>","reason":"已核对目标与规则"}' \
  'http://127.0.0.1:18080/api/v1/approvals/<id>/approve'
```

- `auto` 规则在维护窗口、规则被阻断、同一事件已执行过主要动作或监控数据不可用时降级为人工审批；急停、服务正在处置或预算耗尽时直接拒绝。领取执行时再检查一次，已排队的任务同样受急停和阻断影响；已开始的外部操作不能瞬时撤回。
- 直接 `/health` 2xx 只代表该检查通过，不代替 Incident 的告警恢复状态。数据不足或查询失败不是通过；验证失败会在重试预算内自动重诊，但同一事件的第二个动作必须由人批准。动作标错、结果未知、验证失败或复发都会阻断该规则的自动执行，直到管理员复位。
- 所有重诊入口共用事务准入：存在活跃 Run、审批或验证任务时返回 `409 active_processing`，人工重诊还有 60 秒冷却（`429 cooldown` 与 `Retry-After`）。

### Web 控制台

控制台与飞书机器人共用同一套 Incident、Run、Approval、Conversation 状态，两端都不绕过 `Plan → Guard → Policy → Approval → Executor → Verify`。

控制台没有匿名访问。每个操作人一个个人令牌，配置里只保存其 SHA-256：

```yaml
web:
  base_url: "http://127.0.0.1:18080"  # 非空即启用控制台，不控制监听或访问权限
  operator_allowlist: []              # 仅约束飞书审批卡片的操作人
  operators:
    - id: "oncall-admin"
      role: "admin"                   # viewer 只读；operator 审批、提问、重诊、标注；admin 另可切换模型、急停、复位规则
      token_sha256: "${ONCALL_ADMIN_TOKEN_SHA256}"
```

令牌用 `openssl rand -hex 32` 生成，`printf %s "$TOKEN" | shasum -a 256` 的结果写进配置，明文只交给本人。浏览器登录后换取 12 小时的 HttpOnly 会话 Cookie（进程重启后需重新登录），写请求另需 CSRF 头。审批、急停、标注等记录的是服务端确定的身份；`X-Operator` 等请求头不是身份。机器令牌只能读取、接入告警、触发诊断和登记发布。

前端产物不入库，每次构建 Go 服务前先构建前端（产物落在 `web/dist/`，由 `//go:embed` 打进二进制）：

```bash
cd web && npm ci && npm run build && cd ..
```

`.gitkeep` 仅能让空目录通过 Go 编译，不能作为可用控制台；补建前端后必须重新编译/启动 Go 服务。

打开 `http://127.0.0.1:18080/` 登录后进入概览（触发中事件、待审批、自动处置状态）。左侧导航分「事件」和「自动处置」两组，`⌘K`/`Ctrl+K` 快速跳转页面或事件。Incident 详情页是实时控制室：诊断报告、8 阶段处理流程、事件时间线 / 诊断轨迹 / 问 Agent 三个标签，右侧是审批、最近变更（执行回执/验证/观察）、当前问题、告警成员和复盘。「处置规则」「效果评估」「发布记录」分别对应规则与急停、效果报表与待复盘队列、发布与回退目标。前端默认深色主题，可在侧栏切换浅色；字体随前端打包，不依赖外部 CDN（CSP 只允许同源）。本地开发 `cd web && npm run dev` 会把 `/api` 代理到 `ONCALL_BACKEND`（默认 `http://127.0.0.1:18080`）。

Webhook、API、控制台、SSE、飞书回调和 `/metrics` **共用一个 listener**，网络规则必须限制整个 listener；`/metrics` 不带鉴权。不要暴露公网。

### 诊断模型切换

可选模型由服务端配置 `llm.models` 固定 allowlist；运行时仅能在此列表内切换，模型地址、密钥和 token 上限始终沿用各角色配置，不会下发到浏览器。未配置 `llm.models` 时，reasoner 的默认模型自动成为唯一候选。

```yaml
llm:
  roles:
    reasoner:
      model: "your-model"
  models:
    - id: "your-model"
      thinking: { enabled: false }
```

控制台顶部会显示当前模型，管理员可切换。切换会持久化为全局选择，影响后续诊断和新提问；正在运行的任务保留已创建的模型客户端。

| 端点 | 鉴权 | 说明 |
|---|---|---|
| `GET /api/v1/control-room/model` | 任一身份 | 当前模型与可选 ID（不含凭据） |
| `GET /api/v1/admin/model` | 任一身份 | 当前模型与可选 ID |
| `PUT /api/v1/admin/model` | admin | `{"model":"your-model"}` 切换全局模型 |

## API 一览

“任一身份”包括机器令牌；viewer < operator < admin，高角色包含低角色。控制台相关端点只在启用 Web 时注册。

| 端点 | 鉴权 | 说明 |
|---|---|---|
| `POST /webhook/alertmanager` | 机器令牌 | 告警接入 |
| `GET /api/v1/incidents?status=`、`GET /api/v1/incidents/{id}` | 任一身份 | incident 列表与详情（含成员） |
| `POST /api/v1/incidents/{id}/diagnose` | operator 或机器令牌 | 手动重诊，统一准入与冷却 |
| `GET /api/v1/approvals?status=`、`GET /api/v1/approvals/{id}` | 任一身份 | 审批快照展示与验证摘要 |
| `POST /api/v1/approvals/{id}/approve\|deny` | operator | 请求体含 `plan_hash`、`reason` |
| `GET /api/v1/remediation` | 任一身份 | 规则、模式、预算使用、阻断、急停与控制记录 |
| `GET /api/v1/remediation/report?days=` | 任一身份 | 效果报表与待复盘队列（1–90 天） |
| `POST /api/v1/remediation/stop\|resume` | admin | 急停 / 解除，必须填写原因 |
| `POST /api/v1/remediation/rules/{rule}/reset` | admin | 复位规则阻断，必须填写原因 |
| `GET\|POST /api/v1/incidents/{id}/reviews` | 读：任一身份；写：operator | 复盘标注，评审人取自登录身份 |
| `GET\|POST /api/v1/changes` | 读：任一身份；写：operator 或机器令牌 | 发布记录；CI 用机器令牌登记发布 |
| `POST /api/v1/changes/{id}/verify` | operator | 人工确认发布健康，只有这类发布能作为回退目标 |
| `GET /debug/evidence/{id}` | admin 或机器令牌 | 证据调试 |
| `GET /metrics` | 无 | 进程指标，仅供内网 Prometheus 抓取 |
| `GET\|POST\|DELETE /api/v1/session` | 个人令牌 / 会话 | 登录换取会话 Cookie、当前身份、登出 |
| `GET /api/v1/control-room/incidents?status=&limit=` | 任一身份 | Incident 列表（DTO） |
| `GET /api/v1/incidents/{id}/control-room`、`/events`、`/problems`、`/runs` | 任一身份 | 作战台首屏与明细 |
| `GET /api/v1/incidents/{id}/stream` | 任一身份 | SSE 实时事件 |
| `GET /api/v1/incidents/{id}/conversation` | 任一身份 | 对话历史 |
| `POST /api/v1/incidents/{id}/questions\|rediagnose\|request-evidence` | operator | 提问、重新诊断、请求补充证据 |
| `GET /api/v1/observability/dashboards`、`/dashboards/{uid}` | viewer（不含机器令牌） | 监控页看板定义，与 Grafana provisioning 同一份 JSON |
| `POST /api/v1/prometheus/query_range` | viewer（不含机器令牌） | 监控页的 PromQL 区间查询代理：表达式 ≤ 4 KB，30 秒超时 |
| `POST /integrations/feishu/events` | 飞书验签 | 飞书事件与卡片回调 |

## 验证命令

```bash
(cd web && npm ci && npm run typecheck && npm run build)
(cd web && npx playwright install chromium && npm test)
# 必须是已执行全部迁移的独立、可丢弃测试库，不要使用业务库。
export TEST_MYSQL_DSN='<存储/队列测试库 DSN>'
export TEST_API_MYSQL_DSN='<Web/飞书裁决测试库 DSN>'
export TEST_DIAGNOSE_MYSQL_DSN='<诊断/验证测试库 DSN>'
go test -count=1 -race ./...
TEST_PROMETHEUS_URL="http://127.0.0.1:9090" go test ./internal/tools  # 可选真实 Prometheus
go build ./... && go vet ./...
# 冻结回放一次已记录的诊断：只用录制的工具输出，动作只计划不执行
go run ./cmd/replay -config config.yaml -run <agent_run_id>
```

三个测试 DSN 分别用于存储、API、诊断包，不能共用同一队列库；缺少对应变量会跳过该组 MySQL 测试，不能代替事务验证。故障注入使用条件 MySQL trigger + `SIGNAL`，测试账号需要 `TRIGGER`；开启 binlog 时，测试实例还需允许创建触发器（CI 由 root 设置 `log_bin_trust_function_creators=1`，**不可照搬到生产**）。CI 设置全部三个 DSN，并另建 empty/legacy 升级库。前端契约测试拦截 API，真实后端/真实依赖/浏览器验收另见 [执行安全验收记录](docs/execution-trust-verification.md)，其中区分验证范围和未运行的外部服务。

## 执行安全升级

仅在停止 server、停止外部写入并完成数据库备份后，运行离线维护命令：

```bash
MYSQL_DSN=... go run ./cmd/retire-approvals -apply
```

命令适用于 migration 009 前/后：旧 pending/approved 审批转 expired 并写事件，结果未知的旧 executing 转 failed + `manual_check`；不会修改现代审批快照，也不会补造历史验证。按[执行安全升级说明](docs/execution-trust-upgrade.md)继续离线迁移至 014（执行快照随之升级到版本 3，旧快照在领取时失效）、按新配置结构重写配置、先构建前端再构建 Go；同一业务库只运行一个 server，禁止新旧进程重叠消费。

## 文档

- [排查技能、知识库、拓扑与监控设计](docs/skills-knowledge-topology-observability-design.md)：Skills、Tool Search、知识库、拓扑、Loki/Grafana 的取舍、实施顺序与效果评估；待实施方案
- [生产自动处置实施方案](docs/production-auto-remediation-plan.md)：多动作自动处置的设计、阶段与演练矩阵；实施状态见文首
- [生产部署](deploy/README.md)：阶段 A 核对清单、systemd、监控栈、密钥、心跳与上线顺序
- [核心问答与答辩指南](docs/project-qa.md)：面试、评审与技术答辩高频 24 问及源码解析
- [当前架构](docs/current-architecture.md)：现状分层、数据流与关键不变量
- [代码阅读指南](docs/code-reading-guide.md)：按链路顺序的源码导读
- [AWS 部署与 k6 压测报告（2026-09-17）](docs/load-test-2026-09-17.md)：受限资源下的接收/消费能力、积压、风险与复现步骤
- [执行安全与恢复验证设计](docs/execution-trust-design.md)：原始设计与待逐项核验的验收标准，现状以源码为准
- [执行安全升级说明](docs/execution-trust-upgrade.md)：停机、备份、旧审批退役、迁移与回退
- [系统设计](docs/ai-opus-system-design.md)
- [Web 控制台与飞书协同接入方案](docs/web-feishu-control-room-design.md)：实时流程、问题面板、Incident 对话、Web/飞书统一审批与回调设计；
- [开发 SPEC](oncall-agent-开发SPEC.md)
- 历史过程文档（14 天计划与每日实现记录）已归档在 `docs/archive/`，仅供追溯，不再随代码更新

## 安全边界

- `internal/store` 是唯一数据库访问边界；跨表状态变更同事务；
- LLM 只能调用 L1 只读工具（`Registry.ForLLM()`），LLM 输出不构成权限结论；
- 审批 Hash 绑定 tool/args/不可变执行上下文，裁决与领取时重验 Hash、TTL、目标/配置/故障范围；Hash 是内容绑定，不是身份签名；
- 执行结果与验证任务、验证终态与审计/记忆/必要重诊分别原子提交；结果未知的外部动作不自动重放，要求人工核查；
- 不保证外部动作与数据库的 exactly-once、不支持同库多实例，也不保证 IM 消息必达；
- Token/DSN/密钥进日志、数据库、Prompt 前一律脱敏；
- migration 手动执行，不用 AutoMigrate。

## 许可证

AGPL-3.0，见 [LICENSE](LICENSE)。控制台的监控页（原生渲染 Grafana 看板、PromQL 查询代理、时间范围选择）的代码拷自 [ongrid](https://github.com/ongridio/ongrid)（AGPL-3.0），逐个文件的来源和改动见 [NOTICE](NOTICE) 与各文件头。
