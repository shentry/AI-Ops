package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/flow/agent/react"
	"github.com/cloudwego/eino/schema"

	"oncall-agent/internal/config"
	"oncall-agent/internal/tools"
)

// PlanTarget 是修复动作的目标。name 必须是真实运行对象（GC-11），
// Guard（D09）会拿它对照证据里出现过的对象，这里先只做结构定义。
type PlanTarget struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// Plan 是结构化修复计划。action=none 表示"不建议自动动作"。
type Plan struct {
	Action     string     `json:"action"`
	Target     PlanTarget `json:"target"`
	Reason     string     `json:"reason"`
	Confidence string     `json:"confidence"`
	Risk       string     `json:"risk"`
	Expected   string     `json:"expected"`
}

// DiagnoseResult 是一次推理的完整产出，含 token 用量和工具步日志。
type DiagnoseResult struct {
	RCA          string
	Confidence   string
	EvidenceRefs []string
	Plan         Plan
	TokensIn     int
	TokensOut    int
	Steps        []StepLog
	Context      ContextStats
}

// StepLog 记录一次工具调用，供审计回放（GC-18）。
// 带起止时刻：回放要能回答"这次诊断调了几次工具、每次多久"。
type StepLog struct {
	Name   string
	Input  string
	Output string
	// Truncated is set by the Registry when output was bounded.
	Truncated  bool
	Err        string
	StartedAt  time.Time
	FinishedAt time.Time
}

// diagnoseContract 是 LLM 输出 JSON 的解析目标。
type diagnoseContract struct {
	RCA          string   `json:"rca"`
	Confidence   string   `json:"confidence"`
	EvidenceRefs []string `json:"evidence_refs"`
	Plan         Plan     `json:"plan"`
}

// Reasoner 把 Evidence 文本转成 RCA + Plan。Eino react.Agent 只负责
// 推理循环；流水线编排、Guard、审批都在别处（D09+）。
type Reasoner struct {
	factory  *Factory
	registry *tools.Registry
	budget   config.DiagnoseBudget
}

func NewReasoner(factory *Factory, registry *tools.Registry, budget config.DiagnoseBudget) *Reasoner {
	return &Reasoner{factory: factory, registry: registry, budget: budget}
}

// maxSteps 把 mode 翻译成 ReAct 步数上限。未知 mode 按 light 收紧，
// 不让一个陌生 mode 名拿到 full 的预算。
func (r *Reasoner) maxSteps(mode string) int {
	if mode == "full" {
		return r.budget.FullSteps
	}
	return r.budget.LightSteps
}

