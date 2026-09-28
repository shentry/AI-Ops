// Package notify defines the provider-neutral notification boundary.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"oncall-agent/internal/config"
)

// NotificationKind identifies a business notification without exposing a
// provider-specific rendering contract to callers.
type NotificationKind string

const (
	NotificationIncidentFired      NotificationKind = "incident_fired"
	NotificationDiagnosisCompleted NotificationKind = "diagnosis_completed"
	NotificationApprovalRequired   NotificationKind = "approval_required"
	NotificationApprovalDecided    NotificationKind = "approval_decided"
	NotificationExecutionCompleted NotificationKind = "execution_completed"
	NotificationVerifyCompleted    NotificationKind = "verify_completed"
	NotificationEscalationRequired NotificationKind = "escalation_required"
	NotificationProblemDetected    NotificationKind = "problem_detected"
)

// Notification is the common fact passed to every delivery provider.
// Payload contains only kind-specific, already-sanitized display fields.
type Notification struct {
	Kind       NotificationKind
	IncidentID uint64
	RunID      *uint64
	ApprovalID *uint64
	Severity   string
	Title      string
	Summary    string
	Payload    map[string]any
}

// Delivery is the stable provider reference returned after a successful send.
// Webhook providers cannot return a message ID, so MessageID may be empty.
type Delivery struct {
	Provider  string
	MessageID string
}

// Notifier is the single notification exit. Delivery failures are reported to
// callers but must never change the terminal state of a diagnosis or action.
type Notifier interface {
	Send(context.Context, Notification) (Delivery, error)
}

// NoopNotifier is used when no provider is configured.
type NoopNotifier struct {
	Logf func(format string, args ...any)
}

func (n NoopNotifier) Send(_ context.Context, notification Notification) (Delivery, error) {
	if n.Logf != nil {
		n.Logf("notify: provider not configured, skip kind %s incident %d", notification.Kind, notification.IncidentID)
	}
	return Delivery{Provider: "noop"}, nil
}

// WebhookNotifier sends the provider-neutral notification through a legacy
// WeCom or Feishu custom-bot webhook. The HTTP client is reused process-wide.
type WebhookNotifier struct {
	webhook    string
	provider   string
	httpClient *http.Client
}

// NewWebhookNotifier constructs a legacy webhook provider. A blank provider
// with a non-empty webhook keeps the original WeCom-default configuration
// compatible. Validation errors never echo a webhook URL or embedded key.
func NewWebhookNotifier(cfg config.IMConfig) (*WebhookNotifier, error) {
	webhook := strings.TrimSpace(cfg.Webhook)
	if webhook == "" {
		return nil, errors.New("notify: webhook is required")
	}
	provider := strings.ToLower(strings.TrimSpace(cfg.Provider))
	if provider == "" {
		provider = "wecom"
	}
	switch provider {
	case "wecom", "feishu":
	default:
		return nil, fmt.Errorf("notify: unsupported provider %q", cfg.Provider)
	}
	parsed, err := url.ParseRequestURI(webhook)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, errors.New("notify: webhook is invalid")
	}
	return &WebhookNotifier{
		webhook:    webhook,
		provider:   provider,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}, nil
}

// Send renders the established Markdown fallback and posts it to the webhook.
// Webhook URLs frequently contain secret keys, so request errors are reduced to
// a safe classification instead of wrapping net/http's URL-bearing error.
func (n *WebhookNotifier) Send(ctx context.Context, notification Notification) (Delivery, error) {
	if err := n.sendText(ctx, RenderMarkdown(notification)); err != nil {
		return Delivery{}, err
	}
	return Delivery{Provider: n.provider}, nil
}

func (n *WebhookNotifier) sendText(ctx context.Context, markdown string) error {
	payload, err := n.payload(markdown)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.webhook, bytes.NewReader(payload))
	if err != nil {
		return errors.New("notify: build request failed")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.httpClient.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("notify: send failed: %w", ctxErr)
		}
		return errors.New("notify: send failed")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4097))
	if err != nil || len(body) > 4096 {
		return errors.New("notify: webhook acknowledgement unreadable or too large")
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("notify: webhook returned HTTP %d", resp.StatusCode)
	}
	var ack struct {
		ErrCode *int `json:"errcode"`
		Code    *int `json:"code"`
	}
	if json.Unmarshal(body, &ack) != nil {
		return errors.New("notify: webhook acknowledgement is not valid JSON")
	}
	code := ack.ErrCode
	if n.provider == "feishu" && ack.Code != nil {
		code = ack.Code
	}
	if code == nil || *code != 0 || ack.ErrCode != nil && *ack.ErrCode != 0 || ack.Code != nil && *ack.Code != 0 {
		return errors.New("notify: webhook did not explicitly acknowledge success")
	}
	return nil
}

func (n *WebhookNotifier) payload(markdown string) ([]byte, error) {
	var body any
	switch n.provider {
	case "wecom":
		body = map[string]any{"msgtype": "markdown", "markdown": map[string]any{"content": markdown}}
	case "feishu":
		body = map[string]any{"msg_type": "text", "content": map[string]any{"text": markdown}}
	default:
		return nil, errors.New("notify: unsupported webhook provider")
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("notify: encode payload: %w", err)
	}
	return encoded, nil
}

