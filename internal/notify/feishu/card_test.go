package feishu

import (
	"strings"
	"testing"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"

	"oncall-agent/internal/notify"
)

func TestRenderCardOmitsSecretsAndIncludesIncident(t *testing.T) {
	encoded, err := RenderCard(notify.Notification{
		Kind:       notify.NotificationApprovalRequired,
		IncidentID: 11,
		Severity:   "sev-1",
		Title:      "需要审批",
		Summary:    "重启容器",
		Payload: map[string]any{
			"action":     "restart",
			"app_secret": "should-not-appear",
			"webhook":    "https://example.invalid/secret",
		},
	})
	if err != nil {
		t.Fatalf("RenderCard: %v", err)
	}
	body := string(encoded)
	if !strings.Contains(body, "Incident") || !strings.Contains(body, "#11") {
		t.Fatalf("missing incident: %s", body)
	}
	if strings.Contains(body, "should-not-appear") || strings.Contains(body, "example.invalid/secret") {
		t.Fatalf("secret leaked: %s", body)
	}
	if !strings.Contains(body, `"schema":"2.0"`) && !strings.Contains(body, `"schema": "2.0"`) {
		if !strings.Contains(body, "2.0") {
			t.Fatalf("not card 2.0: %s", body)
		}
	}
}

func TestRenderCardSanitizesSelectedDisplayFields(t *testing.T) {
	encoded, err := RenderCard(notify.Notification{
		IncidentID: 11,
		Title:      "token=title-secret",
		Summary:    "dsn: mysql://ops:summary-secret@db.internal/oncall",
		Payload: map[string]any{
			"reason":  "Authorization: Bearer payload-secret",
			"web_url": "https://console.example.invalid/incidents/11",
		},
	})
	if err != nil {
		t.Fatalf("RenderCard: %v", err)
	}
	body := string(encoded)
	for _, secret := range []string{"title-secret", "summary-secret", "payload-secret"} {
		if strings.Contains(body, secret) {
			t.Fatalf("secret %q leaked: %s", secret, body)
		}
	}
}

// 卡片按钮的 value 必须能被自己的回调解析器接受。现有回调测试自行拼造
// value，覆盖不到这条接缝：payload 少一个 plan_hash 就会让按钮永远无效。
func TestApprovalButtonValueParsesInCallback(t *testing.T) {
	approvalID := uint64(42)
	card := BuildCard(notify.Notification{
		Kind:       notify.NotificationDiagnosisCompleted,
		IncidentID: 11,
		ApprovalID: &approvalID,
		Title:      "诊断报告",
		Summary:    "连接池耗尽",
		Payload:    map[string]any{"plan_hash": "abc"},
	})
	value := firstCallbackValue(t, card)
	action, gotID, gotHash, err := cardActionValue(&callback.CardActionTriggerEvent{
		Event: &callback.CardActionTriggerRequest{
			Operator: &callback.Operator{OpenID: "ou_1"},
			Action:   &callback.CallBackAction{Value: value},
		},
	})
	if err != nil {
		t.Fatalf("cardActionValue() error = %v, value = %v", err, value)
	}
	if action != "approve" || gotID != approvalID || gotHash != "abc" {
		t.Fatalf("parsed = (%q, %d, %q)", action, gotID, gotHash)
	}
}

// firstCallbackValue 取卡片里第一个 callback 按钮的 value。
func firstCallbackValue(t *testing.T, card map[string]any) map[string]any {
	t.Helper()
	body, _ := card["body"].(map[string]any)
	pending, _ := body["elements"].([]any)
	for len(pending) > 0 {
		node, _ := pending[0].(map[string]any)
		pending = pending[1:]
		if node["tag"] == "column_set" {
			columns, _ := node["columns"].([]any)
			for _, column := range columns {
				col, _ := column.(map[string]any)
				inner, _ := col["elements"].([]any)
				pending = append(pending, inner...)
			}
			continue
		}
		behaviors, _ := node["behaviors"].([]any)
		for _, behavior := range behaviors {
			b, _ := behavior.(map[string]any)
			if b["type"] != "callback" {
				continue
			}
			if value, ok := b["value"].(map[string]any); ok {
				return value
			}
		}
	}
	t.Fatal("card has no callback button")
	return nil
}
