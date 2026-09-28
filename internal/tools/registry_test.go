package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func okHandler(output string) Handler {
	return func(context.Context, json.RawMessage) (string, error) { return output, nil }
}

func testSpec(name string) ToolSpec {
	return ToolSpec{
		Name: name, Description: "test tool",
		Timeout: time.Second, MaxOutput: 16, Handler: okHandler("ok"),
	}
}

func TestRegistryRegisterValidation(t *testing.T) {
	tests := []struct {
		name string
		spec ToolSpec
	}{
		{"empty name", ToolSpec{Description: "d", Timeout: time.Second, Handler: okHandler("ok")}},
		{"empty description", ToolSpec{Name: "a", Timeout: time.Second, Handler: okHandler("ok")}},
		{"nil handler", ToolSpec{Name: "a", Description: "d", Timeout: time.Second}},
		{"non-positive timeout", ToolSpec{Name: "a", Description: "d", Handler: okHandler("ok")}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := NewRegistry().Register(test.spec); err == nil {
				t.Fatalf("Register(%s) error = nil, want validation failure", test.name)
			}
		})
	}
}

func TestRegistryRejectsDuplicateName(t *testing.T) {
	registry := NewRegistry()
	if err := registry.Register(testSpec("dup")); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(testSpec("dup")); err == nil {
		t.Fatal("Register(duplicate) error = nil, want failure")
	}
	if err := registry.RegisterAction(stubAction{def: ActionDefinition{Name: "dup", Version: 1, TargetKind: "container", Description: "d", Timeout: time.Second}}); err == nil {
		t.Fatal("action shadowing a tool accepted")
	}
}

type stubAction struct {
	Action
	def ActionDefinition
}

func (a stubAction) Definition() ActionDefinition { return a.def }

// Writes are never callable tools: the model sees only read tools, and may
// merely name a plannable action in its plan.
func TestRegistrySeparatesReadToolsFromActions(t *testing.T) {
	registry := NewRegistry()
	for _, spec := range []ToolSpec{testSpec("z_read"), testSpec("a_read")} {
		if err := registry.Register(spec); err != nil {
			t.Fatal(err)
		}
	}
	for _, def := range []ActionDefinition{
		{Name: "restart", Version: 1, TargetKind: "container", Description: "d", Timeout: time.Second},
		{Name: "undo", Version: 1, TargetKind: "container", Description: "d", Timeout: time.Second, Compensation: true},
	} {
		if err := registry.RegisterAction(stubAction{def: def}); err != nil {
			t.Fatal(err)
		}
	}
	if err := registry.RegisterAction(stubAction{def: ActionDefinition{Name: "bad"}}); err == nil {
		t.Fatal("incomplete action definition accepted")
	}
	exposed := registry.ForLLM()
	if len(exposed) != 2 || exposed[0].Name != "a_read" || exposed[1].Name != "z_read" {
		t.Fatalf("ForLLM() = %+v", exposed)
	}
	if _, err := registry.Execute(context.Background(), "restart", nil); !errors.Is(err, ErrToolNotRegistered) {
		t.Fatalf("action executed as a tool: %v", err)
	}
	plannable := registry.PlannableActions()
	if len(plannable) != 1 || plannable[0].Name != "restart" || len(registry.ActionDefinitions()) != 2 {
		t.Fatalf("plannable = %+v", plannable)
	}
}

func TestRegistryExecuteRejectsUnregistered(t *testing.T) {
	registry := NewRegistry()
	_, err := registry.Execute(context.Background(), "ghost", nil)
	if !errors.Is(err, ErrToolNotRegistered) {
		t.Fatalf("Execute() error = %v, want ErrToolNotRegistered", err)
	}
}

func TestRegistryExecuteAppliesTimeout(t *testing.T) {
	registry := NewRegistry()
	if err := registry.Register(ToolSpec{
		Name: "slow", Description: "slow tool",
		Timeout: 20 * time.Millisecond, MaxOutput: 16,
		Handler: func(ctx context.Context, _ json.RawMessage) (string, error) {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(5 * time.Second):
				return "done", nil
			}
		},
	}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err := registry.Execute(context.Background(), "slow", nil)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("Execute() error = %v, want timeout", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Execute() took %s, want bounded by tool timeout", elapsed)
	}
}

func TestRegistryExecuteTruncatesOutput(t *testing.T) {
	registry := NewRegistry()
	if err := registry.Register(ToolSpec{
		Name: "loud", Description: "loud tool",
		Timeout: time.Second, MaxOutput: 8,
		Handler: okHandler(strings.Repeat("数", 32)),
	}); err != nil {
		t.Fatal(err)
	}
	output, err := registry.Execute(context.Background(), "loud", nil)
	if err != nil {
		t.Fatal(err)
	}
	// 截断按 rune：8 个汉字 + 截断标记，不会出现半个 UTF-8 字符。
	if !strings.HasPrefix(output, strings.Repeat("数", 8)) || !strings.HasSuffix(output, "[truncated]") {
		t.Fatalf("Execute() output = %q, want 8 runes + truncation marker", output)
	}
}

// 模型和回放看到的是同一份脱敏文本：日志里的凭据和控制字符在出 Registry 前就处理掉。
func TestRegistryExecuteSanitizesOutput(t *testing.T) {
	registry := NewRegistry()
	if err := registry.Register(ToolSpec{
		Name: "logs", Description: "log tool", Timeout: time.Second,
		Handler: okHandler("connect postgres://app:s3cret@db/sub2api token=abc123\x1b[31m"),
	}); err != nil {
		t.Fatal(err)
	}
	output, err := registry.Execute(context.Background(), "logs", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, leaked := range []string{"s3cret", "abc123", "\x1b"} {
		if strings.Contains(output, leaked) {
			t.Fatalf("Execute() output = %q leaked %q", output, leaked)
		}
	}
}

func TestRegistryExecuteWrapsHandlerError(t *testing.T) {
	registry := NewRegistry()
	if err := registry.Register(ToolSpec{
		Name: "broken", Description: "broken tool",
		Timeout: time.Second, MaxOutput: 16,
		Handler: func(context.Context, json.RawMessage) (string, error) { return "", errors.New("boom") },
	}); err != nil {
		t.Fatal(err)
	}
	_, err := registry.Execute(context.Background(), "broken", nil)
	if err == nil || !strings.Contains(err.Error(), "broken failed: boom") {
		t.Fatalf("Execute() error = %v", err)
	}
}

func TestTruncate(t *testing.T) {
	if got := Truncate("short", 10); got != "short" {
		t.Fatalf("Truncate(short) = %q", got)
	}
	if got := Truncate("short", 0); got != "short" {
		t.Fatalf("Truncate(no limit) = %q", got)
	}
	got := Truncate("你好世界", 2)
	if got != "你好…[truncated]" {
		t.Fatalf("Truncate(multibyte) = %q", got)
	}
}
