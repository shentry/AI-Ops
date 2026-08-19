package llm

import (
	"context"
	"encoding/json"
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

const validPlanJSON = `{
  "rca": "Sub2API 网关进程退出，容器状态为 stopped（见 docker 段）",
  "confidence": "high",
  "evidence_refs": ["docker", "alert_snapshot"],
  "plan": {
    "action": "restart_container",
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

func stubLLMRegistry(t *testing.T) *tools.Registry {
	t.Helper()
	registry := tools.NewRegistry()
	if err := registry.Register(tools.ToolSpec{
		Name: tools.ToolPromSeriesMeta, Description: "stub series meta", Level: tools.L1ReadOnly,
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
	// 未知角色拒绝。
	if _, err := ok.Build("hacker"); err == nil {
		t.Fatal("Build(unknown role) error = nil")
	}
}

func TestReasonerDiagnoseStructuredOutput(t *testing.T) {
	fake := newFakeOpenAIServer(t, func(call int, body map[string]any) map[string]any {
		return chatResponse(validPlanJSON, 120, 45)
	})
	reasoner := testReasoner(t, fake.server.URL, stubLLMRegistry(t))
	result, err := reasoner.Diagnose(context.Background(), "evidence text", "full")
	if err != nil {
		t.Fatalf("Diagnose() error = %v", err)
	}
	if result.RCA == "" || result.Confidence != "high" || len(result.EvidenceRefs) != 2 {
		t.Fatalf("result = %+v", result)
	}
	if result.Plan.Action != "restart_container" || result.Plan.Target.Name != "sub2api" || result.Plan.Risk != "low" {
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
	result, err := reasoner.Diagnose(context.Background(), "evidence", "light")
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
	registry := tools.NewRegistry()
	if err := registry.Register(tools.ToolSpec{
		Name: tools.ToolPromSeriesMeta, Description: "stub", Level: tools.L1ReadOnly,
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
	result, err := reasoner.Diagnose(context.Background(), "evidence", "light")
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

func TestReasonerRejectsInvalidJSONAfterOneRetry(t *testing.T) {
	fake := newFakeOpenAIServer(t, func(call int, body map[string]any) map[string]any {
		return chatResponse("这不是 JSON", 10, 5)
	})
	reasoner := testReasoner(t, fake.server.URL, stubLLMRegistry(t))
	_, err := reasoner.Diagnose(context.Background(), "evidence", "full")
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
	result, err := reasoner.Diagnose(context.Background(), "evidence", "full")
	if err != nil {
		t.Fatalf("Diagnose() error = %v", err)
	}
	if result.Confidence != "high" {
		t.Fatalf("confidence = %q", result.Confidence)
	}
}
func TestReasonerRejectsEmptyEvidence(t *testing.T) {
	reasoner := testReasoner(t, "http://127.0.0.1:1", stubLLMRegistry(t))
	if _, err := reasoner.Diagnose(context.Background(), "  ", "full"); err == nil {
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
	if _, err := reasoner.Diagnose(ctx, "evidence", "full"); err == nil {
		t.Fatal("Diagnose() error = nil, want timeout")
	}
}

func TestReasonerLLMCannotReachL2Tools(t *testing.T) {
	// 注册一个 L2 工具：它绝不能出现在 Reasoner 的工具面里。
	registry := stubLLMRegistry(t)
	if err := registry.Register(tools.ToolSpec{
		Name: "docker_restart", Description: "restart", Level: tools.L2LowRisk,
		Timeout: time.Second, Handler: func(context.Context, json.RawMessage) (string, error) { return "", nil },
	}); err != nil {
		t.Fatal(err)
	}
	reasoner := testReasoner(t, "http://127.0.0.1:1", registry)
	agentTools, err := reasoner.agentTools(&stepRecorder{})
	if err != nil {
		t.Fatal(err)
	}
	for _, at := range agentTools {
		info, _ := at.Info(context.Background())
		if info.Name == "docker_restart" {
			t.Fatal("L2 tool leaked into LLM tool surface")
		}
	}
	if len(agentTools) != 1 {
		t.Fatalf("agent tools = %d, want 1 (L1 only)", len(agentTools))
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
	result, err := reasoner.Diagnose(context.Background(), "evidence", "full")
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
	_, err := reasoner.Diagnose(context.Background(), "evidence", "full")
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
	if _, err := reasoner.Diagnose(context.Background(), "evidence", "full"); err == nil {
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
	registry := tools.NewRegistry()
	if err := registry.Register(tools.ToolSpec{
		Name: tools.ToolPromSeriesMeta, Description: "stub", Level: tools.L1ReadOnly,
		Timeout: 5 * time.Second, MaxOutput: 512,
		Params: []tools.ParamSpec{{Name: "match", Description: "selector", Required: true}},
		Handler: func(context.Context, json.RawMessage) (string, error) {
			return "", fmt.Errorf("prometheus unreachable")
		},
	}); err != nil {
		t.Fatal(err)
	}
	reasoner := testReasoner(t, fake.server.URL, registry)
	result, err := reasoner.Diagnose(context.Background(), "evidence", "light")
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