// Diagnose 执行一次推理。输出不是合法 JSON 时重试一次，仍失败返回错误；
// 任何失败都不会产生 Plan 副作用 —— Plan 只是数据，执行决策在 D09+。
// 失败时返回的 *DiagnoseResult 可能非空（只带 Steps/token，供审计），
// 调用方必须先判 error 再看内容。
func (r *Reasoner) Diagnose(ctx context.Context, evidence string, mode string) (result *DiagnoseResult, err error) {
	// Context compaction permits longer investigations; keep the entire run,
	// including contract retry, below the worker's five-minute stale threshold.
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	stats := &ContextStats{}
	defer func() {
		if result != nil {
			result.Context = *stats
		}
	}()
	if strings.TrimSpace(evidence) == "" {
		return nil, errors.New("llm: evidence is empty")
	}
	chatModel, inputLimit, err := r.factory.buildForDiagnosis()
	if err != nil {
		return nil, err
	}
	recorder := &stepRecorder{}
	counter := &usageCounter{}
	agentTools, err := r.agentTools(recorder)
	if err != nil {
		return nil, err
	}
	agent, err := react.NewAgent(ctx, &react.AgentConfig{
		ToolCallingModel: &diagnosisModel{ToolCallingChatModel: wrapUsageModel(chatModel, counter), maxSteps: r.maxSteps(mode),
			context: contextBudget{inputLimit: inputLimit, stats: stats, estimator: &tokenEstimator{}}},
		ToolsConfig: compose.ToolsNodeConfig{Tools: agentTools},
		MaxStep:     r.maxSteps(mode),
	})
	if err != nil {
		return nil, fmt.Errorf("llm: build react agent: %w", err)
	}

	messages := []*schema.Message{
		schema.SystemMessage(systemPrompt),
		schema.UserMessage(evidence),
	}
	result, err = r.runOnce(ctx, agent, messages, recorder)
	if err == nil {
		result.TokensIn, result.TokensOut = counter.in, counter.out
		return result, nil
	}
	var parseErr *contractError
	if !errors.As(err, &parseErr) {
		return partialResult(recorder, counter), err
	}
	// 解析失败重试一次：明确告诉模型上次输出的问题，仍失败则放弃。
	// 计数器跨重试累计 —— 第一次烧掉的 token 也是成本。
	retryMessages := append(messages,
		schema.AssistantMessage(parseErr.raw, nil),
		schema.UserMessage("上次输出不是合法 JSON 契约。请只输出符合契约的 JSON 对象，不要输出其他文字。"),
	)
	result, err = r.runOnce(ctx, agent, retryMessages, recorder)
	if err != nil {
		return partialResult(recorder, counter), err
	}
	result.TokensIn, result.TokensOut = counter.in, counter.out
	return result, nil
}

// partialResult 是失败时的审计残骸：只有 Steps 和 token 用量有意义，
// RCA/Plan 一律空。调用方必须先看 error —— 有 error 时这份结果不是结论，
// 只是"这次失败前调了哪些工具、烧了多少 token"的账（GC-18 要求失败也可回放）。
func partialResult(recorder *stepRecorder, counter *usageCounter) *DiagnoseResult {
	if recorder == nil || len(recorder.steps) == 0 {
		return nil
	}
	return &DiagnoseResult{Steps: recorder.steps, TokensIn: counter.in, TokensOut: counter.out}
}

// runOnce 跑一轮 ReAct 并解析最终输出。
func (r *Reasoner) runOnce(ctx context.Context, agent *react.Agent, messages []*schema.Message, recorder *stepRecorder) (*DiagnoseResult, error) {
	final, err := agent.Generate(ctx, messages)
	if err != nil {
		return nil, fmt.Errorf("llm: react generate: %w", err)
	}
	if final == nil || strings.TrimSpace(final.Content) == "" {
		return nil, errors.New("llm: empty model output")
	}
	contract, err := parseContract(final.Content)
	if err != nil {
		return nil, err
	}
	// token 用量由调用方从 usageCounter 填（跨轮次、跨重试累计），
	// 这里只看最终消息的内容和契约。
	return &DiagnoseResult{
		RCA:          contract.RCA,
		Confidence:   contract.Confidence,
		EvidenceRefs: contract.EvidenceRefs,
		Plan:         contract.Plan,
		Steps:        recorder.steps,
	}, nil
}

// ErrContractParse classifies model output that violates the response contract.
var ErrContractParse = errors.New("llm: contract parse failure")

// contractError 是"输出不是合法契约"的错误类型，带原文供重试时回放。
type contractError struct {
	raw string
	err error
}

func (e *contractError) Error() string { return e.err.Error() }
func (e *contractError) Unwrap() error { return ErrContractParse }

