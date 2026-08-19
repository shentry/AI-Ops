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
- Docker（Compose 提供 MySQL/Prometheus/Alertmanager/node_exporter）

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

- 批准的 `docker_restart` 由执行器真实执行并独立 Verify；Verify 失败进入有限重诊，超限升级人工；
- 默认 `approval.dry_run: true`、`auto_execute_l2: false`（见 config.example.yaml）——先演练再放开。

## API 一览

| 端点 | 说明 |
|---|---|
| `POST /webhook/alertmanager` | 告警接入（Bearer 鉴权） |
| `GET /api/v1/incidents?status=` | incident 列表 |
| `GET /api/v1/incidents/{id}` | incident 详情（含成员） |
| `POST /api/v1/incidents/{id}/diagnose` | 手动重诊 |
| `GET /api/v1/approvals?status=` | 审批单列表 |
| `POST /api/v1/approvals/{id}/approve\|deny` | 审批决策（需 X-Operator） |
| `GET /debug/evidence/{id}` | 证据调试 |
| `GET /metrics` | 进程指标 |

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
- [开发 SPEC](oncall-agent-开发SPEC.md)
- 每日实现文档：`docs/day1-implementation.md` … `docs/day13-implementation.md`

## 安全边界

- `internal/store` 是唯一数据库访问边界；跨表状态变更同事务；
- LLM 只能调用 L1 只读工具（`Registry.ForLLM()`），LLM 输出不构成权限结论；
- 审批绑定 tool/args/plan_hash/过期时间，执行前重算校验；篡改即拒执；
- Token/DSN/密钥进日志、数据库、Prompt 前一律脱敏；
- migration 手动执行，不用 AutoMigrate。
