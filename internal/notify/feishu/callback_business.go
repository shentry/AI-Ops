package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	"gorm.io/datatypes"
	"gorm.io/gorm"

	"oncall-agent/internal/conversation"
	"oncall-agent/internal/eventlog"
	"oncall-agent/internal/notify"
	"oncall-agent/internal/store"
)

const (
	defaultFeishuProvider      = "feishu_app"
	defaultCallbackEventType   = "card.action.trigger"
	defaultMessageEventType    = "im.message.receive_v1"
	fixedIncidentPrompt        = "请回复一张 Incident 卡片，或输入 “incident #123 你的问题”。"
	approvalActionRequestProof = "request_evidence"
)

var (
	ErrCallbackDependency   = errors.New("feishu: callback dependency is not configured")
	ErrCallbackEventID      = errors.New("feishu: callback event_id is required")
	ErrCallbackOperator     = errors.New("feishu: callback operator is not allowed")
	ErrCallbackAction       = errors.New("feishu: callback action is invalid")
	ErrCallbackPlanMismatch = errors.New("feishu: callback approval plan does not match")
	ErrCallbackChat         = errors.New("feishu: callback chat is not configured")
	ErrCallbackBinding      = errors.New("feishu: callback message is not bound")
)

// ReceiptStore is the minimal callback de-duplication surface. The concrete
// *store.DB implementation uses integration_event_receipt's primary key as the
// cross-process CAS boundary. Finalization is optional for small test fakes;
// production always provides it.
type ReceiptStore interface {
	ClaimIntegrationEventReceipt(context.Context, store.IntegrationEventReceipt) (bool, error)
}

type receiptFinalizer interface {
	CompleteIntegrationEventReceipt(context.Context, string, string, time.Time) error
}

type receiptAbandoner interface {
	DeleteIntegrationEventReceipt(context.Context, string) error
}

// BindingStore persists and resolves Feishu message/thread-to-Incident links.
// FindIMBinding must enforce provider and chat equality; callers must not fall
// back to a global message search.
type BindingStore interface {
	CreateIMBinding(context.Context, store.IMBinding) (store.IMBinding, error)
}

type bindingFinder interface {
	FindIMBinding(context.Context, string, string, string, string, string) (store.IMBinding, error)
}

type bindingGetter interface {
	GetIMBinding(context.Context, string, string) (store.IMBinding, error)
}

// CallbackEventWriter is used only for durable notification.failed facts when
// an already-committed approval cannot be reflected back into Feishu. It must
// never be used to roll back the approval decision.
type CallbackEventWriter interface {
	AppendIncidentEvent(context.Context, store.IncidentEvent) (store.IncidentEvent, error)
}

// ApprovalDecider is intentionally the same contract implemented by
// approval.Service. Both Web and Feishu therefore use the store CAS and retain
// one audit trail. Get is required so the card's immutable plan_hash is checked
// against a fresh database row before Decide.
type ApprovalDecider interface {
	Get(context.Context, uint64) (store.Approval, error)
	Decide(context.Context, uint64, bool, string, string, string) (store.Approval, error)
}

// ConversationAsker is the shared Web/Feishu conversation entrypoint. Its
// implementation only queues a question; it never performs LLM work here.
type ConversationAsker interface {
	Ask(context.Context, uint64, conversation.Actor, string, string) (store.ConversationMessage, error)
}

// MessageClient is the narrow outbound surface required by callback business
// logic. *Client satisfies it, while tests can inject a no-network fake.
type MessageClient interface {
	Reply(context.Context, string, string) (notify.Delivery, error)
	Patch(context.Context, string, string) error
}

// BusinessDependencies contains callback business dependencies. Store is the
// preferred aggregate field; DB is an explicit alias for assembly code that
// names its persistence dependency db.
type BusinessDependencies struct {
	Store any
	DB    any

	Receipts          ReceiptStore
	Bindings          BindingStore
	Events            CallbackEventWriter
	Approval          ApprovalDecider
	Conversation      ConversationAsker
	Client            MessageClient
	ChatID            string
	Provider          string
	OperatorAllowlist []string
}

