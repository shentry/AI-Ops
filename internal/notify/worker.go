package notify

import (
	"context"
	"fmt"
	"log"
	"time"

	"oncall-agent/internal/metrics"
	"oncall-agent/internal/store"
	"oncall-agent/internal/tools"
)

type notificationStore interface {
	NextNotification(context.Context, time.Time) (store.NotificationTask, bool, error)
	FinishNotification(context.Context, store.NotificationTask, time.Time, string) error
}

// Worker delivers durable human-attention events under the server's single-process
// execution lock. A restart retries undelivered events, never their side effects.
type Worker struct {
	db       notificationStore
	notifier Notifier
	done     chan struct{}
}

func NewWorker(db notificationStore, notifier Notifier) *Worker {
	return &Worker{db: db, notifier: notifier, done: make(chan struct{})}
}

func (w *Worker) Start(ctx context.Context) error {
	if _, _, err := w.db.NextNotification(ctx, time.Now().UTC()); err != nil {
		return err
	}
	go func() {
		defer close(w.done)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			if err := w.RunOnce(ctx); err != nil && ctx.Err() == nil {
				log.Printf("notification worker: %v", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return nil
}

func (w *Worker) Wait() { <-w.done }

func (w *Worker) RunOnce(ctx context.Context) error {
	task, found, err := w.db.NextNotification(ctx, time.Now().UTC())
	if err != nil || !found {
		return err
	}
	event := task.Event
	sendCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	_, sendErr := w.notifier.Send(sendCtx, Notification{Kind: NotificationEscalationRequired, IncidentID: event.IncidentID,
		RunID: event.RunID, ApprovalID: event.ApprovalID, Title: "需要人工处理", Summary: fmt.Sprintf("事件 #%d · %s：%s", event.ID, event.EventType, tools.Sanitize(event.Summary))})
	cancel()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	failure := ""
	if sendErr != nil {
		failure = tools.Sanitize(sendErr.Error())
		metrics.Inc(metrics.NotificationFailed)
	} else {
		metrics.Inc(metrics.EscalationSent)
	}
	return w.db.FinishNotification(ctx, task, time.Now().UTC(), failure)
}
