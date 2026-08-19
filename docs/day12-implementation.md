# Day12 实现文档：验证失败重诊与安全闭环

> 本文对应 `oncall-agent-开发SPEC.md` 的 D12 和 `docs/14-day-plan/day12-retry-safety.md`。目标：Verify 失败后的有限重诊与人工升级。

## 1. Day12 做了什么

- `diagnose.RetryScheduler`：Verify 失败 → 沿 `retry_of` 链数链长（首诊=1，人工手动重诊各自成链、不占自动预算），链长 ≤ 2 时落 `retry_of=failedRunID` 的 `pending` run（重诊用 full 预算）；超限 → 人工升级通知（失败原因 + 完整 run 链）；
- 并发护栏：`HasActiveRun` 存在（pending/running）时不创建重诊 run —— 同一 incident 的重诊不并发；
- Pipeline 重诊上下文：`run.RetryOf != nil` 时在证据前注入上一轮的 RCA/Plan/状态/verify 结论（读自 `agent_run_step`，标注"已失败、不复用、仅供对照"）；读不到上一轮不阻断诊断；
- `notify.EscalationMessage` + `SendEscalation`：`[紧急]` 前缀卡片，含 run 链；Noop 实现同步支持；
- Executor 接线：Verify 失败 → `ScheduleRetry`；升级失败记日志不吞；
- store：`GetAgentRun`、`CountIncidentRuns`、`HasActiveRun`、`ListIncidentRunIDs`。

## 2. 既有安全性质（复用，不重复实现）

- 进程重启恢复：pending/超时 running 回补在 D09 已实现；
- 已完成步骤不产生副作用：执行副作用全部经 `approved→executing` 条件领取，天然幂等；
- L4/过期/target 消失拒绝：D10/D11 已覆盖（policy 拒绝、claim 带 expires_at、白名单校验）。

## 3. 测试覆盖

- RetryScheduler：创建 retry_of 链（pending/full/retry_of 正确）；有活跃 run 时跳过；预算耗尽升级人工（通知含 incident、run 链、原因）；
- Pipeline：retryContext 注入上一轮 RCA/Plan 与"不复用"声明；
- Executor：verify 失败触发 ScheduleRetry，verify 通过不触发；
- Notify：升级文案含 run 链。

## 4. 真实端到端验收

本机 Docker + 目标容器 `d12-target`，incident 由 simulate 促发且故意不 resolved：

1. approved 审批单 → 容器真实重启 → approval=executed → verify step `passed=false`；
2. 首轮 verify 失败时 run 73 为 pending → 重诊被并发护栏正确跳过（不重复创建）；
3. 篡改 plan_hash 的审批单 → failed，不执行（GC-13 实测）；
4. run 73 标 failed 后再来一张审批单 → verify 失败 → 创建 run 74（`retry_of=73, status=pending`）。

无 LLM 环境下 run 74 停 pending 属预期（诊断 worker 未启动）。

```bash
go test ./...    # 含 TEST_MYSQL_DSN、TEST_PROMETHEUS_URL 集成测试
go build ./... && go vet ./...
```