type CallbackBusiness struct {
	receipts     ReceiptStore
	finalizer    receiptFinalizer
	abandoner    receiptAbandoner
	bindings     BindingStore
	finder       bindingFinder
	getter       bindingGetter
	events       CallbackEventWriter
	approval     ApprovalDecider
	conversation ConversationAsker
	client       MessageClient
	chatID       string
	provider     string
	allowlist    map[string]struct{}
}

// NewCallbackBusiness builds authenticated-event business handlers. It makes
// no network requests and is safe to use in tests or during server assembly.
func NewCallbackBusiness(deps BusinessDependencies) *CallbackBusiness {
	b := &CallbackBusiness{
		chatID:       strings.TrimSpace(deps.ChatID),
		provider:     strings.TrimSpace(deps.Provider),
		approval:     deps.Approval,
		conversation: deps.Conversation,
		client:       deps.Client,
	}
	if b.provider == "" {
		b.provider = defaultFeishuProvider
	}
	b.allowlist = make(map[string]struct{}, len(deps.OperatorAllowlist))
	for _, id := range deps.OperatorAllowlist {
		id = strings.TrimSpace(id)
		if id != "" {
			b.allowlist[id] = struct{}{}
			if strings.HasPrefix(id, "feishu:") {
				b.allowlist[strings.TrimPrefix(id, "feishu:")] = struct{}{}
			}
		}
	}

	// Explicit interfaces win. This lets a test fake expose only the methods it
	// needs while *store.DB can still be passed once through Store.
	b.receipts = deps.Receipts
	b.bindings = deps.Bindings
	b.events = deps.Events
	persistence := deps.Store
	if persistence == nil {
		persistence = deps.DB
	}
	if persistence != nil {
		if b.receipts == nil {
			b.receipts, _ = persistence.(ReceiptStore)
		}
		if b.bindings == nil {
			b.bindings, _ = persistence.(BindingStore)
		}
		if b.events == nil {
			b.events, _ = persistence.(CallbackEventWriter)
		}
		b.finalizer, _ = persistence.(receiptFinalizer)
		b.abandoner, _ = persistence.(receiptAbandoner)
		b.finder, _ = persistence.(bindingFinder)
		b.getter, _ = persistence.(bindingGetter)
	}
	if candidate, ok := deps.Receipts.(receiptFinalizer); ok {
		b.finalizer = candidate
	}
	if candidate, ok := deps.Receipts.(receiptAbandoner); ok {
		b.abandoner = candidate
	}
	if candidate, ok := deps.Bindings.(bindingFinder); ok {
		b.finder = candidate
	}
	if candidate, ok := deps.Bindings.(bindingGetter); ok {
		b.getter = candidate
	}
	return b
}

// NewBusinessHandlers creates the two callbacks consumed by NewDispatcher.
func NewBusinessHandlers(deps BusinessDependencies) CallbackHandlers {
	business := NewCallbackBusiness(deps)
	return CallbackHandlers{CardAction: business.CardAction, MessageReceive: business.MessageReceive}
}

// NewCallbackHandlers is a descriptive alias used by route assembly.
func NewCallbackHandlers(deps BusinessDependencies) CallbackHandlers {
	return NewBusinessHandlers(deps)
}

// NewFeishuCallbackBusiness is an explicit constructor alias for integrations.
func NewFeishuCallbackBusiness(deps BusinessDependencies) *CallbackBusiness {
	return NewCallbackBusiness(deps)
}

