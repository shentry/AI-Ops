# Day8 实现文档：Eino Reasoner 与结构化 Plan

> 本文对应 `oncall-agent-开发SPEC.md` 的 D08 和 `docs/14-day-plan/day08-reasoner.md`。目标：角色化 LLM 工厂 + 单一推理节点，把 Evidence 转成可校验的 RCA 和结构化 Plan。
>
> D08 不做诊断 worker 串联（D09）、不做 Guard/Policy（D09/D10）。按确认：Eino 只用于 Reasoner 推理节点；当前无真实 LLM Key，验收用 fake OpenAI 兼容服务完成，真实调用验收待 Key 到位后补。

## 1. Day8 做了什么

- `llm.Factory`：按角色（reasoner/summarizer）创建并缓存 OpenAI 兼容客户端；`Validate()` 启动期校验 reasoner 配置完整性，错误不含密钥值；`Build` 失败不透出 SDK 原始错误（可能带请求头密钥）；
- `llm.Reasoner.Diagnose(ctx, evidence, mode)`：Eino `react.Agent`，MaxStep full=8/light=3（未知 mode 按 light 收紧），工具面只挂 `Registry.ForLLM()` 的 L1 工具；
- token 用量经 `usageModel` 包装按模型调用累计（ReAct 多轮 + 契约重试都计入），写入 `DiagnoseResult.TokensIn/TokensOut`；
- 工具调用经 `registryTool` 适配器执行——仍走 `Registry.Execute`（超时、截断、未注册拒绝不变）；工具失败以 `tool error: ...` 观测文本喂回模型继续推理（不炸掉整个 ReAct 循环），同时记 `StepLog.Err` 供审计；
- store 新增 `CompleteAgentRun`：RCA/Plan/token/终态落库，终态（succeeded/failed）不可覆盖（审计不可改写）；
- `tools.ToolSpec` 增加 `Params` 声明，供 Eino function-calling schema；五个已注册工具补齐参数；`prom_series_meta` 的 `match` 兼容字符串与数组两种 LLM 输出。

## 2. 边界

- LLM 权限面 = `ForLLM()`（L1 只读）。L2+ 工具即使被模型点名也调不到（未注册即拒绝）；
- LLM 输出只是数据：`Plan` 的执行决策权在 Guard/Policy（D09/D10），LLM 编造 target 会被 Guard 拦（GC-09/GC-11）；
- Reasoner 不接编排：重试上限、并发、队列都在 D09 的 Pipeline。

## 3. 测试覆盖（fake OpenAI 兼容服务）

- 结构化输出完整解析（RCA/置信度/引用/Plan 全字段）；
- ```json 围栏剥壳；
- 工具调用真实执行并记入 Steps，light 模式预算内；
- 工具 handler 失败：StepLog 记 Err，错误以文本喂回模型，诊断继续完成；
- 非法 JSON 第一次失败 → 重试一次 → 仍失败报错（恰好 2 次请求）；
- 第一次非法、第二次合法 → 成功；
- token 用量跨轮次与重试累计（重试用例断言 30/13，非仅末轮）；
- 429 限流直接报错且不重试（恰好 1 次请求）；
- 模型空输出直接报错且不重试；空证据拒绝；
- 模型超时返回错误；
- L2 工具不出现在 Reasoner 工具面；
- Factory：缺 api_key 报错且不含密钥值；未知角色拒绝；
- store：`CompleteAgentRun` 落库、终态不可覆盖、非法终态拒绝。

本次实际执行并通过：

```bash
go test ./...    # 含 TEST_MYSQL_DSN、TEST_PROMETHEUS_URL 集成测试
go build ./... && go vet ./...
```

## 4. 真实环境验收记录

- 已通过真实 LLM 验收（OpenAI 兼容网关，模型 k3）：喂 D07 格式证据 → 非空 RCA（正确推断出容器 OOM：exit_code=137 + oom_killed + restart_count=3）+ 合法 Plan + `tokens_in > 0`；测试 `TestReasonerAgainstRealLLM` 由 `TEST_LLM_BASE_URL/TEST_LLM_API_KEY/TEST_LLM_MODEL` 门控；
- light 模式（3 步）对带 extended reasoning 的模型偏紧：模型一轮推理 + 一次工具调用 + 总结即触顶，超预算按设计报错（`exceeds max steps`）。full 模式（8 步）正常。真实部署时 reasoning 型模型建议调大 light_steps 或关 reasoning；
- `Factory.Validate()` 已挂到启动路径（D09 接线时完成，LLM 缺失时诊断链不启动）。