// RenderMarkdown preserves the established legacy webhook diagnosis card and
// supplies a compact fallback for every other notification kind.
func RenderMarkdown(notification Notification) string {
	switch notification.Kind {
	case NotificationDiagnosisCompleted:
		return renderDiagnosis(notification)
	case NotificationEscalationRequired:
		return RenderEscalation(notification)
	default:
		return renderGeneric(notification)
	}
}

func renderDiagnosis(notification Notification) string {
	var out strings.Builder
	fmt.Fprintf(&out, "**[AI-Opus] Incident #%d 诊断报告**\n", notification.IncidentID)
	runID := uint64(0)
	if notification.RunID != nil {
		runID = *notification.RunID
	}
	mode := payloadString(notification.Payload, "mode")
	fmt.Fprintf(&out, "> run: %d（mode: %s）\n", runID, mode)
	rca := payloadString(notification.Payload, "rca")
	if rca == "" {
		rca = notification.Summary
	}
	fmt.Fprintf(&out, "> RCA（置信度 %s）：%s\n", payloadString(notification.Payload, "confidence"), rca)
	if payloadBool(notification.Payload, "overridden") {
		fmt.Fprintf(&out, "> ⚠️ Guard 改写了计划：%s\n", payloadString(notification.Payload, "guard_note"))
	}
	fmt.Fprintf(&out, "> 决策：%s\n", payloadString(notification.Payload, "decision"))
	if policyDecision := payloadString(notification.Payload, "policy_decision"); policyDecision != "" {
		fmt.Fprintf(&out, "> 执行决策：%s\n", policyDecision)
	}
	planAction := payloadString(notification.Payload, "plan_action")
	if planAction != "" && planAction != "none" {
		fmt.Fprintf(&out, "> 建议动作：%s → %s（%s）\n", planAction, payloadString(notification.Payload, "plan_target"), payloadString(notification.Payload, "plan_reason"))
	}
	if notification.ApprovalID != nil {
		base := strings.TrimRight(payloadString(notification.Payload, "base_url"), "/")
		fmt.Fprintf(&out, "\n**审批单 #%d（等待审批）**\n", *notification.ApprovalID)
		fmt.Fprintf(&out, "批准：\n```\ncurl -X POST %s/api/v1/approvals/%d/approve -H 'Authorization: Bearer ${AUTH_TOKEN}' -H 'X-Operator: <你的工号>'\n```\n", base, *notification.ApprovalID)
		fmt.Fprintf(&out, "拒绝：\n```\ncurl -X POST %s/api/v1/approvals/%d/deny -H 'Authorization: Bearer ${AUTH_TOKEN}' -H 'X-Operator: <你的工号>'\n```\n", base, *notification.ApprovalID)
	}
	return out.String()
}

// RenderEscalation is the high-priority legacy webhook card.
func RenderEscalation(notification Notification) string {
	var out strings.Builder
	fmt.Fprintf(&out, "**[AI-Opus][紧急] Incident #%d 需要人工介入**\n", notification.IncidentID)
	reason := notification.Summary
	if reason == "" {
		reason = payloadString(notification.Payload, "reason")
	}
	fmt.Fprintf(&out, "> 自动处理失败：%s\n", reason)
	fmt.Fprintf(&out, "> 诊断 run 链：%v\n", payloadRunIDs(notification.Payload))
	return out.String()
}

func renderGeneric(notification Notification) string {
	var out strings.Builder
	title := strings.TrimSpace(notification.Title)
	if title == "" {
		title = strings.ReplaceAll(string(notification.Kind), "_", " ")
	}
	fmt.Fprintf(&out, "**[AI-Opus] %s**\n", title)
	if notification.IncidentID != 0 {
		fmt.Fprintf(&out, "> Incident #%d\n", notification.IncidentID)
	}
	if notification.RunID != nil {
		fmt.Fprintf(&out, "> run: %d\n", *notification.RunID)
	}
	if notification.ApprovalID != nil {
		fmt.Fprintf(&out, "> approval: %d\n", *notification.ApprovalID)
	}
	if notification.Severity != "" {
		fmt.Fprintf(&out, "> severity: %s\n", notification.Severity)
	}
	if notification.Summary != "" {
		fmt.Fprintf(&out, "> %s\n", notification.Summary)
	}
	return out.String()
}

func payloadString(payload map[string]any, key string) string {
	if payload == nil {
		return ""
	}
	value, _ := payload[key].(string)
	return value
}

func payloadBool(payload map[string]any, key string) bool {
	if payload == nil {
		return false
	}
	value, _ := payload[key].(bool)
	return value
}

func payloadRunIDs(payload map[string]any) []uint64 {
	if payload == nil {
		return nil
	}
	if ids, ok := payload["run_ids"].([]uint64); ok {
		return ids
	}
	values, ok := payload["run_ids"].([]any)
	if !ok {
		return nil
	}
	ids := make([]uint64, 0, len(values))
	for _, value := range values {
		switch id := value.(type) {
		case uint64:
			ids = append(ids, id)
		case int:
			if id >= 0 {
				ids = append(ids, uint64(id))
			}
		case float64:
			if id >= 0 {
				ids = append(ids, uint64(id))
			}
		}
	}
	return ids
}
