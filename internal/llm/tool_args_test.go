package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"oncall-agent/internal/tools"
)

func TestNormalizeToolArguments(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr string
	}{
		{name: "single object", input: `{"name":"safe"}`, want: `{"name":"safe"}`},
		{name: "identical objects", input: `{"name":"safe"}{"name":"safe"}`, want: `{"name":"safe"}`},
		{name: "identical objects with whitespace", input: ` {"match":"up"}  { "match" : "up" } `, want: `{"match":"up"}`},
		{name: "empty placeholder before object", input: `{}{ "name": "safe" }`, want: `{ "name": "safe" }`},
		{name: "empty placeholder after object", input: `{"name":"safe"}{}`, want: `{"name":"safe"}`},
		{name: "prom instant gateway placeholder", input: `{}{"query":"probe_success{job=\"sub2api-deps\"}"}`, want: `{"query":"probe_success{job=\"sub2api-deps\"}"}`},
		{name: "docker inspect gateway placeholder", input: `{}{"name":"sub2api"}`, want: `{"name":"sub2api"}`},
		{name: "different objects", input: `{"name":"safe"}{"name":"evil"}`, wantErr: "multiple different"},
		{name: "different selectors", input: `{"match":"up"}{"match":"down"}`, wantErr: "multiple different"},
		{name: "empty placeholder does not hide conflict", input: `{}{"name":"safe"}{"name":"evil"}`, wantErr: "multiple different"},
		{name: "repeated primitive", input: `true true`, wantErr: "repeated non-object"},
		{name: "trailing malformed data", input: `{"name":"safe"}{`, wantErr: "unexpected EOF"},
		{name: "empty", input: " \t", wantErr: "empty"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := normalizeToolArguments(test.input)
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("normalizeToolArguments() error = %v", err)
				}
				if got != test.want {
					t.Fatalf("normalizeToolArguments() = %q, want %q", got, test.want)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("normalizeToolArguments() error = %v, want %q", err, test.wantErr)
			}
		})
	}
}

func TestExecuteLLMToolRejectsConflictingRepeatedArguments(t *testing.T) {
	registry := tools.NewRegistry()
	called := false
	if err := registry.Register(tools.ToolSpec{
		Name: "docker_inspect", Description: "test",
		Timeout: time.Second,
		Handler: func(context.Context, json.RawMessage) (string, error) {
			called = true
			return "ok", nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	_, _, err := executeLLMTool(context.Background(), registry, "docker_inspect", `{"name":"safe"}{"name":"evil"}`)
	if err == nil || !strings.Contains(err.Error(), "multiple different") {
		t.Fatalf("executeLLMTool() error = %v, want conflicting-arguments error", err)
	}
	if called {
		t.Fatal("handler called for conflicting repeated arguments")
	}
}

func TestQuestionRegistryToolNormalizesEmptyGatewayPlaceholder(t *testing.T) {
	registry := tools.NewRegistry()
	called := false
	if err := registry.Register(tools.ToolSpec{
		Name: "prom_series_meta", Description: "test",
		Timeout: time.Second,
		Handler: func(_ context.Context, raw json.RawMessage) (string, error) {
			called = true
			if string(raw) != `{"match":"up"}` {
				return "", fmt.Errorf("raw args = %s", raw)
			}
			return "ok", nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	recorder := &questionStepRecorder{counts: make(map[string]int)}
	tool := &questionRegistryTool{registry: registry, spec: tools.ToolSpec{Name: "prom_series_meta"}, recorder: recorder}
	output, err := tool.InvokableRun(context.Background(), `{}{"match":"up"}`)
	if err != nil || output != "ok" {
		t.Fatalf("InvokableRun() = %q, %v", output, err)
	}
	if !called || len(recorder.messages) != 1 || recorder.messages[0].Err != "" {
		t.Fatalf("recorder = %+v, called=%v", recorder.messages, called)
	}
}
