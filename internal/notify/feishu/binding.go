package feishu

import (
	"context"
	"log"
	"strings"
	"time"

	"oncall-agent/internal/eventlog"
	"oncall-agent/internal/notify"
	"oncall-agent/internal/store"
)

// NotificationBindingStore persists IM message identity after a successful send.
type NotificationBindingStore interface {
	CreateIMBinding(context.Context, store.IMBinding) (store.IMBinding, error)
	AppendIncidentEvent(context.Context, store.IncidentEvent) (store.IncidentEvent, error)
}

// BindingNotifier wraps a Feishu notifier and records message_id bindings.
// A binding write failure never rolls back the already-sent Feishu message.
type BindingNotifier struct {
	inner    notify.Notifier
	store    NotificationBindingStore
	provider string
	chatID   string
	now      func() time.Time
	logf     func(string, ...any)
}

func NewBindingNotifier(inner notify.Notifier, bindings NotificationBindingStore, provider, chatID string) notify.Notifier {
	if inner == nil {
		return notify.NoopNotifier{}
	}
	if bindings == nil {
		return inner
	}
	return &BindingNotifier{
		inner:    inner,
		store:    bindings,
		provider: strings.TrimSpace(provider),
		chatID:   strings.TrimSpace(chatID),
		now:      time.Now,
		logf:     log.Printf,
	}
}

func (n *BindingNotifier) Send(ctx context.Context, notification notify.Notification) (notify.Delivery, error) {
	delivery, err := n.inner.Send(ctx, notification)
	if err != nil {
		return delivery, err
	}
	if n == nil || n.store == nil || notification.IncidentID == 0 || strings.TrimSpace(delivery.MessageID) == "" {
		return delivery, nil
	}
	kind := "incident_card"
	switch notification.Kind {
	case notify.NotificationDiagnosisCompleted:
		kind = "diagnosis"
	case notify.NotificationApprovalRequired:
		kind = "approval"
	case notify.NotificationExecutionCompleted, notify.NotificationVerifyCompleted:
		kind = "execution"
	}
	_, bindErr := n.store.CreateIMBinding(ctx, store.IMBinding{
		Provider:      n.provider,
		ChatID:        n.chatID,
		MessageID:     delivery.MessageID,
		RootMessageID: stringPointer(delivery.MessageID),
		IncidentID:    notification.IncidentID,
		RunID:         notification.RunID,
		ApprovalID:    notification.ApprovalID,
		MessageKind:   kind,
		CreatedAt:     n.now().UTC(),
	})
	if bindErr != nil {
		n.logf("feishu: persist im binding failed: %v", bindErr)
		if notification.IncidentID != 0 {
			_, _ = n.store.AppendIncidentEvent(ctx, store.IncidentEvent{
				IncidentID: notification.IncidentID,
				RunID:      notification.RunID,
				ApprovalID: notification.ApprovalID,
				EventType:  string(eventlog.EventNotificationFailed),
				Phase:      "notify",
				Status:     "failed",
				Summary:    "im binding write failed",
				CreatedAt:  n.now().UTC(),
			})
		}
	}
	return delivery, nil
}

func stringPointer(value string) *string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return &value
}
