# Day12：验证失败重诊与安全闭环

> 阶段：P2 权限审批  
> 状态：已完成
> 依赖：Day11  
> 详细实现记录：[Day12 实现文档](../day12-implementation.md)

## 当日目标

处理执行后验证失败的情况，限制自动重试次数，并将无法安全处理的故障升级人工。

## 实现清单

- [x] Verify 失败时记录失败原因、证据引用和执行结果（verify step + result_json）；
- [x] 沿 `retry_of` 链计算链长，最多允许两次重诊（人工重诊不占预算）；
- [x] 创建重诊 run 时注入上次失败 Plan 和 failure reason（重诊上下文含上一轮 verify step 结论）；
- [x] 重诊重新执行 Evidence、Reasoner 和 Guard，不复用未经验证的执行结论；
- [x] 第二次失败后 run 标记 failed，Incident 升级人工（链预算耗尽 → 升级通知）；
- [x] 通过 Notifier 发送高优先级人工升级通知；
- [x] 同一 Incident 的重诊不能并发执行；
- [x] 进程重启后 pending/running 超时任务可恢复（D09 回补语义，worker 测试覆盖）；
- [x] 已完成步骤不得重复产生副作用（执行经 approved→executing 条件领取，幂等）；
- [x] 增加 L4 拒绝、target 消失、审批过期和重复动作测试（D10/D11 用例 + 本轮重诊用例）。

## 关键文件

- `internal/diagnose/retry.go`
- `internal/diagnose/pipeline.go`
- `internal/approval/executor.go`
- `internal/notify/notifier.go`
- `internal/store/store.go`

## 验收清单

- [x] 第一次 Verify 失败创建 `retry_of` 指向原 run 的新 run；
- [x] 第二次失败后不再创建第三个自动 run；
- [x] 人工升级通知包含失败原因和完整 run 链；
- [x] L4、审批过期、target 消失均不能执行动作；
- [x] 模拟进程中断后，已 executed 的动作不会再次执行（条件领取幂等，D11 测试覆盖）；
- [x] 自动修复失败不会写入高置信故障记忆（D13 记忆写入只收验证成功的高置信案例）；
- [x] `go test ./... && go build ./... && go vet ./...` 通过。

## 不包含

- 语义向量记忆；
- 多租户和多集群；
- 复杂拓扑推理。
