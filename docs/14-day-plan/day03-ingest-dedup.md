# Day03：Webhook、可靠摄入和两级去重

> 阶段：P0 告警核心  
> 状态：已完成，待统一回归  
> 依赖：Day02  
> 详细实现记录：[Day3 实现文档](../day3-implementation.md)

## 当日目标

打通 Alertmanager 到 MySQL 的可靠摄入链路，实现进程重启可补账的异步 worker 和两级去重。

## 实现清单

- [x] 实现 `POST /webhook/alertmanager` 和 Bearer Token 鉴权；
- [x] 原始 JSON 成功写入 `raw_event(pending)` 后返回 HTTP 202；
- [x] 使用非阻塞 channel 唤醒 ingest worker；
- [x] worker 启动时和运行中扫描 pending 事件补账；
- [x] 在 worker 中按配置重算 fingerprint、hash 和 severity；
- [x] 实现 new、partial、full 两级去重；
- [x] full duplicate 只刷新当前快照的存活时间；
- [x] 输入错误标记 failed，数据库错误保留 pending；
- [x] `cmd/simulate` 使用真实 Alertmanager v4 格式；
- [ ] 在当前工作区重新执行当日回归命令。

## 关键文件

- `internal/api/alertmanager.go`
- `internal/ingest/worker.go`
- `internal/store/store.go`
- `cmd/simulate/main.go`
- `cmd/server/main.go`
- `migrations/002_alert_generator_url.sql`

## 验收清单

- [ ] 错误 Token 返回 401，非法 JSON 返回 400；
- [ ] 数据库不可用时不返回虚假 202；
- [ ] `simulate -n 100 -dup 0.6` 后去重结果符合预期；
- [ ] full duplicate 不新增 `alert` 行，只更新 `last_seen`；
- [ ] partial duplicate 新增历史并更新 `last_alert`；
- [ ] 重启进程后 pending 事件能继续处理；
- [ ] `go test ./... && go build ./... && go vet ./...` 通过。

## 不包含

- Incident 生命周期；
- Agent Run、LLM、审批和修复执行。
