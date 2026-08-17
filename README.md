# oncall-agent

面向生产告警的 On-Call Agent。项目目标是把 Alertmanager 告警接入、去重、incident 聚合、证据收集、诊断和人工审批串成一条可审计的处理链路。

当前仓库处于基础能力阶段：D01 和 D02 已完成，D03 的 webhook、落库和 worker 尚未实现。

## 当前进度

| 阶段 | 内容 | 状态 |
|---|---|---|
| D01 | Go 工程骨架、配置、MySQL schema、GORM 映射、开发容器 | 已完成 |
| D02 | Alertmanager v4 归一化、fingerprint、full hash、severity 纯函数 | 已完成 |
| D03 | webhook、raw event、两级去重 worker、simulate | 未开始 |
| D04+ | incident 聚合、诊断、工具、审批、记忆和通知 | 规划中 |

D02 的当前提交：`1adb83f D02: normalize alerts and compute fingerprints`。

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

D01 使用手动 migration，不调用 GORM `AutoMigrate`：

```bash
docker compose -f docker-compose.dev.yml exec -T mysql \
  mysql -uoncall -poncall-pass oncall < migrations/001_init.sql
```

该 SQL 创建十张表：`raw_event`、`alert`、`last_alert`、`incident`、
`incident_alert`、`fault_memory`、`fault_cmd_history`、`approval`、`agent_run`、
`agent_run_step`。

### 启动当前 server

`cmd/server` 当前只验证配置、数据库连接和信号退出，不提供 webhook 路由。
D01 最小 smoke test 可以只创建 MySQL 配置：

```bash
cat > config.yaml <<'YAML'
mysql:
  dsn: ${MYSQL_DSN}
YAML

export MYSQL_DSN='oncall:oncall-pass@tcp(127.0.0.1:3306)/oncall?parseTime=true&loc=UTC'
CONFIG_FILE=config.yaml go run ./cmd/server
```

另一个终端发送 `Ctrl-C` 或 `SIGTERM`，服务应关闭数据库连接并正常退出。

`config.example.yaml` 展示了后续阶段的完整配置。它包含 `${AUTH_TOKEN}`、`${ARK_KEY}`、
`${IM_WEBHOOK}` 等占位符，直接加载完整示例前需要先设置对应环境变量；不要将真实凭据
写入 YAML 或提交到 Git。

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
go test ./internal/ingest
go test ./...
go build ./...
go vet ./...
```

当前 `cmd/simulate` 会明确提示 D03 尚未接入，不会伪造 Alertmanager payload。

## 配置与安全边界

- `config.yaml`、`.env`、日志、编译产物和本地数据已加入 `.gitignore`；
- `config.example.yaml` 只使用环境变量占位符，不放真实 token、DSN 或 API key；
- `internal/store` 是唯一允许导入 GORM 的业务包；
- 服务错误输出不打印完整 DSN、token 或 API key；
- D03 的 webhook 和审批接口需要 Bearer token 鉴权，当前尚未实现。

## 开发约束

- 每个人日独立验收并提交，提交消息使用 `D0x:` 前缀；
- 纯函数优先，核心逻辑与数据库解耦；
- 不使用 `log.Fatal` 或 `panic` 处理业务错误；
- migration 手动执行，避免运行时隐式修改 schema；
- 改动后至少运行对应包测试，并通过 `go build ./...` 与 `go vet ./...`。
