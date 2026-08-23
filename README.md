# oncall-agent（AI-Opus）

面向 Prometheus/Alertmanager 告警的 V1 自愈系统：告警接入 → 去重 → incident 归并 → 证据采集 → LLM 诊断 → Guard/Policy 决策 → 审批 → 受控执行 → 恢复验证 → 故障记忆，全链路可审计回放。

V1 目标环境是 Sub2API 测试环境（网关 + PostgreSQL + Redis + 宿主机）；AI-Opus 自身只依赖 MySQL，不引入 Redis/MQ/向量数据库。

## 功能概览

- **摄入**：Alertmanager v4 webhook，先落 `raw_event(pending)` 再异步处理，崩溃可重放；
- **归并**：fingerprint 两级去重 + group key 时间窗归并，candidate 攒够阈值促发 firing；
- **生命周期**：全部成员 resolved 时 incident 自动关单；
- **诊断**：agent_run 队列驱动独立诊断 worker（摄入不阻塞）；证据由代码采集（0 次 LLM），Eino ReAct 只在受控步数内推理；
- **安全**：L1-L4 分级 + Guard 纠偏 + Policy 决策 + plan_hash 绑定审批；L4 永远禁止；
- **执行**：approved 审批单 → 原子领取 → hash 校验 → 注册表工具执行 → 回写 → 独立 Verify；
- **记忆**：验证成功的高置信案例入库；同类故障命中时 0 次 LLM（memory_hit），命中失败自动降级拉黑；
- **可观测**：`/metrics`（Prometheus 文本格式）+ `agent_run_step` 逐步审计。

## 快速开始

### 环境要求

- Go 1.24+
- Docker（Compose 提供 MySQL/Prometheus/Alertmanager/blackbox/node_exporter）

### 三步跑通

```bash
# 1. 起依赖 + 建表
docker compose -f docker-compose.dev.yml up -d
for f in migrations/00*.sql; do
  docker compose -f docker-compose.dev.yml exec -T mysql \
    mysql -uoncall -poncall-pass oncall < "$f"
done

# 2. 最小配置并启动
cat > config.yaml <<'YAML'
server:
  port: 8080
  auth_token: ${AUTH_TOKEN}
mysql:
  dsn: ${MYSQL_DSN}
YAML
export AUTH_TOKEN='replace-with-a-random-token'
export MYSQL_DSN='oncall:oncall-pass@tcp(127.0.0.1:3306)/oncall?parseTime=true&loc=UTC'
CONFIG_FILE=config.yaml go run ./cmd/server

# 3. 注入模拟故障
AUTH_TOKEN="$AUTH_TOKEN" go run ./cmd/simulate -n 3 -dup 0        # firing
AUTH_TOKEN="$AUTH_TOKEN" go run ./cmd/simulate -n 3 -dup 0 -resolved  # resolved
```

观察：

```bash
# incident 列表与详情（含成员）
curl -H "Authorization: Bearer $AUTH_TOKEN" 'http://127.0.0.1:8080/api/v1/incidents?status=firing'
# 证据调试端点
curl -H "Authorization: Bearer $AUTH_TOKEN" 'http://127.0.0.1:8080/debug/evidence/<incident_id>'
# 进程指标
curl 'http://127.0.0.1:8080/metrics'
```

本地 `config.yaml` 若把 `server.port` 设为 `18080`，Alertmanager webhook 也要指向同一端口（见 `alertmanager.yml`）。Compose 用 blackbox 探 `Sub2API /health` 以及本机 SSH 隧道上的 Postgres/Redis，Prometheus 规则在 `alerts.yml`，firing 经 Alertmanager 推入 `/webhook/alertmanager`。

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

- L3/降级 L2 计划落 `approval(pending)`，人工经 API 审批：

```bash
curl -X POST -H "Authorization: Bearer $AUTH_TOKEN" -H 'X-Operator: <你的工号>' \
  http://127.0.0.1:8080/api/v1/approvals/<id>/approve
```

- 批准的 `docker_restart` 由执行器真实执行并独立 Verify；Verify 失败进入有限重诊，超限升级人工；Verify 不可判定（进程关闭、读库失败、没有可复查的成员告警）既不算成功也不算失败，只留痕等人工核查；
- 默认 `approval.dry_run: true`、`auto_execute_l2: false`（见 config.example.yaml）——先演练再放开；
- 放开 L2 自动执行后仍需同时满足七条护栏：白名单命中、target 来自告警标签、影响范围是单个具体对象、限频未超、全局开关开启、非 dry-run、结果可验证。任一不满足自动降级为人工审批，降级原因写在审批单上。`docker_restart` 在工具层另有独立限频（最小间隔 + 每小时上限）。

