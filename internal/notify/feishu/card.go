package feishu

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"oncall-agent/internal/notify"
	"oncall-agent/internal/tools"
)

// MaxCardBytes is Feishu's maximum serialized size for an interactive card.
// Keep a small amount of headroom for the enclosing message request.
const MaxCardBytes = 30 * 1024

const (
	// Keep individual fields bounded so a populated notification remains below
	// Feishu's 30 KiB serialized-card limit even with several summary fields.
	maxCardTextRunes = 1024
	maxCardSummary   = 4096
)

// BuildCard builds a Card JSON 2.0 document from a notification. Payload fields
// are intentionally selected rather than copied wholesale: notifications can
// contain provider-specific data, while a card must remain a bounded,
// credential-free summary.
func BuildCard(n notify.Notification) map[string]any {
	title := cardText(n.Title, maxCardTextRunes)
	if title == "" {
		if n.IncidentID > 0 {
			title = fmt.Sprintf("Incident #%d", n.IncidentID)
		} else {
			title = "On-call notification"
		}
	}
	summary := cardText(n.Summary, maxCardSummary)
	if summary == "" {
		summary = payloadText(n.Payload, "summary")
	}

	severity := cardText(n.Severity, maxCardTextRunes)
	kind := cardText(string(n.Kind), maxCardTextRunes)
	if kind == "" {
		kind = "notification"
	}
	lines := make([]string, 0, 10)
	if severity != "" {
		lines = append(lines, "**Severity:** "+severity)
	}
	if kind != "" {
		lines = append(lines, "**Kind:** "+kind)
	}
	if n.IncidentID > 0 {
		lines = append(lines, fmt.Sprintf("**Incident:** #%d", n.IncidentID))
	}
	if n.RunID != nil {
		lines = append(lines, fmt.Sprintf("**Run:** #%d", *n.RunID))
	}
	if summary != "" {
		lines = append(lines, summary)
	}

	for _, field := range []struct {
		label string
		keys  []string
	}{
		{label: "Action", keys: []string{"action", "plan_action"}},
		{label: "Target", keys: []string{"target"}},
		{label: "Target ID", keys: []string{"target_id"}},
		{label: "Rule", keys: []string{"rule_id"}},
		{label: "Mode", keys: []string{"rule_mode"}},
		{label: "Reason", keys: []string{"reason", "plan_reason"}},
		{label: "Expires at", keys: []string{"expires_at"}},
		{label: "RCA", keys: []string{"rca"}},
		{label: "Confidence", keys: []string{"confidence"}},
		{label: "Result", keys: []string{"result", "result_summary"}},
		{label: "Verify", keys: []string{"verify", "verify_status"}},
		{label: "Problems", keys: []string{"problems", "open_problems"}},
	} {
		if value := payloadFirstText(n.Payload, field.keys...); value != "" {
			lines = append(lines, "**"+field.label+":** "+truncate(value, maxCardTextRunes))
		}
	}

	approvalID, hasApproval := notificationApprovalID(n)
	decisionReady := approvalCardReady(n.Payload)
	if hasApproval && !decisionReady {
		lines = append(lines, "执行上下文不完整，无法批准；请在 Web 控制室查看并重新诊断。")
	}

	elements := make([]any, 0, len(lines)+2)
	for _, line := range lines {
		elements = append(elements, map[string]any{
			"tag":     "markdown",
			"content": truncate(line, maxCardTextRunes),
		})
	}

	webURL := safeCardURL(payloadFirstText(n.Payload, "web_url", "webURL", "url"), n.IncidentID)
	if webURL != "" {
		elements = append(elements, map[string]any{
			"tag":  "button",
			"text": map[string]any{"tag": "plain_text", "content": "打开 Web 控制室"},
			"type": "default",
			"behaviors": []any{map[string]any{
				"type":        "open_url",
				"default_url": webURL,
			}},
		})
	}
	planHash, _ := n.Payload["plan_hash"].(string)
	if hasApproval && strings.TrimSpace(planHash) != "" {
		buttons := make([]any, 0, 2)
		if decisionReady {
			buttons = append(buttons, ApprovalButton("approve", "批准", approvalID, planHash, "primary"))
		}
		buttons = append(buttons, ApprovalButton("deny", "拒绝", approvalID, planHash, "danger"))
		elements = append(elements, map[string]any{
			"tag":       "column_set",
			"flex_mode": "none",
			"columns": []any{map[string]any{
				"tag":      "column",
				"width":    "weighted",
				"weight":   1,
				"elements": buttons,
			}},
		})
	}

	cardSummary := truncate(strings.TrimSpace(strings.Join([]string{title, summary}, " ")), maxCardTextRunes)
	if cardSummary == "" {
		cardSummary = title
	}
	return map[string]any{
		"schema": "2.0",
		"config": map[string]any{
			"update_multi": true,
			"summary":      map[string]any{"content": cardSummary},
		},
		"header": map[string]any{
			"title":    map[string]any{"tag": "plain_text", "content": truncate(title, maxCardTextRunes)},
			"template": cardTemplate(n),
		},
		"body": map[string]any{
			"direction": "vertical",
			"elements":  elements,
		},
	}
}

