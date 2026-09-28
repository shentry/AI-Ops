package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"oncall-agent/internal/config"
	"oncall-agent/internal/tools"
)

// fakeOpenAIServer 模拟 OpenAI 兼容 /chat/completions。
// handler 按第 n 次请求决定返回内容，记录请求体供断言。
type fakeOpenAIServer struct {
	server   *httptest.Server
	requests atomic.Int32
	handler  func(call int, body map[string]any) map[string]any
}

func newFakeOpenAIServer(t *testing.T, handler func(call int, body map[string]any) map[string]any) *fakeOpenAIServer {
	t.Helper()
	fake := &fakeOpenAIServer{handler: handler}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		bodyBytes, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(bodyBytes, &body)
		call := int(fake.requests.Add(1))
		w.Header().Set("Content-Type", "application/json")
		response := fake.handler(call, body)
		_ = json.NewEncoder(w).Encode(response)
	}))
	t.Cleanup(fake.server.Close)
	return fake
}

// chatResponse 构造一条非流式 chat completion 响应。
func chatResponse(content string, promptTokens, completionTokens int) map[string]any {
	return map[string]any{
		"id":      "chatcmpl-fake",
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   "fake",
		"choices": []map[string]any{{
			"index":         0,
			"finish_reason": "stop",
			"message":       map[string]any{"role": "assistant", "content": content},
		}},
		"usage": map[string]any{
			"prompt_tokens":     promptTokens,
			"completion_tokens": completionTokens,
			"total_tokens":      promptTokens + completionTokens,
		},
	}
}

// toolCallResponse 构造一条要求调用工具的响应。
func toolCallResponse(callID, toolName, argsJSON string) map[string]any {
	return map[string]any{
		"id":      "chatcmpl-fake",
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   "fake",
		"choices": []map[string]any{{
			"index":         0,
			"finish_reason": "tool_calls",
			"message": map[string]any{
				"role":    "assistant",
				"content": "",
				"tool_calls": []map[string]any{{
					"id":   callID,
					"type": "function",
					"function": map[string]any{
						"name":      toolName,
						"arguments": argsJSON,
					},
				}},
			},
		}},
		"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
	}
}

func toolCallResponseWithThinking(callID, toolName, argsJSON, thinking string) map[string]any {
	resp := toolCallResponse(callID, toolName, argsJSON)
	choices := resp["choices"].([]map[string]any)
	message := choices[0]["message"].(map[string]any)
	message["reasoning_content"] = thinking
	message["content"] = []map[string]any{{"type": "thinking", "thinking": thinking}}
	return resp
}

const validPlanJSON = `{
  "rca": "Sub2API 网关进程退出，容器状态为 stopped（见 docker 段）",
  "confidence": "high",
  "evidence_refs": ["docker", "alert_snapshot"],
  "plan": {
    "action": "docker_restart",
    "target": {"kind": "container", "name": "sub2api"},
    "reason": "进程退出且无配置错误迹象",
    "confidence": "high",
    "risk": "low",
    "expected": "/health 恢复 200"
  }
}`

func testReasoner(t *testing.T, baseURL string, registry *tools.Registry) *Reasoner {
	t.Helper()
	factory := NewFactory(config.LLMConfig{Roles: config.LLMRoles{
		Reasoner: config.RoleConfig{BaseURL: baseURL, APIKey: "fake-key", Model: "fake-model", MaxTokens: 1024},
	}})
	return NewReasoner(factory, registry, config.DiagnoseBudget{FullSteps: 8, LightSteps: 3})
}

var stubActionDefinition = tools.ActionDefinition{Name: "docker_restart", Version: 1, TargetKind: "container", Description: "restart a container",
	Params: []tools.ParamSpec{{Name: "reason", Description: "why"}}, Timeout: time.Second}

// stubAction is a registered write the model may name; tests never execute it.
type stubAction struct{ def tools.ActionDefinition }