// CardAction is the callback registered for card.action.trigger. It does only
// bounded DB reads/CAS and returns a toast; card patching is always dispatched
// asynchronously after the decision commits.
func (b *CallbackBusiness) CardAction(ctx context.Context, event *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
	if b == nil || b.receipts == nil || b.approval == nil {
		return nil, ErrCallbackDependency
	}
	if ctx == nil {
		ctx = context.Background()
	}
	eventID := cardEventID(event)
	if eventID == "" {
		return nil, ErrCallbackEventID
	}
	claimed, err := b.claimReceipt(ctx, eventID, eventType(event, defaultCallbackEventType))
	if err != nil {
		return nil, err
	}
	if !claimed {
		return toast("success", "该操作已处理"), nil
	}
	finish := func(result string) { b.finishReceipt(ctx, eventID, result) }

	operator := cardOperator(event)
	if !b.allowed(operator) {
		finish("forbidden")
		return toast("error", "你没有审批权限"), nil
	}
	action, approvalID, planHash, err := cardActionValue(event)
	if err != nil {
		finish("invalid")
		return toast("error", "审批操作无效"), nil
	}
	approvalRow, err := b.approval.Get(ctx, approvalID)
	if err != nil {
		finish("invalid")
		return toast("error", "审批不存在"), nil
	}
	if approvalRow.PlanHash != planHash {
		finish("invalid")
		return toast("error", "审批计划已变更，请刷新卡片"), nil
	}
	if approvalRow.Status != "pending" {
		finish("conflict")
		return toast("success", "该审批已处理"), nil
	}

	if action == approvalActionRequestProof {
		finish("invalid")
		return toast("error", "请在 Web 控制室请求补充证据"), nil
	}
	if action != "approve" && action != "deny" {
		finish("invalid")
		return toast("error", "审批操作无效"), nil
	}
	approve := action == "approve"
	decidedBy := "feishu:" + operator
	decided, err := b.approval.Decide(ctx, approvalID, approve, decidedBy, "", "feishu")
	if err != nil {
		if errors.Is(err, store.ErrApprovalConflict) {
			finish("conflict")
			return toast("success", "该审批已处理"), nil
		}
		b.abandonReceipt(ctx, eventID)
		finish("failed")
		return nil, err
	}
	finish(decided.Status)
	b.patchDecisionAsync(decided, cardMessageID(event), approve)
	if approve {
		return toast("success", "审批已批准"), nil
	}
	return toast("success", "审批已拒绝"), nil
}

// HandleCardAction is an explicit method name for adapters and tests.
func (b *CallbackBusiness) HandleCardAction(ctx context.Context, event *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
	return b.CardAction(ctx, event)
}

// MessageReceive handles im.message.receive_v1. It accepts only messages in
// the configured chat that are explicitly incident-bound or belong to a known
// message/thread binding. It queues a question and returns without LLM work.
func (b *CallbackBusiness) MessageReceive(ctx context.Context, event *larkim.P2MessageReceiveV1) error {
	if b == nil || b.receipts == nil || b.bindings == nil || b.conversation == nil {
		return ErrCallbackDependency
	}
	if ctx == nil {
		ctx = context.Background()
	}
	message := messageFromEvent(event)
	if message == nil {
		return errors.New("feishu: message payload is required")
	}
	eventID := messageEventID(event, message)
	if eventID == "" {
		return ErrCallbackEventID
	}
	claimed, err := b.claimReceipt(ctx, eventID, eventType(event, defaultMessageEventType))
	if err != nil {
		return err
	}
	if !claimed {
		return nil
	}
	finish := func(result string) { b.finishReceipt(ctx, eventID, result) }

	chatID := stringValue(message.ChatId)
	if b.chatID == "" || chatID != b.chatID {
		finish("ignored")
		return nil
	}
	if event != nil && event.Event != nil && event.Event.Sender != nil && event.Event.Sender.SenderType != nil && strings.EqualFold(strings.TrimSpace(*event.Event.Sender.SenderType), "bot") {
		finish("ignored")
		return nil
	}
	messageID := stringValue(message.MessageId)
	if messageID == "" {
		finish("invalid")
		return nil
	}
	rootID := stringValue(message.RootId)
	threadID := stringValue(message.ThreadId)
	binding, bindErr := b.findBinding(ctx, chatID, messageID, rootID, threadID)
	bound := bindErr == nil
	if bindErr != nil && !errors.Is(bindErr, gorm.ErrRecordNotFound) && !errors.Is(bindErr, ErrCallbackBinding) {
		b.abandonReceipt(ctx, eventID)
		finish("failed")
		return bindErr
	}

	text := messageText(message.Content)
	explicitIncident, question, explicit := parseIncidentQuestion(text)
	incidentID := uint64(0)
	if bound {
		incidentID = binding.IncidentID
		if incidentID == 0 {
			finish("invalid")
			return nil
		}
		if explicit {
			if explicitIncident != incidentID {
				b.replyFixed(ctx, messageID)
				finish("rejected")
				return nil
			}
		} else {
			question = text
		}
	} else if explicit {
		incidentID = explicitIncident
	} else {
		b.replyFixed(ctx, messageID)
		finish("ignored")
		return nil
	}
	if strings.TrimSpace(question) == "" {
		b.replyFixed(ctx, messageID)
		finish("invalid")
		return nil
	}
	actorID := messageActorID(event)
	if actorID == "" {
		finish("invalid")
		return nil
	}
	// Bind every inbound question before queueing. Replies to an existing card
	// also need their own message identity so the worker cannot answer another thread.
	if err := b.createQuestionBinding(ctx, chatID, message, incidentID); err != nil {
		b.recordNotificationFailure(ctx, incidentID, nil, "feishu message binding failed")
		finish("failed")
		return nil
	}
	_, askErr := b.conversation.Ask(ctx, incidentID, conversation.Actor{ID: actorID, Source: "feishu", SourceMessageID: messageID}, question, "feishu")
	if askErr != nil {
		// Ask may have persisted a queued row before reporting an audit-event failure.
		b.replyText(ctx, messageID, "问题暂时无法入队，请稍后重试。")
		finish("failed")
		return nil
	}
	finish("queued")
	return nil
}

