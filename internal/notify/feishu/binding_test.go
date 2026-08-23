package feishu

import (
	"context"
	"errors"
	"testing"

	"oncall-agent/internal/eventlog"
	"oncall-agent/internal/notify"
	"oncall-agent/internal/store"
)

type fakeNotifier struct {
	delivery notify.Delivery
	err      error
	sent     []notify.Notification
}

func (f *fakeNotifier) Send(_ context.Context, n notify.Notification) (notify.Delivery, error) {
	f.sent = append(f.sent, n)
	return f.delivery, f.err
}

type fakeBindingStore struct {
	created []store.IMBinding
	events  []store.IncidentEvent
	bindErr error
}

func (f *fakeBindingStore) CreateIMBinding(_ context.Context, binding store.IMBinding) (store.IMBinding, error) {
	if f.bindErr != nil {
		return store.IMBinding{}, f.bindErr
	}
	f.created = append(f.created, binding)
	return binding, nil
}

func (f *fakeBindingStore) AppendIncidentEvent(_ context.Context, event store.IncidentEvent) (store.IncidentEvent, error) {
	f.events = append(f.events, event)
	return event, nil
}

func TestBindingNotifierPersistsMessageID(t *testing.T) {
	inner := &fakeNotifier{delivery: notify.Delivery{Provider: "feishu_app", MessageID: "om_1"}}
	bindings := &fakeBindingStore{}
	notifier := NewBindingNotifier(inner, bindings, "feishu_app", "oc_chat")
	runID, approvalID := uint64(3), uint64(4)
	delivery, err := notifier.Send(context.Background(), notify.Notification{
		Kind: notify.NotificationApprovalRequired, IncidentID: 9, RunID: &runID, ApprovalID: &approvalID,
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if delivery.MessageID != "om_1" {
		t.Fatalf("message id = %q", delivery.MessageID)
	}
	if len(bindings.created) != 1 {
		t.Fatalf("bindings = %d", len(bindings.created))
	}
	got := bindings.created[0]
	if got.IncidentID != 9 || got.MessageKind != "approval" || got.MessageID != "om_1" || got.ChatID != "oc_chat" {
		t.Fatalf("binding = %+v", got)
	}
}

func TestBindingNotifierRecordsFailureWithoutRollback(t *testing.T) {
	inner := &fakeNotifier{delivery: notify.Delivery{MessageID: "om_2"}}
	bindings := &fakeBindingStore{bindErr: errors.New("duplicate")}
	notifier := NewBindingNotifier(inner, bindings, "feishu_app", "oc_chat")

	delivery, err := notifier.Send(context.Background(), notify.Notification{Kind: notify.NotificationIncidentFired, IncidentID: 2})
	if err != nil {
		t.Fatalf("Send rolled back: %v", err)
	}
	if delivery.MessageID != "om_2" {
		t.Fatalf("message id = %q", delivery.MessageID)
	}
	if len(bindings.events) != 1 || bindings.events[0].EventType != string(eventlog.EventNotificationFailed) {
		t.Fatalf("events = %+v", bindings.events)
	}
}