func (a stubAction) Definition() tools.ActionDefinition { return a.def }
func (a stubAction) Prepare(context.Context, tools.PrepareRequest) (tools.Prepared, error) {
	return tools.Prepared{}, errors.New("stub")
}
func (a stubAction) Execute(context.Context, tools.Operation) (tools.Receipt, error) {
	return tools.Receipt{}, errors.New("stub")
}
func (a stubAction) Reconcile(context.Context, tools.Operation) (tools.Reconciliation, error) {
	return tools.Reconciliation{Outcome: tools.OutcomeUnknown}, errors.New("stub")
}

// actionRegistry holds the plannable action the fixtures' plans name.
func actionRegistry(t *testing.T) *tools.Registry {
	t.Helper()
	registry := tools.NewRegistry()
	for _, def := range []tools.ActionDefinition{stubActionDefinition, {Name: "upstream_restore", Version: 1, TargetKind: "account", Description: "undo", Compensation: true, Timeout: time.Second}} {
		if err := registry.RegisterAction(stubAction{def: def}); err != nil {
			t.Fatal(err)
		}
	}
	return registry
}

func stubLLMRegistry(t *testing.T) *tools.Registry {
	t.Helper()
	registry := actionRegistry(t)
	if err := registry.Register(tools.ToolSpec{
		Name: tools.ToolPromSeriesMeta, Description: "stub series meta",
		Timeout: 5 * time.Second, MaxOutput: 512,
		Params: []tools.ParamSpec{{Name: "match", Description: "selector", Required: true}},
		Handler: func(_ context.Context, raw json.RawMessage) (string, error) {
			return `{"series":[{"__name__":"up"}],"truncated":false,"returned":1}`, nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	return registry
}

func TestFactoryValidate(t *testing.T) {
	// 缺 api_key 要报错，且错误文本不得含任何密钥值。
	factory := NewFactory(config.LLMConfig{Roles: config.LLMRoles{
		Reasoner: config.RoleConfig{BaseURL: "http://x", Model: "m"},
	}})
	err := factory.Validate()
	if err == nil || !strings.Contains(err.Error(), "api_key") {
		t.Fatalf("Validate() = %v, want missing api_key", err)
	}
	// 完整配置通过。
	ok := NewFactory(config.LLMConfig{Roles: config.LLMRoles{
		Reasoner: config.RoleConfig{BaseURL: "http://x", APIKey: "k", Model: "m"},
	}})
	if err := ok.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
}

func TestReasonerDiagnoseStructuredOutput(t *testing.T) {
	fake := newFakeOpenAIServer(t, func(call int, body map[string]any) map[string]any {
		return chatResponse(validPlanJSON, 120, 45)
	})
	reasoner := testReasoner(t, fake.server.URL, stubLLMRegistry(t))
	result, err := reasoner.Diagnose(context.Background(), "evidence text", "full", nil)
	if err != nil {
		t.Fatalf("Diagnose() error = %v", err)
	}
	if result.RCA == "" || result.Confidence != "high" || len(result.EvidenceRefs) != 2 {
		t.Fatalf("result = %+v", result)
	}
	if result.Plan.Action != "docker_restart" || result.Plan.Target.Name != "sub2api" || result.Plan.Risk != "low" {
		t.Fatalf("plan = %+v", result.Plan)
	}
	if result.TokensIn != 120 || result.TokensOut != 45 {
		t.Fatalf("tokens = %d/%d, want 120/45", result.TokensIn, result.TokensOut)
	}
}

func TestReasonerStripsMarkdownFence(t *testing.T) {
	fake := newFakeOpenAIServer(t, func(call int, body map[string]any) map[string]any {
		return chatResponse("```json\n"+validPlanJSON+"\n```", 10, 5)
	})
	reasoner := testReasoner(t, fake.server.URL, stubLLMRegistry(t))
	result, err := reasoner.Diagnose(context.Background(), "evidence", "light", nil)
	if err != nil {
		t.Fatalf("Diagnose() error = %v", err)
	}
	if result.Confidence != "high" {
		t.Fatalf("confidence = %q", result.Confidence)
	}
}

func TestReasonerToolCallWithinBudget(t *testing.T) {
	toolCalls := 0
	fake := newFakeOpenAIServer(t, func(call int, body map[string]any) map[string]any {
		if call == 1 {
			return toolCallResponse("call-1", tools.ToolPromSeriesMeta, `{"match":"up"}`)
		}
		return chatResponse(validPlanJSON, 30, 12)
	})
	registry := actionRegistry(t)
	if err := registry.Register(tools.ToolSpec{
		Name: tools.ToolPromSeriesMeta, Description: "stub",
		Timeout: 5 * time.Second, MaxOutput: 512,
		Params: []tools.ParamSpec{{Name: "match", Description: "selector", Required: true}},
		Handler: func(_ context.Context, raw json.RawMessage) (string, error) {
			toolCalls++
			return `{"series":[]}`, nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	reasoner := testReasoner(t, fake.server.URL, registry)
	result, err := reasoner.Diagnose(context.Background(), "evidence", "light", nil)
	if err != nil {
		t.Fatalf("Diagnose() error = %v", err)
	}
	// light 模式：工具调用被真实执行且不超过步数预算。
	if toolCalls != 1 {
		t.Fatalf("tool calls = %d, want 1", toolCalls)
	}
	if len(result.Steps) != 1 || result.Steps[0].Name != tools.ToolPromSeriesMeta || result.Steps[0].Output == "" {
		t.Fatalf("steps = %+v", result.Steps)
	}
	if result.TokensIn <= 0 {
		t.Fatalf("tokens_in = %d, want > 0", result.TokensIn)
	}
}

func TestReasonerEchoesThinkingOnToolFollowUp(t *testing.T) {
	fake := newFakeOpenAIServer(t, func(call int, body map[string]any) map[string]any {
		if call == 1 {
			return toolCallResponseWithThinking("call1", tools.ToolPromSeriesMeta, `{"match":"up"}`, "check series first")
		}
		if !assistantEchoedThinking(body, "check series first") {
			t.Fatalf("second request missing thinking echo: %#v", body["messages"])
		}
		return chatResponse(validPlanJSON, 10, 5)
	})
	factory := NewFactory(config.LLMConfig{Roles: config.LLMRoles{
		Reasoner: config.RoleConfig{
			BaseURL: fake.server.URL, APIKey: "fake-key", Model: "fake-model", MaxTokens: 1024,
			Thinking: config.ThinkingConfig{Enabled: true, Effort: "medium"},
		},
	}})
	reasoner := NewReasoner(factory, stubLLMRegistry(t), config.DiagnoseBudget{FullSteps: 8, LightSteps: 3})
	if _, err := reasoner.Diagnose(context.Background(), "evidence", "light", nil); err != nil {
		t.Fatalf("Diagnose() error = %v", err)
	}
	if got := fake.requests.Load(); got != 2 {
		t.Fatalf("requests = %d, want 2", got)
	}
}

func assistantEchoedThinking(body map[string]any, want string) bool {
	messages, ok := body["messages"].([]any)
	if !ok {
		return false
	}
	for _, raw := range messages {
		msg, ok := raw.(map[string]any)
		if !ok || msg["role"] != "assistant" || msg["reasoning_content"] != want {
			continue
		}
		parts, ok := msg["content"].([]any)
		if !ok {
			continue
		}
		for _, part := range parts {
			item, ok := part.(map[string]any)
			if ok && item["type"] == "thinking" && item["thinking"] == want {
				return true
			}
		}
	}
	return false
}

func TestReasonerRejectsInvalidJSONAfterOneRetry(t *testing.T) {
	fake := newFakeOpenAIServer(t, func(call int, body map[string]any) map[string]any {
		return chatResponse("这不是 JSON", 10, 5)
	})
	reasoner := testReasoner(t, fake.server.URL, stubLLMRegistry(t))
	_, err := reasoner.Diagnose(context.Background(), "evidence", "full", nil)
	if err == nil {
		t.Fatal("Diagnose() error = nil, want contract failure")
	}
	// 第一次 + 一次重试，不能无限重试。
	if got := fake.requests.Load(); got != 2 {
		t.Fatalf("requests = %d, want 2 (1 retry)", got)
	}
}

func TestReasonerRetriesOnceThenParses(t *testing.T) {
	fake := newFakeOpenAIServer(t, func(call int, body map[string]any) map[string]any {
		if call == 1 {
			return chatResponse("not json at all", 10, 5)
		}
		return chatResponse(validPlanJSON, 20, 8)
	})
	reasoner := testReasoner(t, fake.server.URL, stubLLMRegistry(t))
	result, err := reasoner.Diagnose(context.Background(), "evidence", "full", nil)
	if err != nil {
		t.Fatalf("Diagnose() error = %v", err)
	}
	if result.Confidence != "high" {
		t.Fatalf("confidence = %q", result.Confidence)
	}
}
func TestReasonerRejectsEmptyEvidence(t *testing.T) {
	reasoner := testReasoner(t, "http://127.0.0.1:1", stubLLMRegistry(t))
	if _, err := reasoner.Diagnose(context.Background(), "  ", "full", nil); err == nil {
		t.Fatal("Diagnose(empty evidence) error = nil")
	}
}

func TestReasonerModelTimeout(t *testing.T) {
	fake := newFakeOpenAIServer(t, func(call int, body map[string]any) map[string]any {
		time.Sleep(3 * time.Second)
		return chatResponse(validPlanJSON, 1, 1)
	})
	reasoner := testReasoner(t, fake.server.URL, stubLLMRegistry(t))
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if _, err := reasoner.Diagnose(ctx, "evidence", "full", nil); err == nil {
		t.Fatal("Diagnose() error = nil, want timeout")
	}
}

// Writes are actions: the model may name an enabled one in its plan, but no
// action is ever a callable tool.
func TestReasonerNeverExposesActionsAsTools(t *testing.T) {
	registry := stubLLMRegistry(t)
	reasoner := testReasoner(t, "http://127.0.0.1:1", registry)
	agentTools := reasoner.agentTools(registry.ForLLM(), &stepRecorder{})
	for _, at := range agentTools {
		info, _ := at.Info(context.Background())
		if info.Name == "docker_restart" {
			t.Fatal("action leaked into the LLM tool surface")
		}
	}
	if len(agentTools) != 1 {
		t.Fatalf("agent tools = %d, want 1 read-only tool", len(agentTools))
	}
	if !strings.Contains(reasoner.prompt, "docker_restart") || !strings.Contains(reasoner.prompt, "reason") || strings.Contains(reasoner.prompt, "upstream_restore") {
		t.Fatalf("prompt must list plannable actions and their params only:\n%s", reasoner.prompt)
	}
}

func TestParseContractAcceptsOnlyDeclaredActionParams(t *testing.T) {
	actions := map[string]tools.ActionDefinition{"docker_restart": stubActionDefinition}
	for name, test := range map[string]struct {
		plan string
		ok   bool
	}{
		"none":             {`{"action":"none"}`, true},
		"declared param":   {`{"action":"docker_restart","params":{"reason":"oom"}}`, true},
		"null params":      {`{"action":"docker_restart","params":null}`, true},
		"unknown action":   {`{"action":"upstream_restore"}`, false},
		"undeclared param": {`{"action":"docker_restart","params":{"image":"x"}}`, false},
		"params not obj":   {`{"action":"docker_restart","params":["reason"]}`, false},
	} {
		raw := `{"rca":"x","confidence":"high","evidence_refs":["docker"],"plan":` + test.plan + `}`
		contract, err := parseContract(raw, actions)
		if (err == nil) != test.ok {
			t.Fatalf("%s: err=%v", name, err)
		}
		if err == nil && len(contract.EvidenceRefs) != 1 {
			t.Fatalf("%s: evidence refs lost: %+v", name, contract)
		}
	}
}

func TestReasonerUsageAccumulatesAcrossRoundsAndRetry(t *testing.T) {
	// 第一轮非法 JSON（10/5），重试成功（20/8）：累计必须是 30/13，
	// 不能只记最后一轮 —— 重试烧掉的 token 也是成本。
	fake := newFakeOpenAIServer(t, func(call int, body map[string]any) map[string]any {
		if call == 1 {
			return chatResponse("not json", 10, 5)
		}
		return chatResponse(validPlanJSON, 20, 8)
	})
	reasoner := testReasoner(t, fake.server.URL, stubLLMRegistry(t))
	result, err := reasoner.Diagnose(context.Background(), "evidence", "full", nil)
	if err != nil {
		t.Fatalf("Diagnose() error = %v", err)
	}
	if result.TokensIn != 30 || result.TokensOut != 13 {
		t.Fatalf("tokens = %d/%d, want accumulated 30/13", result.TokensIn, result.TokensOut)
	}
}

func TestReasonerRateLimited(t *testing.T) {
	// 429 限流：直接报错，不重试（限流不是格式问题，重试只会更糟）。
	fake := &fakeOpenAIServer{}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"message":"rate limited","type":"rate_limit_error"}}`)
	}))
	t.Cleanup(fake.server.Close)
	reasoner := testReasoner(t, fake.server.URL, stubLLMRegistry(t))
	_, err := reasoner.Diagnose(context.Background(), "evidence", "full", nil)
	if err == nil {
		t.Fatal("Diagnose() error = nil, want rate limit failure")
	}
	if got := fake.requests.Load(); got != 1 {
		t.Fatalf("requests = %d, want 1 (no retry on 429)", got)
	}
}

func TestReasonerRejectsEmptyModelOutput(t *testing.T) {
	// 模型返回空 content：报错，且不走"非法 JSON"的重试路径。
	fake := newFakeOpenAIServer(t, func(call int, body map[string]any) map[string]any {
		return chatResponse("", 10, 5)
	})
	reasoner := testReasoner(t, fake.server.URL, stubLLMRegistry(t))
	if _, err := reasoner.Diagnose(context.Background(), "evidence", "full", nil); err == nil {
		t.Fatal("Diagnose() error = nil, want empty output failure")
	}
	if got := fake.requests.Load(); got != 1 {
		t.Fatalf("requests = %d, want 1 (empty output is not retryable)", got)
	}
}

func TestReasonerToolErrorRecordedInSteps(t *testing.T) {
	// 工具 handler 失败：错误回填给模型继续推理，StepLog 记 Err，
	// 最终输出不受影响。
	fake := newFakeOpenAIServer(t, func(call int, body map[string]any) map[string]any {
		if call == 1 {
			return toolCallResponse("call-1", tools.ToolPromSeriesMeta, `{"match":"up"}`)
		}
		return chatResponse(validPlanJSON, 30, 12)
	})
	registry := actionRegistry(t)
	if err := registry.Register(tools.ToolSpec{
		Name: tools.ToolPromSeriesMeta, Description: "stub",
		Timeout: 5 * time.Second, MaxOutput: 512,
		Params: []tools.ParamSpec{{Name: "match", Description: "selector", Required: true}},
		Handler: func(context.Context, json.RawMessage) (string, error) {
			return "", fmt.Errorf("prometheus unreachable")
		},
	}); err != nil {
		t.Fatal(err)
	}
	reasoner := testReasoner(t, fake.server.URL, registry)
	result, err := reasoner.Diagnose(context.Background(), "evidence", "light", nil)
	if err != nil {
		t.Fatalf("Diagnose() error = %v", err)
	}
	if len(result.Steps) != 1 || result.Steps[0].Err == "" || result.Steps[0].Output != "" {
		t.Fatalf("steps = %+v, want one step with error", result.Steps)
	}
	if result.Confidence != "high" {
		t.Fatalf("confidence = %q", result.Confidence)
	}
}

func TestReasonerNormalizesEmptyToolArgumentPlaceholder(t *testing.T) {
	var received string
	fake := newFakeOpenAIServer(t, func(call int, body map[string]any) map[string]any {
		if call == 1 {
			return toolCallResponse("call-placeholder", tools.ToolPromSeriesMeta, `{}{"match":"up"}`)
		}
		return chatResponse(validPlanJSON, 30, 12)
	})
	registry := actionRegistry(t)
	if err := registry.Register(tools.ToolSpec{
		Name: tools.ToolPromSeriesMeta, Description: "stub",
		Timeout: 5 * time.Second, MaxOutput: 512,
		Params: []tools.ParamSpec{{Name: "match", Description: "selector", Required: true}},
		Handler: func(_ context.Context, raw json.RawMessage) (string, error) {
			received = string(raw)
			var args struct {
				Match string `json:"match"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			if args.Match != "up" {
				return "", fmt.Errorf("unexpected match %q", args.Match)
			}
			return `{"series":[]}`, nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	reasoner := testReasoner(t, fake.server.URL, registry)
	result, err := reasoner.Diagnose(context.Background(), "evidence", "light", nil)
	if err != nil {
		t.Fatalf("Diagnose() error = %v", err)
	}
	if received != `{"match":"up"}` {
		t.Fatalf("normalized tool args = %q", received)
	}
	if len(result.Steps) != 1 || result.Steps[0].Err != "" {
		t.Fatalf("steps = %+v", result.Steps)
	}
}

// A model that keeps requesting evidence must still get a final answer turn
// inside the existing graph budget (model + tools + model).
func TestReasonerReservesFinalAnswerWithinBudget(t *testing.T) {
	for _, steps := range []int{1, 2, 3, 8} {
		t.Run(fmt.Sprint(steps), func(t *testing.T) {
			fake := newFakeOpenAIServer(t, func(call int, body map[string]any) map[string]any {
				if body["tool_choice"] == "none" {
					return chatResponse(validPlanJSON, 10, 5)
				}
				return toolCallResponse(fmt.Sprint(call), tools.ToolPromSeriesMeta, `{"match":"up"}`)
			})
			r := testReasoner(t, fake.server.URL, stubLLMRegistry(t))
			r.budget.LightSteps = steps
			result, err := r.Diagnose(context.Background(), "evidence", "light", nil)
			if err != nil {
				t.Fatalf("Diagnose() = %v", err)
			}
			wantCalls := (steps + 1) / 2
			if got := int(fake.requests.Load()); got != wantCalls || len(result.Steps) != wantCalls-1 {
				t.Fatalf("calls=%d tools=%d, want %d/%d", got, len(result.Steps), wantCalls, wantCalls-1)
			}
		})
	}
}

func TestReasonerRejectsFreeTextAction(t *testing.T) {
	fake := newFakeOpenAIServer(t, func(call int, body map[string]any) map[string]any {
		return chatResponse(strings.Replace(validPlanJSON, `"docker_restart"`, `"提高内存或扩容"`, 1), 10, 5)
	})
	r := testReasoner(t, fake.server.URL, stubLLMRegistry(t))
	if _, err := r.Diagnose(context.Background(), "evidence", "light", nil); err == nil {
		t.Fatal("free-text action accepted")
	}
	if fake.requests.Load() != 2 {
		t.Fatal("invalid action must use the existing single contract retry")
	}
}

func TestReasonerFinalTurnRejectsMoreTools(t *testing.T) {
	fake := newFakeOpenAIServer(t, func(call int, body map[string]any) map[string]any {
		return toolCallResponse(fmt.Sprint(call), tools.ToolPromSeriesMeta, `{"match":"up"}`)
	})
	r := testReasoner(t, fake.server.URL, stubLLMRegistry(t))
	result, err := r.Diagnose(context.Background(), "evidence", "light", nil)
	if err == nil || !strings.Contains(err.Error(), "evidence budget ended") {
		t.Fatalf("error=%v, want tool budget rejection", err)
	}
	if result == nil || len(result.Steps) != 1 || result.Plan.Action != "" || result.TokensIn != 20 || fake.requests.Load() != 2 {
		t.Fatalf("partial=%+v requests=%d", result, fake.requests.Load())
	}
}

func TestReasonerBatchToolsPreservesEveryStep(t *testing.T) {
	const batchSize = 8
	fake := newFakeOpenAIServer(t, func(call int, body map[string]any) map[string]any {
		if call > 1 {
			if body["tool_choice"] != "none" {
				t.Error("batch must count as one tool round")
			}
			return chatResponse(validPlanJSON, 10, 5)
		}
		response := toolCallResponse("unused", tools.ToolPromSeriesMeta, `{"match":"up"}`)
		calls := make([]map[string]any, 0, batchSize)
		for i := 0; i < batchSize; i++ {
			calls = append(calls, map[string]any{"id": fmt.Sprint(i), "type": "function", "function": map[string]any{
				"name": tools.ToolPromSeriesMeta, "arguments": fmt.Sprintf(`{"match":"metric_%d"}`, i),
			}})
		}
		response["choices"].([]map[string]any)[0]["message"].(map[string]any)["tool_calls"] = calls
		return response
	})
	r := testReasoner(t, fake.server.URL, stubLLMRegistry(t))
	result, err := r.Diagnose(context.Background(), "evidence", "light", nil)
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	for _, step := range result.Steps {
		seen[step.Input] = true
	}
	if len(result.Steps) != batchSize || len(seen) != batchSize {
		t.Fatalf("recorded %d steps (%d distinct), want %d", len(result.Steps), len(seen), batchSize)
	}
}

// The replay record must be written before the model sees anything, and name
// exactly the model, prompt and tool surface used. A failed record stops the call.
func TestReasonerRecordsInputBeforeFirstModelCall(t *testing.T) {
	fake := newFakeOpenAIServer(t, func(int, map[string]any) map[string]any {
		return chatResponse(validPlanJSON, 10, 5)
	})
	reasoner := testReasoner(t, fake.server.URL, stubLLMRegistry(t))
	var recorded DiagnosisInput
	result, err := reasoner.Diagnose(context.Background(), "evidence text", "light", func(input DiagnosisInput) error {
		if fake.requests.Load() != 0 {
			t.Error("input recorded after the model was called")
		}
		recorded = input
		return nil
	})
	if err != nil || result == nil {
		t.Fatalf("Diagnose() = %v", err)
	}
	if recorded.Model != "fake-model" || recorded.PromptSHA256 != reasoner.promptSHA256 || len(recorded.PromptSHA256) != 64 || recorded.Evidence != "evidence text" {
		t.Fatalf("recorded = %+v", recorded)
	}
	if len(recorded.Tools) != 1 || recorded.Tools[0].Name != tools.ToolPromSeriesMeta || len(recorded.Tools[0].Params) != 1 {
		t.Fatalf("recorded tools = %+v", recorded.Tools)
	}

	_, err = reasoner.Diagnose(context.Background(), "evidence text", "light", func(DiagnosisInput) error {
		return fmt.Errorf("snapshot store unavailable")
	})
	if err == nil || !strings.Contains(err.Error(), "record diagnosis input") || fake.requests.Load() != 1 {
		t.Fatalf("err=%v requests=%d, want no model call after a failed record", err, fake.requests.Load())
	}
}
