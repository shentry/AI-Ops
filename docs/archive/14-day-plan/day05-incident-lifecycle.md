# Day05：Incident 生命周期、查询 API 和诊断分流

> 阶段：P0 告警核心  
> 状态：已完成
> 依赖：Day04  
> 详细实现记录：[Day5 实现文档](../day5-implementation.md)

## 当日目标

补齐 Incident 的 resolved 生命周期，建立可查询的事件 API，并把 firing Incident 转成持久化的诊断队列。

## 实现清单

- [x] 为 `resolved` 告警建立 fingerprint 到当前 Incident 的传播路径；
- [x] 仅当 Incident 全部成员 resolved 时将 Incident 标记为 resolved；
- [x] resolved 事件不创建新 Incident，也不延长开放事件时间窗；
- [x] 实现 `GET /api/v1/incidents?status=`；
- [x] 实现 `GET /api/v1/incidents/{id}`，返回成员告警和当前状态；
- [x] 实现 `POST /api/v1/incidents/{id}/diagnose` 手动重诊入口；
- [x] 按 severity route 创建 `agent_run(status=pending)`；
- [x] `info/low` 进入 `skip`，并以可统计的 succeeded 状态落库；
- [x] 诊断队列与摄入 worker 保持独立；
- [x] 查询 API 只读，不直接改变告警或执行状态。

## 关键文件

- `internal/incident/`
- `internal/api/incident.go`
- `internal/store/store.go`
- `internal/store/models.go`
- `cmd/server/main.go`

## 验收清单

- [x] firing 后发送 resolved，全部成员 resolved 时 Incident 变为 resolved；
- [x] 部分成员 resolved 时 Incident 仍保持开放；
- [x] 已 resolved Incident 不会被新 resolved 事件重复处理；
- [x] 查询列表支持 status 过滤和稳定排序；
- [x] 手动重诊会新建 `retry_of` 为空的 pending run；
- [x] low/info 不调用 LLM；
- [x] `go test ./... && go build ./... && go vet ./...` 通过。

## 不包含

- Evidence 收集；
- LLM 推理和工具调用；
- 自动执行和审批。
