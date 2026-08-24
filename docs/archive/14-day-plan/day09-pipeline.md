# Day09：诊断 Pipeline、Guard 与通知

> 阶段：P1 Agent 诊断  
> 状态：已完成
> 依赖：Day08  
> 详细实现记录：[Day9 实现文档](../day9-implementation.md)

## 当日目标

把 agent_run、Evidence、Reasoner、Guard 和通知串成独立诊断闭环，保证诊断失败不影响摄入。

## 实现清单

- [x] 实现独立诊断 worker，扫描 pending 和超时 running 的 `agent_run`；
- [x] 实现 `Pipeline.Run`：mode → memory lookup 占位 → evidence → reason → guard → report；
- [x] 每个阶段写入一条 `agent_run_step`，记录输入摘要、输出摘要、耗时和错误；
- [x] Guard 拒绝无 target 的 Plan；
- [x] Guard 对配置错误、镜像不存在等场景禁止盲目重启；
- [x] Guard 结果不得被后续 LLM 覆盖（Guard 在 LLM 之后执行，无回写路径）；
- [x] 实现飞书或企业微信 Notifier 接口；
- [x] 发送 RCA、证据摘要、Plan、决策和 run 标识；
- [x] 通知失败独立重试，不回滚诊断结果；
- [x] 诊断进行中继续接收和处理告警。

## 关键文件

- `internal/diagnose/pipeline.go`
- `internal/diagnose/guard.go`
- `internal/diagnose/worker.go`
- `internal/notify/notifier.go`
- `internal/store/store.go`
- `cmd/server/main.go`

## 验收清单

- [x] critical Incident 能生成完整 run 和 step 链；
- [x] warning 使用 light 路径，info/low 不调用 LLM（分流见 D05 路由单测与 D08 步数预算单测）；
- [x] Guard 命中时记录 `kind=guard` 且决策为拒绝或升级；
- [x] Prometheus 不可用时诊断降级完成（证据缺失留痕、进程存活）；与计划"run failed"的偏差及理由见实现文档 §2；
- [x] 诊断耗时期间新 webhook 仍能返回并进入 raw_event；
- [x] 通知失败不改变 run 终态；
- [x] `go test ./... && go build ./... && go vet ./...` 通过。

## 不包含

- L3 审批 API；
- 真实变更动作；
- 故障记忆命中。
