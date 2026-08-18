# oncall-agent

面向生产告警的 On-Call Agent。项目目标是把 Alertmanager 告警接入、去重、incident 聚合、证据收集、诊断和人工审批串成一条可审计的处理链路。

当前仓库已完成 M0：D01-D03 的工程骨架、Alertmanager 摄入、持久化和两级去重均已接通。

## 当前进度

| 阶段 | 内容 | 状态 |
|---|---|---|
| D01 | Go 工程骨架、配置、MySQL schema、GORM 映射、开发容器 | 已完成 |
| D02 | Alertmanager v4 归一化、fingerprint、full hash、severity 纯函数 | 已完成 |
| D03 | webhook、raw event、两级去重 worker、simulate | 已完成 |
| D04+ | incident 聚合、诊断、工具、审批、记忆和通知 | 规划中 |

D03 的核心入口是 `POST /webhook/alertmanager`，请求先写 `raw_event(pending)`，再由 worker 按 ID 顺序处理。

## 技术栈

- Go 1.24
- MySQL 8
- GORM
- GoFrame v2（已锁定依赖，业务路由尚未接入）
- Prometheus、Alertmanager、node_exporter
- YAML 配置与环境变量展开

## 目录结构

```text
.
├── cmd/
│   ├── server/                 # 服务启动入口
│   └── simulate/               # D03 前的状态提示入口
├── internal/
│   ├── api/                    # HTTP API 边界
│   ├── approval/               # 审批生命周期
│   ├── config/                 # 配置加载、环境变量展开和校验
│   ├── diagnose/               # 诊断流水线
│   ├── incident/               # incident 状态机
│   ├── ingest/                 # 告警解析、指纹和哈希
│   ├── llm/                    # LLM 角色与调用边界
│   ├── memory/                 # 故障记忆
│   ├── notify/                 # IM 通知
│   ├── store/                  # 唯一允许依赖 GORM 的包
│   └── tools/                  # 只读工具边界
├── migrations/                 # 手动 SQL migration
├── docs/                       # Day1/Day2 实现与学习文档
├── config.example.yaml         # 不含真实凭据的完整配置示例
├── docker-compose.dev.yml      # MySQL + Prometheus + Alertmanager + node_exporter
└── oncall-agent-开发SPEC.md    # 按人日拆分的开发验收规范
```

`refs/` 和 `SuperBizAgent-release-2026-01-09/` 是本地参考资产，已通过 `.gitignore`
排除，不属于当前项目源码发布内容。

## 快速开始

### 环境要求

- Go 1.24 或更高版本
- Docker Desktop 或可用的 Docker daemon
- MySQL 8（可以使用项目提供的 Compose 服务）

### 启动开发依赖

```bash
docker compose -f docker-compose.dev.yml up -d
docker compose -f docker-compose.dev.yml ps
```

服务端口：

| 服务 | 地址 |
|---|---|
| MySQL | `127.0.0.1:3306` |
| Prometheus | `http://127.0.0.1:9090` |
| Alertmanager | `http://127.0.0.1:9093` |
| node_exporter | `http://127.0.0.1:9100` |

### 初始化数据库

D01-D03 使用手动 migration，不调用 GORM `AutoMigrate`：

```bash
docker compose -f docker-compose.dev.yml exec -T mysql \
  mysql -uoncall -poncall-pass oncall < migrations/001_init.sql
docker compose -f docker-compose.dev.yml exec -T mysql \
  mysql -uoncall -poncall-pass oncall < migrations/002_alert_generator_url.sql
```

`001_init.sql` 创建十张基础表；`002_alert_generator_url.sql` 为 `alert` 保留
Alertmanager `generatorURL`，供 D07 回放原始 PromQL。两份 migration 都可重复执行。

### 启动当前 server

`cmd/server` 提供带 Bearer token 鉴权的 Alertmanager webhook，并在启动和运行期间持续补账
`raw_event.status=pending` 的事件。

