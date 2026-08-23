package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"oncall-agent/internal/config"
	"oncall-agent/internal/notify"
)

// Config contains only the Feishu application settings needed by this
// adapter. Secrets are retained by the SDK in memory and are never included
// in errors or message payloads.
type Config struct {
	AppID             string
	AppSecret         string
	VerificationToken string
	EncryptKey        string
	ChatID            string
	WebBaseURL        string
	// HTTPClient and OpenBaseURL are optional test/edge-environment hooks;
	// production uses the SDK defaults.
	HTTPClient  larkcore.HttpClient
	OpenBaseURL string
}

// ClientOption customizes the convenience NewClient constructor.
type ClientOption func(*Config)

func WithChatID(chatID string) ClientOption {
	return func(cfg *Config) { cfg.ChatID = chatID }
}

func WithWebBaseURL(baseURL string) ClientOption {
	return func(cfg *Config) { cfg.WebBaseURL = baseURL }
}

func WithVerificationToken(token string) ClientOption {
	return func(cfg *Config) { cfg.VerificationToken = token }
}

func WithHTTPClient(client larkcore.HttpClient) ClientOption {
	return func(cfg *Config) { cfg.HTTPClient = client }
}

func WithOpenBaseURL(baseURL string) ClientOption {
	return func(cfg *Config) { cfg.OpenBaseURL = baseURL }
}

func WithEncryptKey(key string) ClientOption {
	return func(cfg *Config) { cfg.EncryptKey = key }
}

// Client is the Feishu application-bot adapter. The SDK client owns
// tenant_access_token acquisition and its process-local cache.
type Client struct {
	sdk        *lark.Client
	chatID     string
	webBaseURL string
	config     Config
}

// New constructs an application client without making a network request.
// Message API calls acquire tokens lazily through the official SDK.
func New(cfg Config) (*Client, error) {
	cfg.AppID = strings.TrimSpace(cfg.AppID)
	cfg.AppSecret = strings.TrimSpace(cfg.AppSecret)
	if cfg.AppID == "" {
		return nil, errors.New("feishu: app id is required")
	}
	if cfg.AppSecret == "" {
		return nil, errors.New("feishu: app secret is required")
	}
	options := []lark.ClientOptionFunc{lark.WithEnableTokenCache(true)}
	if cfg.HTTPClient != nil {
		options = append(options, lark.WithHttpClient(cfg.HTTPClient))
	}
	if strings.TrimSpace(cfg.OpenBaseURL) != "" {
		options = append(options, lark.WithOpenBaseUrl(strings.TrimRight(strings.TrimSpace(cfg.OpenBaseURL), "/")))
	}
	sdk := lark.NewClient(cfg.AppID, cfg.AppSecret, options...)
	return &Client{
		sdk:        sdk,
		chatID:     strings.TrimSpace(cfg.ChatID),
		webBaseURL: strings.TrimRight(strings.TrimSpace(cfg.WebBaseURL), "/"),
		config:     cfg,
	}, nil
}

// NewFromIMConfig maps the repository's notify configuration to this adapter.
// It performs no network calls and is safe to use during dependency assembly.
func NewFromIMConfig(cfg config.IMConfig) (*Client, error) {
	return New(Config{
		AppID:             cfg.Feishu.AppID,
		AppSecret:         cfg.Feishu.AppSecret,
		VerificationToken: cfg.Feishu.VerificationToken,
		EncryptKey:        cfg.Feishu.EncryptKey,
		ChatID:            cfg.Feishu.ChatID,
		WebBaseURL:        cfg.Feishu.WebBaseURL,
	})
}

// NewClient is a convenience constructor for assembly code that starts with
// app credentials. New validates configuration and returns errors; callers
// that need explicit startup validation should use New directly.
func NewClient(appID, appSecret string, options ...ClientOption) *Client {
	cfg := Config{AppID: appID, AppSecret: appSecret}
	for _, option := range options {
		if option != nil {
			option(&cfg)
		}
	}
	client, err := New(cfg)
	if err != nil {
		return nil
	}
	return client
}

// NewNotifier constructs a Client through the common notification contract.
func NewNotifier(cfg Config) (notify.Notifier, error) {
	return New(cfg)
}