// parseContract 剥离 markdown 围栏后按契约解析。字段校验：
// rca 非空；confidence 限定三档，非法值视为解析失败（宁可重试）。
func parseContract(raw string) (*diagnoseContract, error) {
	cleaned := stripJSONFence(raw)
	var contract diagnoseContract
	if err := json.Unmarshal([]byte(cleaned), &contract); err != nil {
		return nil, &contractError{raw: raw, err: fmt.Errorf("llm: output is not valid plan JSON: %w", err)}
	}
	if strings.TrimSpace(contract.RCA) == "" {
		return nil, &contractError{raw: raw, err: errors.New("llm: plan JSON missing rca")}
	}
	switch contract.Confidence {
	case "high", "medium", "low":
	default:
		return nil, &contractError{raw: raw, err: fmt.Errorf("llm: invalid confidence %q", contract.Confidence)}
	}
	// These are the currently supported remediation suggestions. Policy still
	// validates registration, target, scope and approval before any execution.
	if contract.Plan.Action != "none" && contract.Plan.Action != tools.ToolDockerRestart {
		return nil, &contractError{raw: raw, err: errors.New("llm: plan.action must be none or docker_restart")}
	}
	return &contract, nil
}

// stripJSONFence 剥掉 ```json ... ``` 或 ``` ... ``` 围栏。
// 模型经常多包一层，这是宽容不是契约 —— 契约仍是"一个 JSON 对象"。
func stripJSONFence(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if !strings.HasPrefix(trimmed, "```") {
		return trimmed
	}
	lines := strings.Split(trimmed, "\n")
	if len(lines) < 2 {
		return trimmed
	}
	// 去掉首行（```json 或 ```）和末尾的 ``` 行。
	body := lines[1:]
	if last := strings.TrimSpace(body[len(body)-1]); last == "```" {
		body = body[:len(body)-1]
	}
	return strings.TrimSpace(strings.Join(body, "\n"))
}

// stepRecorder records calls from a tool batch, which Eino executes concurrently.
type stepRecorder struct {
	mu    sync.Mutex
	steps []StepLog
}

func (r *stepRecorder) record(step StepLog) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.steps = append(r.steps, step)
}

// agentTools 把 Registry.ForLLM() 的 L1 工具适配成 Eino InvokableTool。
// 只有 L1 能进这个列表 —— 这是 LLM 权限面的第二道闸（第一道是 ForLLM 本身）。
func (r *Reasoner) agentTools(recorder *stepRecorder) ([]tool.BaseTool, error) {
	specs := r.registry.ForLLM()
	agentTools := make([]tool.BaseTool, 0, len(specs))
	for _, spec := range specs {
		params := make(map[string]*schema.ParameterInfo, len(spec.Params))
		for _, p := range spec.Params {
			params[p.Name] = &schema.ParameterInfo{
				Type:     schema.String,
				Desc:     p.Description,
				Required: p.Required,
			}
		}
		agentTools = append(agentTools, &registryTool{registry: r.registry, spec: spec, params: params, recorder: recorder})
	}
	return agentTools, nil
}

// registryTool 把 tools.ToolSpec 适配成 Eino InvokableTool：
// 执行仍走 Registry.Execute —— 超时、截断、未注册拒绝的纪律不变。
type registryTool struct {
	registry *tools.Registry
	spec     tools.ToolSpec
	params   map[string]*schema.ParameterInfo
	recorder *stepRecorder
}

func (t *registryTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name:        t.spec.Name,
		Desc:        t.spec.Description,
		ParamsOneOf: schema.NewParamsOneOfByParams(t.params),
	}, nil
}

func (t *registryTool) InvokableRun(ctx context.Context, argumentsInJSON string, _ ...tool.Option) (string, error) {
	started := time.Now().UTC()
	output, metadata, err := executeLLMTool(ctx, t.registry, t.spec.Name, argumentsInJSON)
	entry := StepLog{Name: t.spec.Name, Input: argumentsInJSON, Truncated: metadata.Truncated, StartedAt: started, FinishedAt: time.Now().UTC()}
	if err != nil {
		// 工具失败以观测文本喂回模型，而不是炸掉整个 ReAct 循环：
		// Prometheus 抖一下不该让这次诊断归零，模型看到失败后
		// 可以基于其余证据出低置信结论。StepLog 里仍记 Err 供审计。
		entry.Err = err.Error()
		t.recorder.record(entry)
		return "tool error: " + err.Error(), nil
	}
	entry.Output = output
	t.recorder.record(entry)
	return output, nil
}