### Web 控制台

控制台与飞书机器人共用同一套 Incident、Run、Approval、Conversation 状态，两端都不绕过 `Plan → Guard → Policy → Approval → Executor → Verify`。

本机或可信内网免登录进入，不需要飞书 OAuth、浏览器会话或 CSRF token：

```yaml
web:
  base_url: "http://127.0.0.1:8080"  # 非空即启用控制台
  trusted_operator: "oncall"          # 兼容旧配置；不再作为登录身份
```

打开 `http://127.0.0.1:8080/` 即进入 Incident 列表，点进去是实时作战台（流程节点、事件时间线、问题面板、Step 检查器、审批、对话）。控制台所有读写操作都以 `anonymous` 记录，任何能访问该端口的人都可批准、重诊、提问和请求补充证据。

这是真正的公开操作面，只在回环地址或完全可信网络使用。`AUTH_TOKEN` 仍只保护 Alertmanager、传统 Incident API 和证据调试接口；不要把服务端口暴露到公网。

### 诊断模型切换

可选模型由服务端配置 `llm.models` 固定 allowlist；运行时仅能在此列表内切换，模型地址、密钥和 token 上限始终沿用各角色配置，不会下发到浏览器。未配置 `llm.models` 时，reasoner 的默认模型自动成为唯一候选。

```yaml
llm:
  roles:
    reasoner:
      model: "glm-5"
  models:
    - id: "glm-5"
      thinking: { enabled: false }
```

控制台顶部会显示当前模型；输入 `AUTH_TOKEN` 后可切换。令牌只随本次 HTTPS 请求发送，不写入浏览器存储。切换会持久化为全局选择，影响后续诊断和新提问；正在运行的任务保留已创建的模型客户端。

| 端点 | 鉴权 | 说明 |
|---|---|---|
| `GET /api/v1/control-room/model` | Web 启用时公开 | 当前模型与可选 ID（不含凭据） |
| `GET /api/v1/admin/model` | Bearer | 当前模型与可选 ID |
| `PUT /api/v1/admin/model` | Bearer | `{"model":"glm-5"}` 切换全局模型 |

## API 一览

| 端点 | 说明 |
|---|---|
| `POST /webhook/alertmanager` | 告警接入（Bearer 鉴权） |
| `GET /api/v1/incidents?status=` | incident 列表 |
| `GET /api/v1/incidents/{id}` | incident 详情（含成员） |
| `POST /api/v1/incidents/{id}/diagnose` | 手动重诊 |
| `GET /api/v1/approvals?status=` | Web 启用时公开；未启用 Web 时需 Bearer |
| `POST /api/v1/approvals/{id}/approve\|deny` | Web 启用时匿名审批；自动化调用仍可用 Bearer + `X-Operator` |
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
go test ./...
TEST_MYSQL_DSN="$MYSQL_DSN" go test ./internal/store          # MySQL 集成测试
TEST_PROMETHEUS_URL="http://127.0.0.1:9090" go test ./internal/tools  # 真实 Prometheus
go build ./... && go vet ./...
```

## 文档

- [14 天实施计划](docs/14-day-plan/README.md)：每日清单、全局约束与验收记录
- [系统设计](docs/ai-opus-system-design.md)
- [Web 控制台与飞书协同接入方案](docs/web-feishu-control-room-design.md)：实时流程、问题面板、Incident 对话、Web/飞书统一审批与回调设计；
- [开发 SPEC](oncall-agent-开发SPEC.md)
- 每日实现文档：`docs/day1-implementation.md` … `docs/day13-implementation.md`

## 安全边界

- `internal/store` 是唯一数据库访问边界；跨表状态变更同事务；
- LLM 只能调用 L1 只读工具（`Registry.ForLLM()`），LLM 输出不构成权限结论；
- 审批绑定 tool/args/plan_hash/过期时间，执行前重算校验；篡改即拒执；
- Token/DSN/密钥进日志、数据库、Prompt 前一律脱敏；
- migration 手动执行，不用 AutoMigrate。