// SDK exposes the underlying official client for narrowly-scoped assembly
// integrations. It does not expose credentials.
func (c *Client) SDK() *lark.Client {
	if c == nil {
		return nil
	}
	return c.sdk
}

// Send renders and sends an interactive Card 2.0 to the configured chat.
func (c *Client) Send(ctx context.Context, n notify.Notification) (notify.Delivery, error) {
	if c == nil || c.sdk == nil {
		return notify.Delivery{}, errors.New("feishu: client is nil")
	}
	if strings.TrimSpace(c.chatID) == "" {
		return notify.Delivery{}, errors.New("feishu: chat id is required")
	}
	n = c.withWebURL(n)
	content, err := RenderCard(n)
	if err != nil {
		return notify.Delivery{}, err
	}
	return c.sendInteractive(ctx, c.chatID, content)
}

// Reply sends plain text in a reply thread. The parent message ID is supplied
// by the caller and is never accepted from card content.
func (c *Client) Reply(ctx context.Context, parentMessageID, content string) (notify.Delivery, error) {
	if strings.TrimSpace(content) == "" {
		return notify.Delivery{}, errors.New("feishu: reply content is required")
	}
	encoded, err := json.Marshal(map[string]string{"text": truncate(content, maxCardSummary)})
	if err != nil {
		return notify.Delivery{}, fmt.Errorf("feishu: marshal reply: %w", err)
	}
	return c.reply(ctx, parentMessageID, "text", string(encoded), true)
}

// ReplyCard sends an interactive Card 2.0 as a thread reply.
func (c *Client) ReplyCard(ctx context.Context, parentMessageID string, n notify.Notification) (notify.Delivery, error) {
	n = c.withWebURL(n)
	content, err := RenderCard(n)
	if err != nil {
		return notify.Delivery{}, err
	}
	return c.reply(ctx, parentMessageID, "interactive", string(content), true)
}

// Patch replaces an existing interactive message card. cardJSON must be a
// serialized Card 2.0 document; it is size checked before the SDK call.
func (c *Client) Patch(ctx context.Context, messageID, cardJSON string) error {
	if c == nil || c.sdk == nil {
		return errors.New("feishu: client is nil")
	}
	messageID = strings.TrimSpace(messageID)
	if messageID == "" {
		return errors.New("feishu: message id is required")
	}
	cardJSON = strings.TrimSpace(cardJSON)
	if cardJSON == "" {
		return errors.New("feishu: card content is required")
	}
	if len([]byte(cardJSON)) > MaxCardBytes {
		return fmt.Errorf("feishu: card exceeds %d bytes", MaxCardBytes)
	}
	patchBody := larkim.NewPatchMessageReqBodyBuilder().Content(cardJSON).Build()
	resp, err := c.sdk.Im.V1.Message.Patch(ctx, larkim.NewPatchMessageReqBuilder().
		MessageId(messageID).
		Body(patchBody).
		Build())
	if err != nil {
		return fmt.Errorf("feishu: patch request failed: %w", err)
	}
	if resp == nil {
		return errors.New("feishu: patch returned empty response")
	}
	if !resp.Success() {
		return apiError(resp.Code, resp.Msg, responseRequestID(resp.ApiResp))
	}
	return nil
}

// PatchNotification renders and replaces an existing card.
func (c *Client) PatchNotification(ctx context.Context, messageID string, n notify.Notification) error {
	n = c.withWebURL(n)
	content, err := RenderCard(n)
	if err != nil {
		return err
	}
	return c.Patch(ctx, messageID, string(content))
}

