# Day07：Sub2API Evidence Collector

> 阶段：P1 Agent 诊断  
> 状态：已完成
> 依赖：Day06  
> 详细实现记录：[Day7 实现文档](../day7-implementation.md)

## 当日目标

在调用 LLM 前由代码收集可验证证据，覆盖 Sub2API、PostgreSQL、Redis、宿主机和 Prometheus 回放。

## 实现清单

- [x] 定义 `Evidence`、`EvidenceItem` 和 collector 接口；
- [x] 收集 Incident 成员、`last_alert` 快照、labels 和 annotations；
- [x] 从 `generatorURL` 安全解析 PromQL，并按告警时间前后窗口回放；
- [x] 收集 Sub2API `/health`、请求量、5xx 和延迟；
- [x] 收集 PostgreSQL 连通性、连接数和等待摘要；
- [x] 收集 Redis ping、延迟、内存和连接数；
- [x] 收集宿主机 CPU、内存、磁盘和网络黄金指标；
- [x] 收集 Docker 容器状态和时间窗口内的受限日志；
- [x] 每个 collector 独立超时、限长和记录错误；
- [x] `Evidence.Render()` 输出脱敏、稳定、有来源引用的文本；
- [x] 单个 collector 失败不能阻断其他证据收集。

## 关键文件

- `internal/diagnose/evidence.go`
- `internal/diagnose/collectors/`
- `internal/tools/prometheus.go`
- `internal/tools/docker.go`
- `internal/store/store.go`

## 验收清单

- [x] 对一条 Sub2API 测试告警生成完整 Evidence（本地环境以模拟告警验证全链路，Sub2API/PG/Redis 段按 missing 记录）；
- [x] Evidence 中每一项都有来源、采集时间和截断状态；
- [x] 停掉 Prometheus 后只缺失 Prometheus 段，collector 总体仍返回；
- [x] 日志中的 Token、DSN、Cookie、API Key 被脱敏；
- [x] 超长日志和二进制内容被安全截断；
- [x] 证据中包含的用户输入不会被当作系统指令；
- [x] `go test ./... && go build ./... && go vet ./...` 通过。

## 不包含

- LLM 推理；
- 自动执行；
- 向量检索和语义记忆。
