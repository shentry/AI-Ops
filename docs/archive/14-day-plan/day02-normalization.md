# Day02：告警归一化、指纹和严重度

> 阶段：P0 告警核心  
> 状态：已完成，待统一回归  
> 依赖：Day01  
> 详细实现记录：[Day2 实现文档](../day2-implementation.md)

## 当日目标

将 Alertmanager v4 payload 转换为稳定的内部告警模型，完成无数据库依赖的身份与内容计算。

## 实现清单

- [x] 实现 `ParseWebhook` 并校验 Alertmanager v4 格式；
- [x] 归一化 status、labels、annotations 和时间字段；
- [x] label key 小写化，value 保持原值；
- [x] 实现基于排序 labels 的 SHA-256 `fingerprint`；
- [x] 实现排除时间字段的 MD5 `alert_hash`；
- [x] 实现 severity 5 到 1 映射，未知值默认 warning；
- [x] 保证 firing/resolved 状态不参与 fingerprint；
- [x] 用表驱动测试覆盖顺序无关和非法输入；
- [ ] 在当前工作区重新执行当日回归命令。

## 关键文件

- `internal/ingest/types.go`
- `internal/ingest/webhook.go`
- `internal/ingest/fingerprint.go`
- `internal/ingest/severity.go`
- `internal/ingest/*_test.go`

## 验收清单

- [ ] `go test ./internal/ingest` 通过；
- [ ] labels 顺序变化不改变 fingerprint；
- [ ] 同一 labels 的 firing/resolved fingerprint 相同；
- [ ] annotations 变化不改变 fingerprint，但会改变 full hash；
- [ ] 非法 JSON、未知版本和非法 status 返回错误且不 panic；
- [ ] 纯函数测试不需要 MySQL 或网络。

## 不包含

- HTTP 路由和鉴权；
- `raw_event`、`alert`、`last_alert` 持久化；
- 去重 worker 和 Incident 归并。
