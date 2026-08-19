# Day06：Tool Registry 与 Prometheus 查询工具

> 阶段：P1 Agent 诊断  
> 状态：已完成
> 依赖：Day05  
> 详细实现记录：[Day6 实现文档](../day6-implementation.md)

## 当日目标

建立工具安全边界和 Prometheus 只读查询能力，为 Evidence Collector 和 Reasoner 提供稳定接口。

## 实现清单

- [x] 定义 `SafetyLevel`、`ToolSpec` 和 `Registry`；
- [x] 为每个工具声明名称、描述、安全等级、超时、最大输出和 handler；
- [x] `Registry.ForLLM()` 只返回 L1 只读工具；
- [x] 注册 `prom_instant_query`；
- [x] 注册 `prom_range_query`，限制时间范围、step 和最大点数；
- [x] 注册 `prom_series_meta`，支持 labels/series 元数据查询；
- [x] HTTP client 进程级复用，禁止每次工具调用新建连接池；
- [x] 增加超时 wrapper 和输出截断 wrapper；
- [x] Prometheus 返回错误时保留可审计的错误摘要，不泄露凭据；
- [x] 为 Registry、超时、截断和工具等级编写表驱动测试。

## 关键文件

- `internal/tools/registry.go`
- `internal/tools/prometheus.go`
- `internal/tools/tools.go`
- `internal/tools/*_test.go`
- `internal/config/config.go`

## 验收清单

- [x] 对本地 Prometheus 的三个接口各完成一次真实查询；
- [x] `ForLLM()` 结果不包含 L2、L3 或 L4 动作；
- [x] 慢查询在配置超时内返回错误；
- [x] 超大响应被截断到工具声明的最大长度；
- [x] range 查询不会产生无限时间范围或超过最大点数；
- [x] 工具错误不会导致 server 或 worker 退出；
- [x] `go test ./... && go build ./... && go vet ./...` 通过。

## 不包含

- 数据库变更工具；
- Docker 重启工具；
- LLM Agent 编排。
