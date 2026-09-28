package llm

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"

	"oncall-agent/internal/tools"
)

func rangeFixture(count int) string {
	points := make([][]any, count)
	for i := range points {
		value := "5"
		if i == count/2 {
			value = "999"
		}
		points[i] = []any{1700000000 + i, value}
	}
	b, _ := json.Marshal(map[string]any{"resultType": "matrix", "warning": "partial source", "result": []any{
		map[string]any{"metric": map[string]string{"__name__": "memory_bytes", "container": "payments", "instance": "node-0"}, "values": points},
	}})
	return string(b)
}

func contextFixture() []*schema.Message {
	first := schema.AssistantMessage("", []schema.ToolCall{{ID: "old", Type: "function", Function: schema.FunctionCall{Name: tools.ToolPromRangeQuery, Arguments: `{"query":"memory_bytes"}`}}})
	first.ReasoningContent = "protocol reasoning must survive unchanged"
	first.Extra = map[string]any{"signature": "opaque"}
	last := schema.AssistantMessage("", []schema.ToolCall{{ID: "recent", Type: "function", Function: schema.FunctionCall{Name: tools.ToolPromSeriesMeta, Arguments: `{"match":"up"}`}}})
	return []*schema.Message{
		schema.SystemMessage("Rules"), schema.UserMessage("incident=10 target=payments error=OOM"), first,
		schema.ToolMessage(rangeFixture(160), "old", schema.WithToolName(tools.ToolPromRangeQuery)), last,
		schema.ToolMessage(`{"series":[],"error":"permission denied"}`, "recent", schema.WithToolName(tools.ToolPromSeriesMeta)),
	}
}

func TestContextCompactionPreservesProtocolAndEvidence(t *testing.T) {
	messages := contextFixture()
	original, _ := json.Marshal(messages)
	size, err := estimatedMessageTokens(messages)
	if err != nil {
		t.Fatal(err)
	}
	stats := &ContextStats{}
	budget := contextBudget{inputLimit: size - 500, stats: stats}
	out, err := budget.prepare(messages)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != len(messages) {
		t.Fatal("message pair removed")
	}
	for i := range messages {
		if i != 3 && !reflect.DeepEqual(out[i], messages[i]) {
			t.Fatalf("protected message %d changed", i)
		}
	}
	if out[3].ToolCallID != "old" || out[3].ToolName != tools.ToolPromRangeQuery || !strings.Contains(out[3].Content, `"values_summary"`) {
		t.Fatalf("invalid compacted tool: %+v", out[3])
	}
	for _, fragment := range []string{`"container":"payments"`, `"instance":"node-0"`, `"warning":"partial source"`, `"999"`, `"count":160`, `"intermediate_samples_omitted":true`} {
		if !strings.Contains(out[3].Content, fragment) {
			t.Fatalf("lost %s", fragment)
		}
	}
	after, _ := json.Marshal(messages)
	if string(original) != string(after) {
		t.Fatal("mutated original history/audit")
	}
	if stats.Compactions != 1 || stats.SavedEstimate <= 0 || stats.EstimatedAfter > budget.inputLimit {
		t.Fatalf("stats=%+v", stats)
	}
	underBudget := contextBudget{inputLimit: size * 2}
	unchanged, err := underBudget.prepare(messages)
	if err != nil || !reflect.DeepEqual(unchanged, messages) {
		t.Fatal("small context changed")
	}
}

func TestContextBudgetKeepsWholeRecentBatch(t *testing.T) {
	messages := contextFixture()
	// Both outputs belong to the latest batch and must be protected, even when
	// the first contains a long, otherwise compressible result.
	messages[4].ToolCalls = append(messages[4].ToolCalls, schema.ToolCall{ID: "recent-long", Type: "function", Function: schema.FunctionCall{Name: tools.ToolPromRangeQuery, Arguments: `{}`}})
	messages = append(messages, schema.ToolMessage(rangeFixture(160), "recent-long", schema.WithToolName(tools.ToolPromRangeQuery)))
	budget := contextBudget{inputLimit: 1200}
	if _, err := budget.prepare(messages); !errors.Is(err, ErrContextBudget) {
		t.Fatalf("error=%v, want protected-context overflow", err)
	}
	if !strings.Contains(messages[len(messages)-1].Content, `"values"`) {
		t.Fatal("recent batch changed")
	}
}

