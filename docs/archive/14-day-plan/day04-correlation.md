# Day04：Correlator 与 Incident 归并

> 阶段：P0 告警核心  
> 状态：已完成
> 依赖：Day03  
> 详细实现记录：[Day4 实现文档](../day4-implementation.md)

## 当日目标

将去重后的 firing 告警按 group key 和时间窗口归并为 Incident，并保证告警、成员和事件状态原子提交。

## 实现清单

- [x] 实现按 `correlate.group_by` 计算稳定 `GroupKey`；
- [x] 分组字段缺失时回退到 `name:<alertname>`；
- [x] 查找同 group key、时间窗口内的开放 Incident；
- [x] 窗口内归并，窗口外新建 candidate；
- [x] 通过联合主键保证成员幂等；
- [x] `alerts_count` 只统计新增 fingerprint；
- [x] Incident severity 取成员最大值；
- [x] 达到 `min_alerts` 后 candidate 仅促发一次 firing；
- [x] full duplicate 只延长已关联 Incident 的存活时间；
- [x] 告警、快照、Incident、成员和 raw event 在同一事务提交；
- [x] 重新执行单元测试、MySQL 集成测试和真实 simulate 验收；
- [x] 验收通过后将计划状态改为已完成。

## 关键文件

- `internal/ingest/correlate.go`
- `internal/ingest/correlate_test.go`
- `internal/ingest/worker.go`
- `internal/store/store.go`
- `internal/store/store_test.go`

## 验收清单

- [x] 同 service、窗口内三条不同 fingerprint 只形成一个 Incident；
- [x] 窗口外同 group key 形成新 Incident；
- [x] 同 fingerprint 重复发送不增加 `alerts_count`；
- [x] 低 severity 成员不会降低 Incident severity；
- [x] hook 失败时告警、Incident 和 raw event 一起回滚；
- [x] `go test ./... && go build ./... && go vet ./...` 通过。

## 不包含

- resolved 成员传播和 Incident 自动关闭；
- Incident 查询 API；
- `agent_run` 创建与诊断。