func (c *Client) sendInteractive(ctx context.Context, chatID string, content []byte) (notify.Delivery, error) {
	if len(content) > MaxCardBytes {
		return notify.Delivery{}, fmt.Errorf("feishu: card exceeds %d bytes", MaxCardBytes)
	}
	body := larkim.NewCreateMessageReqBodyBuilder().
		ReceiveId(chatID).
		MsgType("interactive").
		Content(string(content)).
		Build()
	resp, err := c.sdk.Im.V1.Message.Create(ctx, larkim.NewCreateMessageReqBuilder().
		ReceiveIdType("chat_id").
		Body(body).
		Build())
	if err != nil {
		return notify.Delivery{}, fmt.Errorf("feishu: create message request failed: %w", err)
	}
	if resp == nil {
		return notify.Delivery{}, errors.New("feishu: create message returned empty response")
	}
	if !resp.Success() {
		return notify.Delivery{}, apiError(resp.Code, resp.Msg, responseRequestID(resp.ApiResp))
	}
	if resp.Data == nil || resp.Data.MessageId == nil || strings.TrimSpace(*resp.Data.MessageId) == "" {
		return notify.Delivery{}, errors.New("feishu: create message response has no message id")
	}
	return notify.Delivery{Provider: "feishu_app", MessageID: *resp.Data.MessageId}, nil
}

func (c *Client) reply(ctx context.Context, parentMessageID, msgType, content string, inThread bool) (notify.Delivery, error) {
	if c == nil || c.sdk == nil {
		return notify.Delivery{}, errors.New("feishu: client is nil")
	}
	parentMessageID = strings.TrimSpace(parentMessageID)
	if parentMessageID == "" {
		return notify.Delivery{}, errors.New("feishu: parent message id is required")
	}
	if len([]byte(content)) > MaxCardBytes && msgType == "interactive" {
		return notify.Delivery{}, fmt.Errorf("feishu: card exceeds %d bytes", MaxCardBytes)
	}
	body, err := larkim.NewReplyMessagePathReqBodyBuilder().
		Content(content).
		MsgType(msgType).
		ReplyInThread(inThread).
		Build()
	if err != nil {
		return notify.Delivery{}, fmt.Errorf("feishu: build reply: %w", err)
	}
	resp, err := c.sdk.Im.V1.Message.Reply(ctx, larkim.NewReplyMessageReqBuilder().
		MessageId(parentMessageID).
		Body(body).
		Build())
	if err != nil {
		return notify.Delivery{}, fmt.Errorf("feishu: reply request failed: %w", err)
	}
	if resp == nil {
		return notify.Delivery{}, errors.New("feishu: reply returned empty response")
	}
	if !resp.Success() {
		return notify.Delivery{}, apiError(resp.Code, resp.Msg, responseRequestID(resp.ApiResp))
	}
	if resp.Data == nil || resp.Data.MessageId == nil || strings.TrimSpace(*resp.Data.MessageId) == "" {
		return notify.Delivery{}, errors.New("feishu: reply response has no message id")
	}
	return notify.Delivery{Provider: "feishu_app", MessageID: *resp.Data.MessageId}, nil
}

func (c *Client) withWebURL(n notify.Notification) notify.Notification {
	if c == nil || c.webBaseURL == "" || n.IncidentID == 0 {
		return n
	}
	if n.Payload == nil {
		n.Payload = make(map[string]any)
	}
	if payloadFirstText(n.Payload, "web_url", "webURL", "url") == "" {
		copyPayload := make(map[string]any, len(n.Payload)+1)
		for key, value := range n.Payload {
			copyPayload[key] = value
		}
		copyPayload["web_url"] = c.webBaseURL + "/incidents/" + url.PathEscape(fmt.Sprint(n.IncidentID))
		n.Payload = copyPayload
	}
	return n
}

// APIError contains only the fields Feishu documents for a business failure.
// It intentionally omits response bodies and request credentials.
type APIError struct {
	Code      int
	Message   string
	RequestID string
}

func (e APIError) Error() string {
	if e.RequestID == "" {
		return fmt.Sprintf("feishu: api error code=%d msg=%s", e.Code, e.Message)
	}
	return fmt.Sprintf("feishu: api error code=%d msg=%s request_id=%s", e.Code, e.Message, e.RequestID)
}

func apiError(code int, message, requestID string) error {
	return APIError{Code: code, Message: strings.TrimSpace(message), RequestID: strings.TrimSpace(requestID)}
}

func responseRequestID(resp interface{ RequestId() string }) string {
	if resp == nil {
		return ""
	}
	return resp.RequestId()
}

var _ notify.Notifier = (*Client)(nil)