// HandleMessageReceive is an explicit method name for adapters and tests.
func (b *CallbackBusiness) HandleMessageReceive(ctx context.Context, event *larkim.P2MessageReceiveV1) error {
	return b.MessageReceive(ctx, event)
}

func (b *CallbackBusiness) claimReceipt(ctx context.Context, eventID, eventType string) (bool, error) {
	receipt := store.IntegrationEventReceipt{
		EventID: eventID, Provider: b.provider, EventType: eventType,
		ProcessedAt: time.Now().UTC(), Result: "processing",
	}
	return b.receipts.ClaimIntegrationEventReceipt(ctx, receipt)
}

func (b *CallbackBusiness) finishReceipt(ctx context.Context, eventID, result string) {
	if b == nil || b.finalizer == nil {
		return
	}
	_ = b.finalizer.CompleteIntegrationEventReceipt(ctx, eventID, result, time.Now().UTC())
}
func (b *CallbackBusiness) abandonReceipt(ctx context.Context, eventID string) {
	if b == nil || b.abandoner == nil {
		return
	}
	_ = b.abandoner.DeleteIntegrationEventReceipt(ctx, eventID)
}

func (b *CallbackBusiness) allowed(openID string) bool {
	if strings.TrimSpace(openID) == "" || len(b.allowlist) == 0 {
		return false
	}
	_, ok := b.allowlist[openID]
	return ok
}

func (b *CallbackBusiness) findBinding(ctx context.Context, chatID, messageID, rootID, threadID string) (store.IMBinding, error) {
	if b.finder != nil {
		return b.finder.FindIMBinding(ctx, b.provider, chatID, messageID, rootID, threadID)
	}
	if b.getter != nil && messageID != "" {
		return b.getter.GetIMBinding(ctx, b.provider, messageID)
	}
	return store.IMBinding{}, ErrCallbackBinding
}
func (b *CallbackBusiness) createQuestionBinding(ctx context.Context, chatID string, message *larkim.EventMessage, incidentID uint64) error {
	messageID := stringValue(message.MessageId)
	binding := store.IMBinding{
		Provider:      b.provider,
		ChatID:        chatID,
		MessageID:     messageID,
		RootMessageID: stringPtr(messageID),
		IncidentID:    incidentID,
		MessageKind:   "conversation_question",
		CreatedAt:     time.Now().UTC(),
	}
	if root := stringValue(message.RootId); root != "" {
		binding.RootMessageID = stringPtr(root)
	}
	if thread := stringValue(message.ThreadId); thread != "" {
		binding.ThreadID = stringPtr(thread)
	}
	_, err := b.bindings.CreateIMBinding(ctx, binding)
	return err
}

