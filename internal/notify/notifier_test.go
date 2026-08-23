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
	notification := Notification{Kind: NotificationDiagnosisCompleted, IncidentID: 7, RunID: uint64Ptr(11), Summary: "容器退出", Payload: map[string]any{"mode": "full", "rca": "容器退出", "confidence": "high", "decision": "allow", "plan_action": "restart_container", "plan_target": "container/sub2api", "plan_reason": "进程退出"}}
	if _, err := notifier.Send(context.Background(), notification); err != nil {
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
	if _, err := notifier.Send(context.Background(), Notification{}); err == nil {
		t.Fatal("Send() error = nil, want business failure")
	}
}

func TestWebhookNotifierHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()
	notifier, _ := NewWebhookNotifier(config.IMConfig{Provider: "feishu", Webhook: server.URL})
	if _, err := notifier.Send(context.Background(), Notification{}); err == nil {
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
	if _, err := n.Send(context.Background(), Notification{IncidentID: 1}); err != nil || called.Load() != 1 {
		t.Fatalf("NoopNotifier err = %v, calls = %d", err, called.Load())
	}
}

func TestRenderMarkdownGuardOverride(t *testing.T) {
	text := RenderMarkdown(Notification{Kind: NotificationDiagnosisCompleted, IncidentID: 1, RunID: uint64Ptr(2), Summary: "rca", Payload: map[string]any{"mode": "full", "confidence": "low", "decision": "escalate", "overridden": true, "guard_note": "restart blocked"}})
	if !strings.Contains(text, "Guard 改写了计划") || !strings.Contains(text, "restart blocked") {
		t.Fatalf("markdown missing guard note:\n%s", text)
	}
}

func TestRenderMarkdownApprovalCard(t *testing.T) {
	approvalID := uint64(7)
	text := RenderMarkdown(Notification{Kind: NotificationDiagnosisCompleted, IncidentID: 1, RunID: uint64Ptr(2), Summary: "rca", ApprovalID: &approvalID, Payload: map[string]any{"mode": "full", "confidence": "high", "decision": "allow", "policy_decision": "approval", "base_url": "http://127.0.0.1:8080/", "plan_action": "resize_pool", "plan_target": "service/sub2api", "plan_reason": "连接池不足"}})
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
	if strings.Contains(text, "//api") {
		t.Fatalf("double slash in card:\n%s", text)
	}
}

func uint64Ptr(v uint64) *uint64 { return &v }
