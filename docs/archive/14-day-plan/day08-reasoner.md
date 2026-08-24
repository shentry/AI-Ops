# Day08：Eino Reasoner 与结构化 Plan

> 阶段：P1 Agent 诊断  
> 状态：已完成
> 依赖：Day07  
> 详细实现记录：[Day8 实现文档](../day8-implementation.md)

## 当日目标

实现角色化 LLM 工厂和单一推理节点，将 Evidence 转换为可校验的 RCA 与修复 Plan。

## 实现清单

- [x] 实现 `llm.Build(role)`，按配置创建并缓存模型客户端；
- [x] 启动时校验 reasoner 配置，但不把密钥写入日志（`Factory.Validate`，D09 挂启动路径）；
- [x] 实现 `Reasoner.Diagnose(ctx, evidence, mode)`；
- [x] 使用 Eino ReAct，仅挂载 `Registry.ForLLM()` 的 L1 工具；
- [x] full 模式最多 8 步，light 模式最多 3 步；
- [x] Prompt 明确先查 series metadata，再生成 PromQL；
- [x] 定义 `RCA`、`confidence`、`evidence_refs` 和 `Plan` JSON 契约；
- [x] 支持剥离 markdown JSON 围栏；
- [x] 非法 JSON 最多重试一次，仍失败则返回错误；
- [x] 记录 prompt/completion token 到 `agent_run`（`CompleteAgentRun` 落库）；
- [x] 对模型超时、限流、空输出和工具错误编写测试。

## 关键文件

- `internal/llm/factory.go`
- `internal/llm/reasoner.go`
- `internal/llm/prompts.go`
- `internal/llm/*_test.go`
- `internal/store/store.go`

## 验收清单

- [x] 使用 fake OpenAI 兼容服务完成一次结构化推理；
- [x] 输出能解析为合法 Plan，且 evidence refs 指向真实 Evidence（契约字段校验通过；与真实 Evidence 段落的一致性校验由 D09 Guard 负责）；
- [x] mode=light 的工具步骤数不超过 3；
- [x] LLM 无法自行调用 L2/L3/L4 工具；
- [x] token 使用量正确写入 `agent_run`；
- [x] 非法输出不会触发任何执行动作（解析失败仅返回错误，无 Plan 副作用）；
- [x] `go test ./... && go build ./... && go vet ./...` 通过。

## 不包含

- 诊断 worker 串联；
- Guard 和 Policy 决策；
- 审批、执行和记忆写入。