func (b *CallbackBusiness) replyFixed(ctx context.Context, messageID string) {
	b.replyText(ctx, messageID, fixedIncidentPrompt)
}

func (b *CallbackBusiness) replyText(ctx context.Context, messageID, text string) {
	if b == nil || b.client == nil || strings.TrimSpace(messageID) == "" {
		return
	}
	if _, err := b.client.Reply(ctx, messageID, text); err != nil {
		b.recordNotificationFailure(ctx, 0, nil, "feishu message reply failed")
	}
}

func (b *CallbackBusiness) patchDecisionAsync(approvalRow store.Approval, messageID string, approve bool) {
	if b == nil || b.client == nil || strings.TrimSpace(messageID) == "" {
		return
	}
	status := "denied"
	summary := "审批已拒绝"
	if approve {
		status = "approved"
		summary = "审批已批准"
	}
	incidentID, runID := approvalRow.IncidentID, approvalRow.RunID
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		content, err := RenderCard(notify.Notification{
			Kind:       notify.NotificationApprovalDecided,
			IncidentID: incidentID,
			RunID:      &runID,
			Summary:    summary,
			Payload:    map[string]any{"approval_status": status},
		})
		if err == nil {
			err = b.client.Patch(ctx, messageID, string(content))
		}
		if err != nil {
			failureCtx, cancelFailure := context.WithTimeout(context.Background(), 2*time.Second)
			b.recordNotificationFailure(failureCtx, incidentID, &runID, "feishu card patch failed")
			cancelFailure()
		}
	}()
}

func (b *CallbackBusiness) recordNotificationFailure(ctx context.Context, incidentID uint64, runID *uint64, summary string) {
	if b == nil || b.events == nil || incidentID == 0 {
		return
	}
	payload := datatypes.JSON([]byte(`{"provider":"feishu_app"}`))
	_, _ = b.events.AppendIncidentEvent(ctx, store.IncidentEvent{
		IncidentID: incidentID, RunID: runID,
		EventType: string(eventlog.EventNotificationFailed),
		Phase:     "notification", Status: "failed", Summary: summary,
		PayloadJSON: &payload, CreatedAt: time.Now().UTC(),
	})
}

func toast(kind, content string) *callback.CardActionTriggerResponse {
	return &callback.CardActionTriggerResponse{Toast: &callback.Toast{Type: kind, Content: content}}
}

func eventType(event any, fallback string) string {
	switch typed := event.(type) {
	case *callback.CardActionTriggerEvent:
		if typed != nil && typed.EventV2Base != nil && typed.EventV2Base.Header != nil && strings.TrimSpace(typed.EventV2Base.Header.EventType) != "" {
			return strings.TrimSpace(typed.EventV2Base.Header.EventType)
		}
	case *larkim.P2MessageReceiveV1:
		if typed != nil && typed.EventV2Base != nil && typed.EventV2Base.Header != nil && strings.TrimSpace(typed.EventV2Base.Header.EventType) != "" {
			return strings.TrimSpace(typed.EventV2Base.Header.EventType)
		}
	}
	return fallback
}

func cardEventID(event *callback.CardActionTriggerEvent) string {
	if event == nil || event.EventV2Base == nil || event.EventV2Base.Header == nil {
		return ""
	}
	return strings.TrimSpace(event.EventV2Base.Header.EventID)
}

func messageEventID(event *larkim.P2MessageReceiveV1, message *larkim.EventMessage) string {
	if event != nil && event.EventV2Base != nil && event.EventV2Base.Header != nil && strings.TrimSpace(event.EventV2Base.Header.EventID) != "" {
		return strings.TrimSpace(event.EventV2Base.Header.EventID)
	}
	// Message IDs are globally unique and make manually constructed test events
	// useful without weakening the v2 event-id path in production.
	if message == nil {
		return ""
	}
	return stringValue(message.MessageId)
}

func cardOperator(event *callback.CardActionTriggerEvent) string {
	if event == nil || event.Event == nil || event.Event.Operator == nil {
		return ""
	}
	return strings.TrimSpace(event.Event.Operator.OpenID)
}

