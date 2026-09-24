# oncall-agent（AI-Ops）

面向 Prometheus/Alertmanager 告警的 V1 自愈系统：告警接入 → 去重 → incident 归并 → 证据采集 → LLM 诊断 → Guard/Policy 决策 → 审批 → 受控执行 → 恢复验证 → 故障记忆，全链路可审计回放。

V1 目标环境是 Sub2API 测试环境（网关 + PostgreSQL + Redis + 宿主机）；AI-Opus 自身只依赖 MySQL，不引入 Redis/MQ/向量数据库。

## 功能概览

- **摄入**：Alertmanager v4 webhook，先落 `raw_event(pending)` 再异步处理，崩溃可重放；
- **归并**：fingerprint 两级去重 + group key 时间窗归并，candidate 攒够阈值促发 firing；
- **生命周期**：全部成员 resolved 时 incident 自动关单；
- **诊断**：agent_run 队列驱动独立诊断 worker（摄入不阻塞）；证据由代码采集（0 次 LLM），ReAct 按每次请求的上下文预算压缩旧工具结果，步数与超时作为兜底；
- **安全**：L1-L4 分级 + Guard/Policy；`incident.PlanHash` 绑定工具、参数和不可变 `ExecutionContext`（安全等级、演练模式、验证目标与时长），L4 永远禁止；
- **准入与发布**：告警、人工重诊、验证失败重诊共用 `RequestRun`；`CompleteRun` 同事务发布诊断结论、审批与审计；
- **执行与验证**：Executor 只执行变更、提交结果；真实执行成功同事务创建 `verify_task`，独立 `VerificationWorker` 进行有界健康检查；演练记为 `simulated`，不创建验证任务；
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
# 1. 起依赖 + 空库顺序执行全部迁移（001–011）
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

配置在 YAML 环境变量展开后严格解码，未知/已删除键会在启动期报错。独立验证默认 `diagnose.verification.interval_seconds/window_seconds/timeout_seconds` 为 `10/120/5`，要求 `0 < timeout < interval < window` 且 `timeout < 30` 秒；与 `diagnose.evidence.timeout_seconds` 无关。这些是调度默认值，不是实测恢复耗时。

### 诊断与审批链路（可选）

配置 LLM（OpenAI 兼容）后，critical/high 告警促发会自动进入诊断：

```yaml
llm:
  roles:
    reasoner:
      base_url: "https://your-openai-compatible-endpoint"
      api_key: ${ARK_KEY}
      model: "your-model"
```

- 当前仅允许已配置且在白名单内的 Sub2API 单容器 `docker_restart`，当前 firing 成员必须都是该目标的 `Sub2APIDown`。Slow、依赖或混合故障保留诊断并转人工处理，不发布本轮无法验证的变更审批。
- 支持范围内的降级 L2（或注册为 L3 的动作）落 `approval(pending)`。先读取审批的目标、范围、`safety_level`、`dry_run` 和 `plan_hash`，人工裁决必须回传该 Hash：

```bash
curl -H "Authorization: Bearer $AUTH_TOKEN" 'http://127.0.0.1:18080/api/v1/approvals/<id>'
curl -X POST -H "Authorization: Bearer $AUTH_TOKEN" -H 'X-Operator: <操作备注>' \
  -H 'Content-Type: application/json' \
  -d '{"plan_hash":"<读取到的 plan_hash>","reason":"已核对目标与模式"}' \
  'http://127.0.0.1:18080/api/v1/approvals/<id>/approve'
```

- 执行使用批准时的快照；`dry_run=true` 只记 `simulated`，真实成功记 `executed` 并入队验证。直接 `/health` 2xx 只代表该健康检查通过，不代替 Incident 的告警恢复状态。窗口结束时仍有新鲜的截止前不健康观测才可判失败，失败最多自动重诊两次；不可判定转人工核查，不降级记忆或自动重诊。停机取消/读库失败留下可恢复任务，不伪造验证终态；
- 默认 `approval.dry_run: true`、`auto_execute_l2: false`（见 config.example.yaml）——先演练再放开；
- 白名单、故障范围与验证绑定是人工/自动共同前置条件；不满足直接拒绝计划。通过后，L2 自动路径再检查全局开关、非 dry-run 与限频，未通过则降级人工审批。`docker_restart` 工具层另有限频（最小间隔 + 每小时上限）。
- 所有重诊入口共用事务准入：存在活跃 Run、审批或验证任务时返回 `409 active_processing`，人工重诊还有 60 秒冷却（`429 cooldown` 与 `Retry-After`）。

### Web 控制台

控制台与飞书机器人共用同一套 Incident、Run、Approval、Conversation 状态，两端都不绕过 `Plan → Guard → Policy → Approval → Executor → Verify`。

本机或可信内网免登录进入，不需要飞书 OAuth、浏览器会话或 CSRF token：

```yaml
web:
  base_url: "http://127.0.0.1:18080"  # 非空即启用控制台，不控制监听或访问权限
  operator_allowlist: []              # 仅约束飞书审批卡片的操作人
```

前端产物不入库，每次构建 Go 服务前先构建前端（产物落在 `web/dist/`，由 `//go:embed` 打进二进制）：

```bash
cd web && npm ci && npm run build && cd ..
```

`.gitkeep` 仅能让空目录通过 Go 编译，不能作为可用控制台；补建前端后必须重新编译/启动 Go 服务。

