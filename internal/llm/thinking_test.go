package llm

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/cloudwego/eino/schema"

	"oncall-agent/internal/config"
)

func TestExtractThinking(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "reasoning_content",
			body: `{"choices":[{"message":{"role":"assistant","reasoning_content":"step one"}}]}`,
			want: "step one",
		},
		{
			name: "content thinking array",
			body: `{"choices":[{"message":{"role":"assistant","content":[{"type":"thinking","thinking":"from parts"}]}}]}`,
			want: "from parts",
		},
		{
			name: "missing",
			body: `{"choices":[{"message":{"role":"assistant","content":"hello"}}]}`,
			want: "",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := extractThinking([]byte(test.body)); got != test.want {
				t.Fatalf("extractThinking() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestEchoThinking(t *testing.T) {
	t.Run("empty content with tool calls", func(t *testing.T) {
		raw := []byte(`{"messages":[{"role":"assistant","content":"","tool_calls":[{"id":"c1"}]}]}`)
		input := []*schema.Message{{
			Role:             schema.Assistant,
			ReasoningContent: "check series first",
		}}
		out, err := echoThinking(raw, input)
		if err != nil {
			t.Fatalf("echoThinking() error = %v", err)
		}
		var body map[string]any
		if err := json.Unmarshal(out, &body); err != nil {
			t.Fatal(err)
		}
		msg := body["messages"].([]any)[0].(map[string]any)
		if msg["reasoning_content"] != "check series first" {
			t.Fatalf("reasoning_content = %v", msg["reasoning_content"])
		}
		parts, ok := msg["content"].([]any)
		if !ok || len(parts) != 1 {
			t.Fatalf("content = %#v", msg["content"])
		}
		part := parts[0].(map[string]any)
		if part["type"] != "thinking" || part["thinking"] != "check series first" {
			t.Fatalf("thinking part = %#v", part)
		}
	})

	t.Run("keeps non-empty string content", func(t *testing.T) {
		raw := []byte(`{"messages":[{"role":"assistant","content":"visible answer"}]}`)
		input := []*schema.Message{{
			Role:             schema.Assistant,
			Content:          "visible answer",
			ReasoningContent: "hidden",
		}}
		out, err := echoThinking(raw, input)
		if err != nil {
			t.Fatalf("echoThinking() error = %v", err)
		}
		var body map[string]any
		if err := json.Unmarshal(out, &body); err != nil {
			t.Fatal(err)
		}
		msg := body["messages"].([]any)[0].(map[string]any)
		if msg["reasoning_content"] != "hidden" {
			t.Fatalf("reasoning_content = %v", msg["reasoning_content"])
		}
		if msg["content"] != "visible answer" {
			t.Fatalf("content = %#v, want string", msg["content"])
		}
	})
}

func TestEchoThinkingPreservesToolArguments(t *testing.T) {
	raw := []byte(`{"messages":[{"role":"assistant","content":"","tool_calls":[{"id":"call-1","type":"function","function":{"name":"docker_inspect","arguments":"{\"name\":\"sub2api\"}"}}]}]}`)
	input := []*schema.Message{{Role: schema.Assistant, ReasoningContent: "inspect before deciding"}}
	out, err := echoThinking(raw, input)
	if err != nil {
		t.Fatalf("echoThinking() error = %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatal(err)
	}
	message := body["messages"].([]any)[0].(map[string]any)
	call := message["tool_calls"].([]any)[0].(map[string]any)
	function := call["function"].(map[string]any)
	if function["arguments"] != `{"name":"sub2api"}` {
		t.Fatalf("tool arguments = %#v", function["arguments"])
	}
}

func TestFactorySendsThinkingFlags(t *testing.T) {
	t.Run("disabled", func(t *testing.T) {
		var got map[string]any
		fake := newFakeOpenAIServer(t, func(_ int, body map[string]any) map[string]any {
			got = body
			return chatResponse("ok", 1, 1)
		})
		factory := NewFactory(config.LLMConfig{Roles: config.LLMRoles{
			Reasoner: config.RoleConfig{BaseURL: fake.server.URL, APIKey: "k", Model: "m", MaxTokens: 16},
		}})
		chatModel, err := factory.Build()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := chatModel.Generate(context.Background(), []*schema.Message{schema.UserMessage("hi")}); err != nil {
			t.Fatalf("Generate() error = %v", err)
		}
		thinking, _ := got["thinking"].(map[string]any)
		if thinking["type"] != "disabled" {
			t.Fatalf("thinking = %#v, want type=disabled", got["thinking"])
		}
		if effort, ok := got["reasoning_effort"]; ok && effort != "" && effort != nil {
			t.Fatalf("reasoning_effort = %#v, want empty", effort)
		}
	})

	t.Run("enabled low", func(t *testing.T) {
		var got map[string]any
		fake := newFakeOpenAIServer(t, func(_ int, body map[string]any) map[string]any {
			got = body
			return chatResponse("ok", 1, 1)
		})
		factory := NewFactory(config.LLMConfig{Roles: config.LLMRoles{
			Reasoner: config.RoleConfig{
				BaseURL: fake.server.URL, APIKey: "k", Model: "m", MaxTokens: 16,
				Thinking: config.ThinkingConfig{Enabled: true, Effort: "low"},
			},
		}})
		chatModel, err := factory.Build()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := chatModel.Generate(context.Background(), []*schema.Message{schema.UserMessage("hi")}); err != nil {
			t.Fatalf("Generate() error = %v", err)
		}
		thinking, _ := got["thinking"].(map[string]any)
		if thinking["type"] != "enabled" {
			t.Fatalf("thinking = %#v, want type=enabled", got["thinking"])
		}
		if got["reasoning_effort"] != "low" {
			t.Fatalf("reasoning_effort = %#v, want low", got["reasoning_effort"])
		}
	})
}
