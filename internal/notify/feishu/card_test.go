package feishu

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"gorm.io/datatypes"
	"oncall-agent/internal/incident"
	"oncall-agent/internal/store"

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
		Payload:    completeCardPayload(t),
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
	if action != "approve" || gotID != approvalID || gotHash != completeCardPayload(t)["plan_hash"] {
		t.Fatalf("parsed = (%q, %d, %q)", action, gotID, gotHash)
	}
}

func completeCardApproval(t *testing.T) store.Approval {
	t.Helper()
	row := store.Approval{ID: 7, IncidentID: 11, RunID: 9, ToolName: "docker_restart", Status: "pending", Reason: "manual approval required", ExpiresAt: time.Now().UTC().Add(time.Hour), ArgsJSON: datatypes.JSON(`{"target_kind":"container","target_name":"sub2api"}`), ExecutionContext: datatypes.JSON(`{"safety_level":"L2","dry_run":false,"verification":{"kind":"sub2api_http_health","target_name":"sub2api","base_url":"http://private-target.invalid:8080","member_fingerprints":["private-member"],"interval_seconds":10,"window_seconds":120,"timeout_seconds":5}}`)}
	var err error
	row.PlanHash, err = incident.PlanHash(row.ToolName, row.ArgsJSON, row.ExecutionContext)
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func completeCardPayload(t *testing.T) map[string]any {
	row := completeCardApproval(t)
	return map[string]any{"plan_hash": row.PlanHash, "action": row.ToolName, "target": "container/sub2api", "scope": "single_container", "safety_level": "L2", "dry_run": false, "reason": row.Reason, "expires_at": row.ExpiresAt.Format(time.RFC3339), "web_url": "https://console.example.invalid/incidents/11"}
}

func TestApprovalCardSnapshotAndImmutableReferences(t *testing.T) {
	for _, dryRun := range []bool{false, true} {
		payload := completeCardPayload(t)
		payload["dry_run"] = dryRun
		payload["risk"] = "invented-model-risk"
		payload["execution_context"] = completeCardApproval(t).ExecutionContext
		payload["args_json"] = completeCardApproval(t).ArgsJSON
		payload["verification"] = map[string]any{"base_url": "http://private-target.invalid:8080"}
		id := uint64(7)
		card := BuildCard(notify.Notification{Kind: notify.NotificationApprovalRequired, IncidentID: 11, ApprovalID: &id, Payload: payload})
		encoded, err := MarshalCard(card)
		if err != nil {
			t.Fatal(err)
		}
		body := string(encoded)
		for _, expected := range []string{"container/sub2api", "single_container", "L2", "**Dry run:** " + map[bool]string{true: "true", false: "false"}[dryRun]} {
			if !strings.Contains(body, expected) {
				t.Fatalf("missing %q: %s", expected, body)
			}
		}
		for _, forbidden := range []string{"invented-model-risk", "private-target.invalid", "private-member", "args_json", "target_kind", "execution_context"} {
			if strings.Contains(body, forbidden) {
				t.Fatalf("leaked %q: %s", forbidden, body)
			}
		}
		value := firstCallbackValue(t, card)
		if len(value) != 3 || value["action"] != "approve" || value["approval_id"] != id || value["plan_hash"] != payload["plan_hash"] {
			t.Fatalf("callback must contain only immutable references: %#v", value)
		}
	}
}

func TestApprovalCardIncompleteSnapshotHasNoApprove(t *testing.T) {
	for _, key := range []string{"plan_hash", "action", "target", "scope", "safety_level", "dry_run", "reason", "expires_at"} {
		t.Run(key, func(t *testing.T) {
			payload := completeCardPayload(t)
			delete(payload, key)
			id := uint64(7)
			encoded, err := RenderCard(notify.Notification{Kind: notify.NotificationApprovalRequired, IncidentID: 11, ApprovalID: &id, Payload: payload})
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(encoded), `"action":"approve"`) {
				t.Fatalf("incomplete %s still approves: %s", key, encoded)
			}
			if key == "dry_run" && (!strings.Contains(string(encoded), "**Dry run:** unknown") || strings.Contains(string(encoded), "**Dry run:** false")) {
				t.Fatalf("missing dry-run misrepresented: %s", encoded)
			}
		})
	}
	for _, invalid := range []any{"false", nil, 0} {
		payload := completeCardPayload(t)
		payload["dry_run"] = invalid
		id := uint64(7)
		encoded, _ := RenderCard(notify.Notification{ApprovalID: &id, Payload: payload})
		if strings.Contains(string(encoded), `"action":"approve"`) || !strings.Contains(string(encoded), "**Dry run:** unknown") {
			t.Fatalf("invalid dry-run accepted: %s", encoded)
		}
	}
	payload := completeCardPayload(t)
	payload["safety_level"] = "model-low"
	id := uint64(7)
	encoded, _ := json.Marshal(BuildCard(notify.Notification{ApprovalID: &id, Payload: payload}))
	if strings.Contains(string(encoded), `"action":"approve"`) {
		t.Fatalf("invalid level accepted: %s", encoded)
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
