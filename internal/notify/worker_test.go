package notify

import (
	"context"
	"errors"
	"testing"
	"time"

	"oncall-agent/internal/store"
)

type deliveryStore struct {
	task      store.NotificationTask
	failures  []string
	delivered bool
	commitErr error
}

func (s *deliveryStore) NextNotification(context.Context, time.Time) (store.NotificationTask, bool, error) {
	return s.task, !s.delivered, nil
}
func (s *deliveryStore) FinishNotification(_ context.Context, _ store.NotificationTask, _ time.Time, failure string) error {
	if s.commitErr != nil {
		return s.commitErr
	}
	s.failures = append(s.failures, failure)
	s.delivered = failure == ""
	return nil
}

type deliveryNotifier struct {
	fail  bool
	calls []Notification
}

func (n *deliveryNotifier) Send(_ context.Context, message Notification) (Delivery, error) {
	n.calls = append(n.calls, message)
	if n.fail {
		return Delivery{}, errors.New("network failed")
	}
	return Delivery{Provider: "test"}, nil
}
func TestNotificationWorkerRetriesDeliveryWithoutLosingCommittedEvent(t *testing.T) {
	db := &deliveryStore{task: store.NotificationTask{EventID: 42, Event: store.IncidentEvent{ID: 42, IncidentID: 7, EventType: "execution.failed", Summary: "outcome unknown"}}}
	n := &deliveryNotifier{fail: true}
	w := NewWorker(db, n)
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if db.delivered || len(db.failures) != 1 || db.failures[0] == "" {
		t.Fatalf("failure not retained: %+v", db)
	}
	n.fail = false
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !db.delivered || len(n.calls) != 2 || n.calls[1].IncidentID != 7 {
		t.Fatalf("retry=%+v", db)
	}
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(n.calls) != 2 {
		t.Fatal("delivered event sent twice")
	}
}
