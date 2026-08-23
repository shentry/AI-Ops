# Day9 实现文档：诊断 Pipeline、Guard 与通知

> 本文对应 `oncall-agent-开发SPEC.md` 的 D09 和 `docs/14-day-plan/day09-pipeline.md`。目标：把 agent_run 队列、Evidence、Reasoner、Guard 和通知串成独立诊断闭环。
>
> SPEC 的可砍项（`mysql_select`、`get_runbook`）按计划顺延到 W5 缓冲，本日未实现。

## 1. Day9 做了什么

- `diagnose.Worker`：独立 goroutine 纯轮询消费 `agent_run(status=pending)`；启动与每轮循环做超时 running 回补（`RequeueStaleAgentRuns`，5 分钟阈值），与 raw_event 的补账语义对齐；单个 run 失败不阻断队列；
- `Pipeline.Run`：evidence → llm → guard → notify 四阶段，每阶段落一条 `agent_run_step`（输入摘要、输出摘要、时间戳、错误）；Reasoner 在 llm 阶段内部的每次工具调用另外逐条落 `kind=tool` step（seq 用 30–89 独立段，主链占 1–6、verify 占 90），llm 摘要里带 `tool_calls=N`——只有一条 RCA 摘要的话，回放看不到模型实际查了什么，"工具调用次数"也没法断言。诊断失败的 run 同样保留已发生的工具 step；
- `Guard`（`guard.go`）：确定性规则链——`action_without_target`（有动作无真实 target → 强制 none + deny）、`non_restartable_failure`（配置/镜像/凭据类根因 → 禁止重启类动作 + escalate）；Guard 在 LLM 之后执行，没有环节能改回它的结论（GC-09）；
- `notify.Notifier`：wecom/feishu webhook 实现 + Noop 兜底；`NotifyReporter` 独立重试 3 次，失败只记 step 不改 run 终态；
- store 新增 `NextPendingAgentRun` / `ClaimAgentRun`（原子认领）/ `RequeueStaleAgentRuns` / `AppendRunStep`；
- `cmd/server`：LLM 配置缺失时诊断 worker 不启动、摄入照常（GC-07）；IM webhook 缺失时用 Noop。

## 2. 与计划验收条目的偏差说明

计划验收写"Prometheus 不可用时 run failed"。实际语义按设计文档 9.2 执行：Prometheus 不可用时 prom 相关证据段记 error、诊断继续完成（RCA 降置信是自然结果）、进程存活。"禁止基于空证据自动修复"由 Guard/Policy 层保证（D10+），不是靠 run 失败。实测：停 Prometheus 后手动重诊 run 36 succeeded，证据段 prom_replay/golden_metrics 为 error，其余段正常。

## 3. 测试覆盖

- Guard：正常放行、无 target 拒绝、配置/镜像/凭据根因禁重启升级人工、none 动作直通；
- Pipeline：四段 step 链与终态落库、token 写入、Guard 命中写 `kind=guard` step（decision=escalate）、证据失败 run failed 且不进 LLM、通知失败 run 仍 succeeded 且 notify step 带错误、Reasoner 工具调用逐条落 step（成功/失败/失败 run 也不丢）；
- Worker：排空队列、单 run 失败继续、超时 running 回补后再消费；
- Notify：wecom payload 与卡片文案、业务错误（errcode≠0）、HTTP 错误、配置校验、Noop；
- store 集成：队首顺序、原子认领、超时回补边界、step 校验。

## 4. 真实端到端验收

本地 mock LLM（OpenAI 兼容）+ 不可达 webhook：

1. simulate 促发 critical incident → run `full/succeeded`，tokens 120/45，step 链 evidence→llm→guard→notify 完整；
2. webhook 不可达 → notify step 记错误，run 终态仍 succeeded；
3. 诊断进行期间 simulate 再发一批 → 全部 202（摄入不被诊断阻塞）；
4. 停 Prometheus → 手动重诊 run succeeded，prom 证据段 error、其余段正常，服务存活；
5. mock LLM 的固定 Plan 经 Guard 放行（action=none）。

本次实际执行并通过：

```bash
go test ./...    # 含 TEST_MYSQL_DSN、TEST_PROMETHEUS_URL 集成测试
go build ./... && go vet ./...
```