// RenderCard serializes a notification as a bounded Card 2.0 JSON document.
// The returned bytes are suitable for the SDK's interactive message Content.
func RenderCard(n notify.Notification) ([]byte, error) {
	return MarshalCard(BuildCard(n))
}

// RenderCardJSON is the string form used by SDK request builders.
func RenderCardJSON(n notify.Notification) (string, error) {
	encoded, err := RenderCard(n)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

// RenderNotificationCard names the provider-neutral entry point explicitly.
func RenderNotificationCard(n notify.Notification) ([]byte, error) {
	return RenderCard(n)
}

// MarshalCard serializes a Card 2.0 value and rejects oversized payloads before
// making an external request. Callers should truncate source summaries before
// constructing arbitrary custom cards.
func MarshalCard(card any) ([]byte, error) {
	encoded, err := json.Marshal(card)
	if err != nil {
		return nil, fmt.Errorf("feishu: marshal card: %w", err)
	}
	if len(encoded) > MaxCardBytes {
		return nil, fmt.Errorf("feishu: card exceeds %d bytes", MaxCardBytes)
	}
	return encoded, nil
}

// ApprovalButton returns a Card 2.0 callback button. Its callback value is
// deliberately limited to immutable references; executable arguments and
// credentials never travel through the card.
func ApprovalButton(action, label string, approvalID uint64, planHash, buttonType string) map[string]any {
	value := map[string]any{
		"action":      action,
		"approval_id": approvalID,
	}
	value["plan_hash"] = planHash
	return map[string]any{
		"tag":  "button",
		"text": map[string]any{"tag": "plain_text", "content": truncate(label, maxCardTextRunes)},
		"type": buttonType,
		"behaviors": []any{map[string]any{
			"type":  "callback",
			"value": value,
		}},
	}
}

// A person may approve only a complete manual snapshot: its action, target
// identity, rule, reason, hash and expiry are all shown. Display-only payloads
// cannot recreate a legacy snapshot.
func approvalCardReady(payload map[string]any) bool {
	text := func(key string) string {
		value, _ := payload[key].(string)
		return strings.TrimSpace(value)
	}
	kind, name, found := strings.Cut(text("target"), "/")
	expiresAt, err := time.Parse(time.RFC3339, text("expires_at"))
	return text("action") != "" && found && kind != "" && name != "" && text("target_id") != "" && text("rule_id") != "" && text("rule_mode") == "manual" &&
		text("plan_hash") != "" && text("reason") != "" && err == nil && !expiresAt.IsZero()
}

func notificationApprovalID(n notify.Notification) (uint64, bool) {
	if n.ApprovalID != nil && *n.ApprovalID > 0 {
		return *n.ApprovalID, true
	}
	if n.Payload == nil {
		return 0, false
	}
	value, ok := n.Payload["approval_id"]
	if !ok || value == nil {
		return 0, false
	}
	switch typed := value.(type) {
	case uint64:
		return typed, typed > 0
	case uint:
		return uint64(typed), typed > 0
	case int:
		return uint64(typed), typed > 0
	case int64:
		return uint64(typed), typed > 0
	case float64:
		return uint64(typed), typed > 0 && typed == float64(uint64(typed))
	case string:
		id, err := strconv.ParseUint(strings.TrimSpace(typed), 10, 64)
		return id, err == nil && id > 0
	default:
		return 0, false
	}
}

func cardTemplate(n notify.Notification) string {
	switch strings.ToLower(strings.TrimSpace(n.Severity)) {
	case "critical", "high":
		return "red"
	case "warning", "medium":
		return "orange"
	case "info", "low":
		return "blue"
	default:
		return "default"
	}
}

func payloadFirstText(payload map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := payloadText(payload, key); value != "" {
			return value
		}
	}
	return ""
}

func payloadText(payload map[string]any, key string) string {
	if payload == nil {
		return ""
	}
	value, ok := payload[key]
	if !ok || value == nil {
		return ""
	}
	var raw string
	switch typed := value.(type) {
	case string:
		raw = typed
	case json.Number:
		raw = typed.String()
	case bool:
		raw = strconv.FormatBool(typed)
	case float64:
		raw = strconv.FormatFloat(typed, 'f', -1, 64)
	case float32:
		raw = strconv.FormatFloat(float64(typed), 'f', -1, 32)
	case int:
		raw = strconv.Itoa(typed)
	case int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		raw = fmt.Sprint(typed)
	default:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return ""
		}
		raw = string(encoded)
	}
	return cardText(raw, maxCardTextRunes)
}

func cardText(value string, maxRunes int) string {
	return truncate(strings.TrimSpace(tools.Sanitize(tools.ToSafeText(value))), maxRunes)
}

func safeCardURL(raw string, incidentID uint64) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" || parsed.User != nil {
		return ""
	}
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && isLoopbackHost(parsed.Hostname())) {
		return ""
	}
	if incidentID > 0 && !strings.Contains(parsed.Path, "/incidents/"+strconv.FormatUint(incidentID, 10)) {
		return ""
	}
	return parsed.String()
}

func isLoopbackHost(host string) bool {
	host = strings.TrimSpace(strings.ToLower(host))
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func truncate(value string, maxRunes int) string {
	if maxRunes <= 0 || utf8.RuneCountInString(value) <= maxRunes {
		return value
	}
	runes := []rune(value)
	return string(runes[:maxRunes]) + "…"
}
