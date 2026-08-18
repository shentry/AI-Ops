package ingest

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"oncall-agent/internal/config"
	"oncall-agent/internal/store"
)

const workerRetryInterval = time.Second

type pendingEventStore interface {
	NextPendingRawEvent(context.Context) (store.RawEvent, bool, error)
	ApplyRawEvent(context.Context, uint64, []store.AlertInput, time.Time) ([]store.DedupResult, error)
	MarkRawEventFailed(context.Context, uint64, string, time.Time) error
}

// Worker drains raw_event rows in ID order. The channel is only a wake-up
// signal; MySQL remains the durable queue and source of ordering.
type Worker struct {
	db            pendingEventStore
	cfg           config.IngestConfig
	wake          chan struct{}
	retryInterval time.Duration
	logger        *log.Logger
	done          chan struct{}
}

func NewWorker(db *store.DB, cfg config.IngestConfig, logger *log.Logger) *Worker {
	return newWorker(db, cfg, logger, workerRetryInterval)
}

func newWorker(db pendingEventStore, cfg config.IngestConfig, logger *log.Logger, retryInterval time.Duration) *Worker {
	if logger == nil {
		logger = log.Default()
	}
	if retryInterval <= 0 {
		retryInterval = workerRetryInterval
	}
	return &Worker{
		db: db, cfg: cfg, wake: make(chan struct{}, 1), retryInterval: retryInterval,
		logger: logger, done: make(chan struct{}),
	}
}

// Start verifies the pending queue is readable, starts the consumer, and
// schedules startup reconciliation before the HTTP listener is opened.
func (w *Worker) Start(ctx context.Context) error {
	if _, _, err := w.db.NextPendingRawEvent(ctx); err != nil {
		return err
	}
	go w.consume(ctx)
	w.Notify()
	return nil
}

// Notify wakes the worker after a durable raw_event insert. Multiple wake-ups
// coalesce because each drain reads MySQL until no pending row remains.
func (w *Worker) Notify() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *Worker) Wait() { <-w.done }

func (w *Worker) consume(ctx context.Context) {
	defer close(w.done)
	ticker := time.NewTicker(w.retryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.wake:
		case <-ticker.C:
		}
		if err := w.drain(ctx); err != nil && ctx.Err() == nil {
			w.logger.Printf("ingest: pending reconciliation failed: %v", err)
		}
	}
}

func (w *Worker) drain(ctx context.Context) error {
	for {
		event, found, err := w.db.NextPendingRawEvent(ctx)
		if err != nil {
			return err
		}
		if !found {
			return nil
		}
		if err := w.process(ctx, event); err != nil {
			return err
		}
	}
}

func (w *Worker) process(ctx context.Context, event store.RawEvent) error {
	alerts, err := ParseWebhook(event.Payload)
	if err != nil {
		return w.reject(ctx, event.ID, err)
	}
	receivedAt := event.CreatedAt.UTC()
	inputs := make([]store.AlertInput, 0, len(alerts))
	for index := range alerts {
		alert := &alerts[index]
		if strings.TrimSpace(alert.Name) == "" {
			return w.reject(ctx, event.ID, fmt.Errorf("ingest: alerts[%d].labels.alertname is required", index))
		}
		if alert.StartsAt.IsZero() {
			return w.reject(ctx, event.ID, fmt.Errorf("ingest: alerts[%d].startsAt is required", index))
		}
		if len(w.cfg.FingerprintFields) > 0 && len(fingerprintKeys(alert.Labels, w.cfg.FingerprintFields)) == 0 {
			return w.reject(ctx, event.ID, fmt.Errorf("ingest: alerts[%d] has none of the configured fingerprint fields", index))
		}
		alert.ReceivedAt = receivedAt
		alert.Fingerprint = Fingerprint(alert.Labels, w.cfg.FingerprintFields)
		alert.Severity = Severity(alert.Labels, w.cfg.SeverityLabel)
		alert.AlertHash = FullHash(*alert)
		inputs = append(inputs, store.AlertInput{
			Fingerprint: alert.Fingerprint, AlertHash: alert.AlertHash, Source: alert.Source,
			Name: alert.Name, Severity: alert.Severity, Status: alert.Status,
			Labels: alert.Labels, Annotations: alert.Annotations, GeneratorURL: alert.GeneratorURL,
			StartsAt: alert.StartsAt, ReceivedAt: alert.ReceivedAt,
		})
	}
	results, err := w.db.ApplyRawEvent(ctx, event.ID, inputs, time.Now().UTC())
	if err != nil {
		return err
	}
	for index, result := range results {
		if result == store.DedupFull {
			w.logger.Printf("ingest: raw event %d alert %d full duplicate", event.ID, index)
		}
	}
	return nil
}

func (w *Worker) reject(ctx context.Context, id uint64, cause error) error {
	message := safeEventError(cause)
	if err := w.db.MarkRawEventFailed(ctx, id, message, time.Now().UTC()); err != nil {
		return fmt.Errorf("%s; mark failed: %w", message, err)
	}
	w.logger.Printf("ingest: raw event %d rejected: %s", id, message)
	return nil
}

func safeEventError(err error) string {
	message := strings.NewReplacer("\r", " ", "\n", " ").Replace(err.Error())
	runes := []rune(message)
	if len(runes) > 2048 {
		message = string(runes[:2048])
	}
	return message
}
