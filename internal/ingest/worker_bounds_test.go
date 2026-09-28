package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"oncall-agent/internal/config"
	"oncall-agent/internal/store"
)

func boundedTestWorker(db pendingEventStore, groupBy []string) *Worker {
	return newWorker(db, config.IngestConfig{}, config.CorrelateConfig{GroupBy: groupBy, WindowMinutes: 15, MinAlerts: 1}, nil, log.New(io.Discard, "", 0), time.Second)
}

func mutateWebhookAlert(t *testing.T, event store.RawEvent, mutate func(map[string]any)) store.RawEvent {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	mutate(payload["alerts"].([]any)[0].(map[string]any))
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	event.Payload = data
	return event
}

func TestWorkerIsolatesSchemaPoisonAndDrainsFollowingEvent(t *testing.T) {
	for _, test := range []struct {
		name    string
		mutate  func(map[string]any)
		groupBy []string
	}{
		{"name 256", func(a map[string]any) { a["labels"].(map[string]any)["alertname"] = strings.Repeat("n", 256) }, nil},
		{"unicode name 256", func(a map[string]any) { a["labels"].(map[string]any)["alertname"] = strings.Repeat("警", 256) }, nil},
		{"fallback group 256", func(a map[string]any) { a["labels"].(map[string]any)["alertname"] = strings.Repeat("n", 251) }, nil},
		{"configured group 256", func(a map[string]any) { a["labels"].(map[string]any)["service"] = strings.Repeat("s", 256) }, []string{"service"}},
		{"joined group 256", func(a map[string]any) {
			a["labels"].(map[string]any)["service"] = strings.Repeat("s", 250)
			a["labels"].(map[string]any)["env"] = "stage"
		}, []string{"service", "env"}},
		{"url text bytes", func(a map[string]any) { a["generatorURL"] = strings.Repeat("警", 21846) }, nil},
		{"early time", func(a map[string]any) { a["startsAt"] = "0999-12-31T23:59:59Z" }, nil},
		{"utc year overflow", func(a map[string]any) { a["startsAt"] = "9999-12-31T23:30:00-01:00" }, nil},
		{"datetime rounding overflow", func(a map[string]any) { a["startsAt"] = "9999-12-31T23:59:59.999999999Z" }, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Now().UTC()
			bad := mutateWebhookAlert(t, testRawEvent(1, now, "BadAlert", nil), test.mutate)
			good := testRawEvent(2, now, "GoodAlert", nil)
			db := newFakePendingEventStore(bad, good)
			if err := boundedTestWorker(db, test.groupBy).drain(context.Background()); err != nil {
				t.Fatal(err)
			}
			if db.failedMessage(1) == "" || len(db.applied) != 1 || db.applied[0].id != 2 || db.attemptCount() != 1 {
				t.Fatalf("poison=%q applied=%v attempts=%d", db.failedMessage(1), db.applied, db.attemptCount())
			}
		})
	}
}

func TestWorkerAcceptsSchemaBoundariesAndResolvedWithoutGrouping(t *testing.T) {
	for _, test := range []struct {
		name      string
		resolved  bool
		groupBy   []string
		alertName string
	}{
		{"fallback group boundary", false, nil, strings.Repeat("警", 250)},
		{"name boundary", false, []string{"service"}, strings.Repeat("警", 255)},
		{"resolved needs no new group", true, nil, strings.Repeat("警", 255)},
	} {
		t.Run(test.name, func(t *testing.T) {
			event := testRawEvent(1, time.Now().UTC(), test.alertName, map[string]string{"service": strings.Repeat("服", 255)})
			event = mutateWebhookAlert(t, event, func(a map[string]any) {
				a["generatorURL"] = strings.Repeat("警", 21845)
				a["startsAt"] = "1000-01-01T00:00:00Z"
				// EndsAt is not persisted, and zero is normal for firing alerts.
				if test.resolved {
					a["status"] = "resolved"
				}
			})
			db := newFakePendingEventStore(event)
			incidentID := uint64(42)
			db.incidentID = &incidentID
			if err := boundedTestWorker(db, test.groupBy).drain(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(db.applied) != 1 || db.failedMessage(1) != "" {
				t.Fatalf("applied=%v failed=%s", db.applied, db.failedMessage(1))
			}
			if test.resolved && (len(db.incidentTx.resolutions) != 1 || len(db.incidentTx.assignments) != 0) {
				t.Fatal("resolved alert did not close its incident")
			}
		})
	}
}

func TestWorkerRejectsWholePoisonBatchBeforeSideEffects(t *testing.T) {
	event := testRawEvent(1, time.Now().UTC(), "GoodFirst", nil)
	var payload map[string]any
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	bad := map[string]any{"status": "firing", "labels": map[string]string{"alertname": strings.Repeat("x", 256)}, "startsAt": "2026-01-01T00:00:00Z"}
	payload["alerts"] = append(payload["alerts"].([]any), bad)
	event.Payload, _ = json.Marshal(payload)
	db := newFakePendingEventStore(event)
	if err := boundedTestWorker(db, nil).drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if db.attemptCount() != 0 || db.failedMessage(1) == "" {
		t.Fatal("partially applied poison batch")
	}
}

type applyErrorStore struct {
	*fakePendingEventStore
	err error
}

func (s *applyErrorStore) ApplyRawEvent(ctx context.Context, id uint64, inputs []store.AlertInput, at time.Time, hook store.RawEventApplyHook) ([]store.AlertApplyResult, error) {
	if id == 1 {
		return nil, s.err
	}
	return s.fakePendingEventStore.ApplyRawEvent(ctx, id, inputs, at, hook)
}

func TestWorkerOnlyQuarantinesExplicitPermanentErrors(t *testing.T) {
	for _, cause := range []error{store.ErrInvalidAlertInput, errors.New("database unavailable"), context.DeadlineExceeded} {
		db := &applyErrorStore{newFakePendingEventStore(testRawEvent(1, time.Now().UTC(), "First", nil), testRawEvent(2, time.Now().UTC(), "Second", nil)), cause}
		err := boundedTestWorker(db, nil).drain(context.Background())
		if errors.Is(cause, store.ErrInvalidAlertInput) {
			if err != nil || db.failedMessage(1) == "" || len(db.applied) != 1 || db.applied[0].id != 2 {
				t.Fatalf("permanent error not isolated: %v", err)
			}
		} else if !errors.Is(err, cause) || db.failedMessage(1) != "" || len(db.applied) != 0 {
			t.Fatalf("transient error lost FIFO retry: %v", err)
		}
	}
}