func TestContextBudgetCountsToolsAndThinking(t *testing.T) {
	messages := []*schema.Message{schema.UserMessage("small")}
	size, _ := estimatedMessageTokens(messages)
	if _, err := (&contextBudget{inputLimit: size + 10, toolTokens: 100}).prepare(messages); !errors.Is(err, ErrContextBudget) {
		t.Fatalf("tool schema not counted: %v", err)
	}
	msg := schema.AssistantMessage("small", nil)
	without, _ := estimatedMessageTokens([]*schema.Message{msg})
	msg.ReasoningContent = strings.Repeat("r", 1000)
	with, _ := estimatedMessageTokens([]*schema.Message{msg})
	if with-without < 660 {
		t.Fatal("echoed thinking must be counted")
	}
}

func TestToolDigestPreservesErrorsAndUnknownData(t *testing.T) {
	for _, content := range []string{"tool error: timeout", `{"result":[{"values":[`, strings.ReplaceAll(rangeFixture(100), `"5"`, `"NaN"`)} {
		if got := compactToolResult(tools.ToolPromRangeQuery, content); got != content {
			t.Fatalf("changed malformed/error/non-finite data: %s", got)
		}
	}
	content := rangeFixture(100)
	if compactToolResult("unknown", content) != content {
		t.Fatal("unknown tool rewritten")
	}
	logs := "lines=2 patterns=1 (most recent first)\ncount=2 first=unknown last=unknown | healthy\n"
	if compactToolResult(tools.ToolDockerLogs, logs) != logs {
		t.Fatal("already aggregated docker logs rewritten")
	}
}

func TestReasonerCompactsOldToolAndContinues(t *testing.T) {
	fake := newFakeOpenAIServer(t, func(call int, body map[string]any) map[string]any {
		switch call {
		case 1:
			return toolCallResponse("range", tools.ToolPromRangeQuery, `{"query":"memory_bytes"}`)
		case 2:
			return toolCallResponse("meta", tools.ToolPromSeriesMeta, `{"match":"up"}`)
		case 3:
			if body["tool_choice"] == "none" {
				t.Error("context compaction must allow continued investigation")
			}
			encoded, _ := json.Marshal(body["messages"])
			if !strings.Contains(string(encoded), "values_summary") {
				t.Error("old result not compacted")
			}
			return toolCallResponse("followup", tools.ToolPromSeriesMeta, `{"match":"memory_bytes"}`)
		default:
			return chatResponse(validPlanJSON, 10, 5)
		}
	})
	registry := stubLLMRegistry(t)
	if err := registry.Register(tools.ToolSpec{Name: tools.ToolPromRangeQuery, Description: "range", Timeout: time.Second, MaxOutput: 20000,
		Params: []tools.ParamSpec{{Name: "query", Required: true}}, Handler: func(context.Context, json.RawMessage) (string, error) { return rangeFixture(180), nil }}); err != nil {
		t.Fatal(err)
	}
	r := testReasoner(t, fake.server.URL, registry)
	r.budget.LightSteps = 16
	r.factory.contextWindowTokens = 5500
	result, err := r.Diagnose(context.Background(), "incident=10 target=payments", "light", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Context.Compactions == 0 || len(result.Steps) != 3 || fake.requests.Load() != 4 {
		t.Fatalf("result=%+v calls=%d", result, fake.requests.Load())
	}
	if !strings.Contains(result.Steps[0].Output, `"values"`) || strings.Contains(result.Steps[0].Output, "values_summary") {
		t.Fatal("audit overwritten by compacted result")
	}
}

func TestReasonerRefusesOversizedEvidenceBeforeCallingModel(t *testing.T) {
	fake := newFakeOpenAIServer(t, func(int, map[string]any) map[string]any { return chatResponse(validPlanJSON, 10, 5) })
	r := testReasoner(t, fake.server.URL, stubLLMRegistry(t))
	r.factory.contextWindowTokens = 4000
	_, err := r.Diagnose(context.Background(), strings.Repeat("critical target evidence", 1000), "light", nil)
	if !errors.Is(err, ErrContextBudget) || fake.requests.Load() != 0 {
		t.Fatalf("error=%v calls=%d", err, fake.requests.Load())
	}
}

func TestRangeDigestRetainsExactLargeValues(t *testing.T) {
	original := strings.ReplaceAll(rangeFixture(100), `"5"`, `"9007199254740993"`)
	out := compactToolResult(tools.ToolPromRangeQuery, original)
	if !strings.Contains(out, `"9007199254740993"`) {
		t.Fatal("numeric precision lost")
	}
}
