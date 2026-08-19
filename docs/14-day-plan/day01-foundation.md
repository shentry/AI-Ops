# Day01：项目脚手架与基础设施

> 阶段：基础设施  
> 状态：已完成，待统一回归  
> 详细实现记录：[Day1 实现文档](../day1-implementation.md)

## 当日目标

建立可编译、可配置、可连接 MySQL、可启动开发依赖的 Go 项目，为后续告警处理提供稳定边界。

## 实现清单

- [x] 初始化 `oncall-agent` Go module 和 `cmd/server`、`internal/*` 包结构；
- [x] 使用 YAML 加载配置并支持 `${ENV_NAME}` 展开；
- [x] `MYSQL_DSN` 缺失时启动失败，不使用隐式降级；
- [x] 通过手工 migration 创建十张核心表；
- [x] 在 `internal/store` 建立 GORM model 和数据库连接；
- [x] 提供 server 生命周期和 graceful shutdown；
- [x] 提供 MySQL、Prometheus、Alertmanager、node_exporter 的 Compose 环境；
- [x] 提供不包含真实凭据的 `config.example.yaml`；
- [ ] 在当前工作区重新执行当日回归命令。

## 关键文件

- `go.mod`
- `cmd/server/main.go`
- `internal/config/config.go`
- `internal/store/models.go`
- `internal/store/store.go`
- `migrations/001_init.sql`
- `docker-compose.dev.yml`
- `config.example.yaml`

## 验收清单

- [ ] `go test ./internal/config ./internal/store` 通过；
- [ ] `go build ./...` 通过；
- [ ] `go vet ./...` 通过；
- [ ] migration 执行后核心表数量为 10；
- [ ] 缺少 `MYSQL_DSN` 时进程返回非零退出码；
- [ ] Docker Compose 中四个基础服务可启动。

## 不包含

- Webhook、告警解析、去重和 Incident；
- LLM、审批、执行、通知和记忆。

## 完成定义

以上验收全部通过后，Day01 才可从“已完成，待回归”改为“已验收”。
