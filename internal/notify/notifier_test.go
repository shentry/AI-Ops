package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"oncall-agent/internal/config"
)

func TestWebhookNotifierWecom(t *testing.T) {
	var received map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&received)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"errcode":0,"errmsg":"ok"}`))
	}))
	defer server.Close()

	notifier, err := NewWebhookNotifier(config.IMConfig{Provider: "wecom", Webhook: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	msg := DiagnosisMessage{IncidentID: 7, RunID: 11, Mode: "full", RCA: "容器退出", Confidence: "high", Decision: "allow", PlanAction: "restart_container", PlanTarget: "container/sub2api", PlanReason: "进程退出"}
	if err := notifier.Send(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	if received["msgtype"] != "markdown" {
		t.Fatalf("payload = %v", received)
	}
	content := received["markdown"].(map[string]any)["content"].(string)
	for _, want := range []string{"Incident #7", "run: 11", "容器退出", "restart_container", "allow"} {
		if !strings.Contains(content, want) {
			t.Fatalf("markdown missing %q:\n%s", want, content)
		}
	}
}

func TestWebhookNotifierBusinessError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"errcode":93000,"errmsg":"invalid webhook"}`))
	}))
	defer server.Close()
	notifier, err := NewWebhookNotifier(config.IMConfig{Provider: "wecom", Webhook: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	if err := notifier.Send(context.Background(), DiagnosisMessage{}); err == nil {
		t.Fatal("Send() error = nil, want business failure")
	}
}

func TestWebhookNotifierHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()
	notifier, _ := NewWebhookNotifier(config.IMConfig{Provider: "feishu", Webhook: server.URL})
	if err := notifier.Send(context.Background(), DiagnosisMessage{}); err == nil {
		t.Fatal("Send() error = nil, want HTTP failure")
	}
}

func TestNewWebhookNotifierValidation(t *testing.T) {
	if _, err := NewWebhookNotifier(config.IMConfig{Provider: "wecom"}); err == nil {
		t.Fatal("empty webhook accepted")
	}
	if _, err := NewWebhookNotifier(config.IMConfig{Provider: "slack", Webhook: "http://x"}); err == nil {
		t.Fatal("unsupported provider accepted")
	}
}

func TestNoopNotifier(t *testing.T) {
	called := atomic.Int32{}
	n := NoopNotifier{Logf: func(string, ...any) { called.Add(1) }}
	if err := n.Send(context.Background(), DiagnosisMessage{IncidentID: 1}); err != nil || called.Load() != 1 {
		t.Fatalf("NoopNotifier err = %v, calls = %d", err, called.Load())
	}
}

func TestRenderMarkdownGuardOverride(t *testing.T) {
	text := RenderMarkdown(DiagnosisMessage{IncidentID: 1, RunID: 2, Mode: "full", RCA: "rca", Confidence: "low", Decision: "escalate", Overridden: true, GuardNote: "restart blocked"})
	if !strings.Contains(text, "Guard 改写了计划") || !strings.Contains(text, "restart blocked") {
		t.Fatalf("markdown missing guard note:\n%s", text)
	}
}

func TestRenderMarkdownApprovalCard(t *testing.T) {
	approvalID := uint64(7)
	text := RenderMarkdown(DiagnosisMessage{
		IncidentID: 1, RunID: 2, Mode: "full", RCA: "rca", Confidence: "high",
		Decision: "allow", PolicyDecision: "approval",
		ApprovalID: &approvalID, BaseURL: "http://127.0.0.1:8080/",
		PlanAction: "resize_pool", PlanTarget: "service/sub2api", PlanReason: "连接池不足",
	})
	for _, want := range []string{
		"审批单 #7（等待审批）",
		"http://127.0.0.1:8080/api/v1/approvals/7/approve",
		"http://127.0.0.1:8080/api/v1/approvals/7/deny",
		"Bearer ${AUTH_TOKEN}",
		"执行决策：approval",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("approval card missing %q:\n%s", want, text)
		}
	}
	// BaseURL 尾斜杠不产生双斜杠。
	if strings.Contains(text, "//api") {
		t.Fatalf("double slash in card:\n%s", text)
	}
}
