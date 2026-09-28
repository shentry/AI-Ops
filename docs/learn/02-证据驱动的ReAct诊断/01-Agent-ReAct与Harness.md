# Agent、ReAct 与 Harness

> 所属：[亮点二 · 证据驱动的 ReAct 诊断](README.md)

## 一句话

Agent 就是「大模型 + 上下文 + 工具」组成的循环。ReAct 是其中最常见的循环方式：想一步、做一步、看结果、再想下一步。本项目只把「根因推理」这一段交给 ReAct，授权、执行、验证都是固定流程，这叫**混合编排**。

## 先弄懂：单次调用和 Agent 的区别

- **单次调用**：把问题和资料一次性交给模型，模型直接回答。适合信息已经给全的任务，比如「把这段日志翻译成中文」。
- **Agent**：模型可以调用工具获取新信息，再根据结果决定下一步。适合「查什么取决于上一步看到了什么」的任务，比如排查故障。

## 先弄懂：ReAct 循环

ReAct = **Re**ason（推理）+ **Act**（行动）。循环如下：

```text
          ┌──────────────────────────────────────────┐
          ▼                                          │
  模型读取「目前掌握的信息」                              │
          │                                          │
          ├── 决定调用工具 ──▶ 执行工具 ──▶ 工具结果追加到消息历史 ─┘
          │
          └── 不再调用工具 ──▶ 输出最终答案，结束
```

在本项目里，一次诊断的真实过程大致是这样：

```text
[system] 你是值班自愈系统的诊断推理节点……（规则 + 输出契约 + 可建议的动作清单）
[user]   # Evidence for incident 7 ……（11 项证据）
[assistant] 调用 prom_series_meta {"match": "up{job=\"sub2api\"}"}   ← 先查有哪些指标
[tool]      [{"__name__":"up","job":"sub2api",...}]
[assistant] 调用 prom_range_query {"query": "up{job=\"sub2api\"}", "start": "...", "end": "..."}
[tool]      {"result":[{"metric":{...},"values":[[...,"1"],[...,"0"]]}]}
[assistant] {"rca": "docker_inspect 显示 sub2api 容器 exited……", "confidence": "high",
             "plan": {"action": "docker_restart", "target": {"kind":"container","name":"sub2api"}, ...}}
```

注意：「模型不再调用工具」只是循环的**停止信号**，不代表任务完成。本项目中「完成」由后面的恢复验证判定（见亮点五）。

## 先弄懂：Agent = Model + Harness

- **Model（模型）**：负责开放性的判断。
- **Harness（外围控制层）**：模型之外的一切。
- **Environment（环境）**：被观察、被改变的外部世界。

| Harness 的职责 | 要回答的问题 | 本项目的实现 |
|---|---|---|
| 上下文管理 | 每次决策时信息够不够？ | 证据采集与渲染、重诊时注入上次失败的原因、上下文预算 |
| 工具接口 | 模型能不能正确理解和使用工具？ | 只读工具注册表、参数说明、统一的超时 / 截断 / 错误语义 |
| 约束 | 哪些能力可以用？ | 写动作不给模型；处置规则是唯一授权 |
| 验证 | 是不是真的做对了？ | 输出契约校验、Guard、恢复验证 |
| 纠正 | 错了怎么办？ | 解析失败重试、对账、补偿、重诊、急停转人工 |

本项目里 Model 只出现在两处：

- 故障诊断：`llm.Reasoner`
- 控制台追问：`llm.Questioner`（见亮点七）

sub2api 服务、Docker、Prometheus、PostgreSQL、Redis、飞书里的值班人员都是 Environment。

## 先弄懂：编排方式怎么选

| 任务特征 | 适合的编排 |
|---|---|
| 输入充分，一次转换就能完成 | 单次模型调用 |
| 步骤固定，有明确的业务关口 | 确定性工作流 |
| 下一步取决于新观察（搜索、调试、排障） | 自主 Agent（ReAct） |
| 稳定关口之间夹着开放探索 | **混合编排** |

本项目属于最后一种：

- **开放探索**：该补查哪个指标、要不要看日志，取决于已有证据，所以交给 ReAct。
- **稳定关口**：能不能执行、怎么执行、修没修好，涉及生产变更，必须确定、可审计，所以是固定流程。

## 本项目怎么用 Eino 搭 ReAct

CloudWeGo Eino 是字节开源的 Go 大模型应用框架，`react.Agent` 实现了上面的 ReAct 循环。

```go
// 摘自 internal/llm/reasoner.go 的 Diagnose（有删减）
func (r *Reasoner) Diagnose(ctx context.Context, evidence string, mode string, record func(DiagnosisInput) error) (*DiagnoseResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute) // 整次诊断最多 3 分钟
	defer cancel()

	chatModel, modelID, inputLimit, err := r.factory.buildForDiagnosis() // 当前选中的模型
	recorder := &stepRecorder{}                                           // 记录每次工具调用
	counter := &usageCounter{}                                            // 累计 token 用量
	specs := r.registry.ForLLM()                                          // 只读工具清单

	agent, err := react.NewAgent(ctx, &react.AgentConfig{
		// 包了一层自己的模型：控制最后一轮、做上下文预算、统计 token
		ToolCallingModel: &diagnosisModel{
			ToolCallingChatModel: wrapUsageModel(chatModel, counter),
			maxSteps:             r.maxSteps(mode),
			context:              contextBudget{inputLimit: inputLimit /* … */},
		},
		// 工具适配器：执行仍然走注册表（统一超时、脱敏、截断）
		ToolsConfig: compose.ToolsNodeConfig{Tools: r.agentTools(specs, recorder)},
		MaxStep:     r.maxSteps(mode), // full=32，light=16
	})

	// 调用模型之前，先把完整输入交给调用方存进回放快照
	record(DiagnosisInput{Model: modelID, PromptSHA256: r.promptSHA256, Tools: /* … */, Evidence: evidence})

	messages := []*schema.Message{
		schema.SystemMessage(r.prompt), // 系统提示词：规则 + 契约 + 动作清单
		schema.UserMessage(evidence),   // 证据文本
	}
	result, err := r.runOnce(ctx, agent, messages, recorder) // 跑循环 + 解析输出
	// 解析失败重试 1 次，见第 6 篇
	return result, err
}
```

分工很清楚：

- **Eino 负责**：循环本身，也就是调模型、执行工具、把工具结果追加进消息历史。
- **本项目负责**：给什么证据（第 2、3 篇）、给什么工具（第 4 篇）、预算怎么控制（第 5 篇）、输出怎么校验（第 6 篇），以及模型之后的全部关口（亮点三到五）。

## 常见追问

- **为什么不用多 Agent？** 这里没有可以并行拆分的子任务，也不需要多个视角互相辩论。多 Agent 会增加通信成本和出错点。本项目确实有「提议者与审核者」分离，但审核者是确定性代码（Guard、Policy）和人，不是另一个模型。
- **ReAct 的最终答案可信吗？** 不直接信。它只是一段需要经过契约校验、Guard、Policy、审批、执行复验和恢复验证的建议数据。