func cardActionValue(event *callback.CardActionTriggerEvent) (string, uint64, string, error) {
	if event == nil || event.Event == nil || event.Event.Action == nil || event.Event.Action.Value == nil {
		return "", 0, "", ErrCallbackAction
	}
	value := event.Event.Action.Value
	action, ok := value["action"].(string)
	if !ok {
		return "", 0, "", ErrCallbackAction
	}
	action = strings.TrimSpace(action)
	if action != "approve" && action != "deny" && action != approvalActionRequestProof {
		return "", 0, "", ErrCallbackAction
	}
	planHash, ok := value["plan_hash"].(string)
	if !ok || strings.TrimSpace(planHash) == "" {
		return "", 0, "", ErrCallbackAction
	}
	approvalID, ok := uint64Value(value["approval_id"])
	if !ok || approvalID == 0 {
		return "", 0, "", ErrCallbackAction
	}
	return action, approvalID, strings.TrimSpace(planHash), nil
}

func uint64Value(value any) (uint64, bool) {
	switch typed := value.(type) {
	case uint64:
		return typed, true
	case uint:
		return uint64(typed), true
	case uint32:
		return uint64(typed), true
	case int:
		if typed < 0 {
			return 0, false
		}
		return uint64(typed), true
	case int64:
		if typed < 0 {
			return 0, false
		}
		return uint64(typed), true
	case float64:
		if typed < 0 || typed != float64(uint64(typed)) {
			return 0, false
		}
		return uint64(typed), true
	case json.Number:
		parsed, err := strconv.ParseUint(string(typed), 10, 64)
		return parsed, err == nil
	case string:
		parsed, err := strconv.ParseUint(strings.TrimSpace(typed), 10, 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}

func cardMessageID(event *callback.CardActionTriggerEvent) string {
	if event == nil || event.Event == nil || event.Event.Context == nil {
		return ""
	}
	return strings.TrimSpace(event.Event.Context.OpenMessageID)
}

func messageFromEvent(event *larkim.P2MessageReceiveV1) *larkim.EventMessage {
	if event == nil || event.Event == nil {
		return nil
	}
	return event.Event.Message
}

func messageActorID(event *larkim.P2MessageReceiveV1) string {
	if event == nil || event.Event == nil || event.Event.Sender == nil || event.Event.Sender.SenderId == nil {
		return ""
	}
	if event.Event.Sender.SenderId.OpenId != nil {
		return strings.TrimSpace(*event.Event.Sender.SenderId.OpenId)
	}
	if event.Event.Sender.SenderId.UserId != nil {
		return strings.TrimSpace(*event.Event.Sender.SenderId.UserId)
	}
	return ""
}

func messageText(content *string) string {
	if content == nil {
		return ""
	}
	raw := strings.TrimSpace(*content)
	if raw == "" {
		return ""
	}
	var body struct {
		Text string `json:"text"`
	}
	if json.Unmarshal([]byte(raw), &body) == nil && strings.TrimSpace(body.Text) != "" {
		return strings.TrimSpace(body.Text)
	}
	return raw
}

var incidentQuestionPattern = regexp.MustCompile(`(?is)^\s*incident\s*#([0-9]+)\s+(.+?)\s*$`)

func parseIncidentQuestion(text string) (uint64, string, bool) {
	matches := incidentQuestionPattern.FindStringSubmatch(strings.TrimSpace(text))
	if len(matches) != 3 {
		return 0, "", false
	}
	id, err := strconv.ParseUint(matches[1], 10, 64)
	if err != nil || id == 0 {
		return 0, "", false
	}
	return id, strings.TrimSpace(matches[2]), true
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}

func stringPtr(value string) *string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return &value
}

func (b *CallbackBusiness) String() string {
	if b == nil {
		return "feishu callback business <nil>"
	}
	return fmt.Sprintf("feishu callback business provider=%s chat=%s", b.provider, b.chatID)
}

var _ ReceiptStore = (*store.DB)(nil)
var _ BindingStore = (*store.DB)(nil)
var _ CallbackEventWriter = (*store.DB)(nil)
var _ ConversationAsker = (conversation.Service)(nil)
var _ MessageClient = (*Client)(nil)