```bash
cat > config.yaml <<'YAML'
server:
  port: 8080
  auth_token: ${AUTH_TOKEN}
mysql:
  dsn: ${MYSQL_DSN}
ingest:
  fingerprint_fields: []
  severity_label: severity
YAML

export AUTH_TOKEN='replace-with-a-random-token'
export MYSQL_DSN='oncall:oncall-pass@tcp(127.0.0.1:3306)/oncall?parseTime=true&loc=UTC'
CONFIG_FILE=config.yaml go run ./cmd/server
```

另一个终端发送 `Ctrl-C` 或 `SIGTERM`，服务会停止 HTTP 接收、结束 worker 并关闭数据库连接。
密钥只通过环境变量传入，不要写入 YAML 或提交到 Git。

### 发送模拟告警

`cmd/simulate` 始终通过生产 webhook 入口发送严格的 Alertmanager v4 payload：

```bash
AUTH_TOKEN="$AUTH_TOKEN" go run ./cmd/simulate -n 100 -dup 0.6
AUTH_TOKEN="$AUTH_TOKEN" go run ./cmd/simulate -n 100 -dup 0.6 -resolved
```

`-dup 0.6` 生成 40 个唯一 fingerprint 和 60 个 full duplicate；`-resolved` 使用相同 labels
发送解除批次。生成的 `generatorURL` 固定指向本地 Prometheus 的 `vector(1)` 表达式。

## D02 API 示例

D02 的实现位于 `internal/ingest`，不访问数据库。

```go
alerts, err := ingest.ParseWebhook(payload)
if err != nil {
    return err
}

for i := range alerts {
    alerts[i].Fingerprint = ingest.Fingerprint(
        alerts[i].Labels,
        cfg.Ingest.FingerprintFields,
    )
    alerts[i].Severity = ingest.Severity(
        alerts[i].Labels,
        cfg.Ingest.SeverityLabel,
    )
    alerts[i].AlertHash = ingest.FullHash(alerts[i])
}
```

- `ParseWebhook`：Alertmanager v4 payload → `[]NormalizedAlert`；
- `Fingerprint`：按 label 计算 SHA-256 fingerprint；
- `FullHash`：排除时间字段后计算 MD5；
- `Severity`：将 critical/high/warning/info/low 映射为 5/4/3/2/1，未知值默认为 3。

D02 学习和实现细节见：

- [Day1 实现文档](docs/day1-implementation.md)
- [Day2 实现文档](docs/day2-implementation.md)
- [开发 SPEC](oncall-agent-开发SPEC.md)

## 验证命令

```bash
go test ./internal/ingest ./internal/api ./cmd/simulate
TEST_MYSQL_DSN="$MYSQL_DSN" go test ./internal/store
go test ./...
go build ./...
go vet ./...
```

`internal/store` 集成测试会使用唯一 fingerprint，并在结束后清理自己的 `raw_event`、`alert` 和 `last_alert` 行。

## 配置与安全边界

- `config.yaml`、`.env`、日志、编译产物和本地数据已加入 `.gitignore`；
- `config.example.yaml` 只使用环境变量占位符，不放真实 token、DSN 或 API key；
- `internal/store` 是唯一允许导入 GORM 的业务包；
- 服务错误输出不打印完整 DSN、token 或 API key；
- D03 webhook 要求精确的 `Authorization: Bearer ${AUTH_TOKEN}`；无效 token 不会写入 `raw_event`。

## 开发约束

- 每个人日独立验收并提交，提交消息使用 `D0x:` 前缀；
- 纯函数优先，核心逻辑与数据库解耦；
- 不使用 `log.Fatal` 或 `panic` 处理业务错误；
- migration 手动执行，避免运行时隐式修改 schema；
- 改动后至少运行对应包测试，并通过 `go build ./...` 与 `go vet ./...`。
