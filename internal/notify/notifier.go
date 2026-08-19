// Package notify 是通知抽象层（D09）：上层只拼报告内容，
// provider 差异（wecom/feishu）在实现里各自消化。
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"oncall-agent/internal/config"
)

// DiagnosisMessage 是跨 provider 的通知内容载体。
// 与 diagnose.DiagnosisReport 解耦：notify 不 import diagnose，
// 避免 diagnose → notify → diagnose 的环。
type DiagnosisMessage struct {
	IncidentID uint64
	RunID      uint64
	Mode       string
	RCA        string
	Confidence string
	Decision   string
	Overridden bool
	GuardNote  string
	PlanAction string
	PlanTarget string
	PlanReason string
	// D10：policy 结论与审批单。ApprovalID 非空时卡片附审批 curl。
	PolicyDecision string
	ApprovalID     *uint64
	// BaseURL 是审批 API 的服务地址，由组装层注入。
	BaseURL string
}

// Notifier 是通知出口。发送失败返回 error，由调用方决定记日志还是重试；
// 通知永远不能反过来改变诊断终态。
type Notifier interface {
	Send(ctx context.Context, msg DiagnosisMessage) error
	// SendEscalation 发高优先级人工升级通知（D12：自动处理失败）。
	SendEscalation(ctx context.Context, msg EscalationMessage) error
}

// EscalationMessage 是人工升级通知：失败原因 + 完整 run 链。
type EscalationMessage struct {
	IncidentID uint64
	RunIDs     []uint64
	Reason     string
}

// NoopNotifier 在 webhook 未配置时使用：记日志，不发外部请求。
type NoopNotifier struct {
	Logf func(format string, args ...any)
}

func (n NoopNotifier) Send(_ context.Context, msg DiagnosisMessage) error {
	if n.Logf != nil {
		n.Logf("notify: webhook not configured, skip incident %d run %d", msg.IncidentID, msg.RunID)
	}
	return nil
}

func (n NoopNotifier) SendEscalation(_ context.Context, msg EscalationMessage) error {
	if n.Logf != nil {
		n.Logf("notify: webhook not configured, skip escalation incident %d runs %v", msg.IncidentID, msg.RunIDs)
	}
	return nil
}

// SendEscalation 复用 webhook 通道，升级文案前缀区别于普通报告。
func (n *WebhookNotifier) SendEscalation(ctx context.Context, msg EscalationMessage) error {
	return n.sendText(ctx, RenderEscalation(msg))
}

// RenderEscalation 是人工升级卡片：失败原因 + 完整 run 链。
func RenderEscalation(msg EscalationMessage) string {
	var out strings.Builder
	fmt.Fprintf(&out, "**[AI-Opus][紧急] Incident #%d 需要人工介入**\n", msg.IncidentID)
	fmt.Fprintf(&out, "> 自动处理失败：%s\n", msg.Reason)
	fmt.Fprintf(&out, "> 诊断 run 链：%v\n", msg.RunIDs)
	return out.String()
}

// WebhookNotifier 通过 IM webhook 发送。进程级复用 HTTP client。
type WebhookNotifier struct {
	webhook    string
	provider   string
	httpClient *http.Client
}

// NewWebhookNotifier 按 provider 构造。webhook 为空返回错误 ——
// 调用方应改用 NoopNotifier，不允许"空地址静默发不出去"。
func NewWebhookNotifier(cfg config.IMConfig) (*WebhookNotifier, error) {
	if strings.TrimSpace(cfg.Webhook) == "" {
		return nil, errors.New("notify: webhook is required")
	}
	switch cfg.Provider {
	case "wecom", "feishu":
	default:
		return nil, fmt.Errorf("notify: unsupported provider %q", cfg.Provider)
	}
	return &WebhookNotifier{
		webhook:    cfg.Webhook,
		provider:   cfg.Provider,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}, nil
}

// Send 拼文本并发 webhook。webhook URL 可能含 key，错误文本不回显 URL。
func (n *WebhookNotifier) Send(ctx context.Context, msg DiagnosisMessage) error {
	return n.sendText(ctx, RenderMarkdown(msg))
}

// sendText 是两类消息共用的发送路径。
func (n *WebhookNotifier) sendText(ctx context.Context, markdown string) error {
	payload, err := n.payload(markdown)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.webhook, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("notify: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("notify: send failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("notify: webhook returned HTTP %d", resp.StatusCode)
	}
	// wecom/feishu 都用 {"errcode":0} 表示成功；非 0 是业务失败。
	var ack struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(body, &ack); err == nil && ack.ErrCode != 0 {
		return fmt.Errorf("notify: webhook rejected: errcode=%d", ack.ErrCode)
	}
	return nil
}

// payload 按 provider 拼请求体。wecom 支持 markdown，feishu 用纯文本。
func (n *WebhookNotifier) payload(markdown string) ([]byte, error) {
	var body any
	switch n.provider {
	case "wecom":
		body = map[string]any{"msgtype": "markdown", "markdown": map[string]any{"content": markdown}}
	case "feishu":
		body = map[string]any{"msg_type": "text", "content": map[string]any{"text": markdown}}
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("notify: encode payload: %w", err)
	}
	return encoded, nil
}

// RenderMarkdown 是诊断报告的卡片文案模板（A8 模板直译的 Go 版）。
func RenderMarkdown(msg DiagnosisMessage) string {
	var out strings.Builder
	fmt.Fprintf(&out, "**[AI-Opus] Incident #%d 诊断报告**\n", msg.IncidentID)
	fmt.Fprintf(&out, "> run: %d（mode: %s）\n", msg.RunID, msg.Mode)
	fmt.Fprintf(&out, "> RCA（置信度 %s）：%s\n", msg.Confidence, msg.RCA)
	if msg.Overridden {
		fmt.Fprintf(&out, "> ⚠️ Guard 改写了计划：%s\n", msg.GuardNote)
	}
	fmt.Fprintf(&out, "> 决策：%s\n", msg.Decision)
	if msg.PolicyDecision != "" {
		fmt.Fprintf(&out, "> 执行决策：%s\n", msg.PolicyDecision)
	}
	if msg.PlanAction != "" && msg.PlanAction != "none" {
		fmt.Fprintf(&out, "> 建议动作：%s → %s（%s）\n", msg.PlanAction, msg.PlanTarget, msg.PlanReason)
	}
	// L3 审批卡片：附 approve/deny curl。token 用环境变量占位 ——
	// 真实凭据不进 IM 通道（GC-19 优先于"自带 token"的便利性）。
	if msg.ApprovalID != nil {
		base := strings.TrimRight(msg.BaseURL, "/")
		fmt.Fprintf(&out, "\n**审批单 #%d（等待审批）**\n", *msg.ApprovalID)
		fmt.Fprintf(&out, "批准：\n```\ncurl -X POST %s/api/v1/approvals/%d/approve -H 'Authorization: Bearer ${AUTH_TOKEN}' -H 'X-Operator: <你的工号>'\n```\n", base, *msg.ApprovalID)
		fmt.Fprintf(&out, "拒绝：\n```\ncurl -X POST %s/api/v1/approvals/%d/deny -H 'Authorization: Bearer ${AUTH_TOKEN}' -H 'X-Operator: <你的工号>'\n```\n", base, *msg.ApprovalID)
	}
	return out.String()
}