打开 `http://127.0.0.1:18080/` 即进入 Incident 列表，点进去是实时作战台（流程节点、事件时间线、问题面板、Step 检查器、审批、执行/验证双状态、对话）。控制台所有读写操作都以 `anonymous` 记录，任何能访问该端口的人都可批准、重诊、提问和请求补充证据。

这是可信网络中的匿名操作面，没有个人身份认证、授权或个人追责保证；`X-Operator`/填写姓名也只是调用方声明。Webhook、API、控制台、SSE、飞书回调和 `/metrics` **共用一个 listener**，网络规则必须限制整个 listener，不能只隐藏首页。`web.base_url`、Bearer 自动化凭据与飞书验签/操作人白名单都不保护匿名 Web 写接口；不要暴露公网，也不能把此共享 listener 当作“Webhook 对外、控制台对内”的隔离。

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

控制台顶部会显示当前模型；输入 `AUTH_TOKEN` 后可切换。令牌只随本次 HTTPS 请求发送，不写入浏览器存储。切换会持久化为全局选择，影响后续诊断和新提问；正在运行的任务保留已创建的模型客户端。

| 端点 | 鉴权 | 说明 |
|---|---|---|
| `GET /api/v1/control-room/model` | Web 启用时公开 | 当前模型与可选 ID（不含凭据） |
| `GET /api/v1/admin/model` | Bearer | 当前模型与可选 ID |
| `PUT /api/v1/admin/model` | Bearer | `{"model":"your-model"}` 切换全局模型 |

## API 一览

| 端点 | 说明 |
|---|---|
| `POST /webhook/alertmanager` | 告警接入（Bearer 鉴权） |
| `GET /api/v1/incidents?status=` | incident 列表 |
| `GET /api/v1/incidents/{id}` | incident 详情（含成员） |
| `POST /api/v1/incidents/{id}/diagnose` | Bearer 手动重诊，统一准入与冷却 |
| `GET /api/v1/approvals?status=` | Web 启用时公开；未启用 Web 时需 Bearer |
| `GET /api/v1/approvals/{id}` | 审批快照展示与验证摘要；鉴权同列表 |
| `POST /api/v1/approvals/{id}/approve\|deny` | 请求体含 `plan_hash`、`reason`；Web 匿名或 Bearer + `X-Operator` |
| `GET /debug/evidence/{id}` | 证据调试（Bearer 鉴权） |
| `GET /metrics` | 进程指标 |
| `GET /api/v1/control-room/incidents?status=&limit=` | Web 启用时公开 Incident 列表（DTO） |
| `GET /api/v1/incidents/{id}/control-room` | Web 启用时公开作战台首屏 |
| `GET /api/v1/incidents/{id}/stream` | Web 启用时公开 SSE 实时事件 |
| `GET /api/v1/incidents/{id}/conversation` | Web 启用时公开对话历史 |
| `POST /api/v1/incidents/{id}/questions` | Web 启用时公开提问 Agent |
| `POST /api/v1/incidents/{id}/rediagnose` | Web 启用时公开重新诊断 |
| `POST /api/v1/incidents/{id}/request-evidence` | Web 启用时公开请求补充证据 |
| `POST /integrations/feishu/events` | 飞书事件与卡片回调（SDK 验签解密） |

## 验证命令

```bash
(cd web && npm ci && npm run typecheck && npm run build)
(cd web && npx playwright install chromium && npm test)
# 必须是已执行 001–011 的独立、可丢弃测试库，不要使用业务库。
export TEST_MYSQL_DSN='<存储/队列测试库 DSN>'
export TEST_API_MYSQL_DSN='<Web/飞书裁决测试库 DSN>'
export TEST_DIAGNOSE_MYSQL_DSN='<诊断/验证测试库 DSN>'
go test -count=1 -race ./...
TEST_PROMETHEUS_URL="http://127.0.0.1:9090" go test ./internal/tools  # 可选真实 Prometheus
go build ./... && go vet ./...
```

三个测试 DSN 分别用于存储、API、诊断包，不能共用同一队列库；缺少对应变量会跳过该组 MySQL 测试，不能代替事务验证。故障注入使用条件 MySQL trigger + `SIGNAL`，测试账号需要 `TRIGGER`；开启 binlog 时，测试实例还需允许创建触发器（CI 由 root 设置 `log_bin_trust_function_creators=1`，**不可照搬到生产**）。CI 设置全部三个 DSN，并另建 empty/legacy 升级库。前端契约测试拦截 API，真实后端/真实依赖/浏览器验收另见 [执行安全验收记录](docs/execution-trust-verification.md)，其中区分验证范围和未运行的外部服务。

## 执行安全升级

仅在停止 server、停止外部写入并完成数据库备份后，运行离线维护命令：

```bash
MYSQL_DSN=... go run ./cmd/retire-approvals -apply
```

命令适用于 migration 009 前/后：旧 pending/approved 审批转 expired 并写事件，结果未知的旧 executing 转 failed + `manual_check`；不会修改现代审批快照，也不会补造历史验证。按[执行安全升级说明](docs/execution-trust-upgrade.md)继续迁移至 011、更新严格配置、先构建前端再构建 Go；同一业务库只运行一个 server，禁止新旧进程重叠消费。

## 文档

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
