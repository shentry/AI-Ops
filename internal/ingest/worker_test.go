package ingest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/datatypes"

	"oncall-agent/internal/config"
	"oncall-agent/internal/store"
)

type appliedRawEvent struct {
	id     uint64
	inputs []store.AlertInput
}

type fakePendingEventStore struct {
	mu            sync.Mutex
	events        []store.RawEvent
	failed        map[uint64]string
	applied       []appliedRawEvent
	applyFailures int
	attempts      int
	appliedCh     chan appliedRawEvent
	failedCh      chan uint64
}

func newFakePendingEventStore(events ...store.RawEvent) *fakePendingEventStore {
	return &fakePendingEventStore{
		events: events, failed: make(map[uint64]string),
		appliedCh: make(chan appliedRawEvent, 512), failedCh: make(chan uint64, 16),
	}
}

func (f *fakePendingEventStore) NextPendingRawEvent(ctx context.Context) (store.RawEvent, bool, error) {
	if err := ctx.Err(); err != nil {
		return store.RawEvent{}, false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	pending := make([]store.RawEvent, 0, len(f.events))
	for _, event := range f.events {
		if event.Status == "pending" {
			pending = append(pending, event)
		}
	}
	if len(pending) == 0 {
		return store.RawEvent{}, false, nil
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].ID < pending[j].ID })
	return pending[0], true, nil
}

func (f *fakePendingEventStore) ApplyRawEvent(ctx context.Context, id uint64, inputs []store.AlertInput, _ time.Time) ([]store.DedupResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.attempts++
	if f.applyFailures > 0 {
		f.applyFailures--
		f.mu.Unlock()
		return nil, errors.New("transient store failure")
	}
	for index := range f.events {
		if f.events[index].ID == id {
			f.events[index].Status = "processed"
		}
	}
	record := appliedRawEvent{id: id, inputs: append([]store.AlertInput(nil), inputs...)}
	f.applied = append(f.applied, record)
	f.mu.Unlock()
	f.appliedCh <- record
	results := make([]store.DedupResult, len(inputs))
	for index := range results {
		results[index] = store.DedupNew
	}
	return results, nil
}

func (f *fakePendingEventStore) MarkRawEventFailed(ctx context.Context, id uint64, message string, _ time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	for index := range f.events {
		if f.events[index].ID == id {
			f.events[index].Status = "failed"
		}
	}
	f.failed[id] = message
	f.mu.Unlock()
	f.failedCh <- id
	return nil
}

func (f *fakePendingEventStore) attemptCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attempts
}

func (f *fakePendingEventStore) failedMessage(id uint64) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.failed[id]
}

func TestWorkerDrainsPendingInIDOrderAndPreservesReceiveTime(t *testing.T) {
	firstTime := time.Date(2026, 8, 17, 10, 0, 0, 0, time.UTC)
	secondTime := firstTime.Add(time.Minute)
	fake := newFakePendingEventStore(
		testRawEvent(2, secondTime, "SecondAlert", map[string]string{"service": "payments"}),
		testRawEvent(1, firstTime, "FirstAlert", map[string]string{"service": "payments"}),
	)
	worker, cancel := startTestWorker(t, fake, config.IngestConfig{SeverityLabel: "severity"})
	defer func() { cancel(); worker.Wait() }()

	first := receiveApplied(t, fake.appliedCh)
	second := receiveApplied(t, fake.appliedCh)
	if first.id != 1 || second.id != 2 {
		t.Fatalf("apply order = [%d, %d], want [1, 2]", first.id, second.id)
	}
	if len(first.inputs) != 1 || !first.inputs[0].ReceivedAt.Equal(firstTime) {
		t.Fatalf("received_at = %v, want %v", first.inputs[0].ReceivedAt, firstTime)
	}
	if first.inputs[0].GeneratorURL != "http://127.0.0.1:9090/graph?g0.expr=vector(1)" {
		t.Fatalf("generator URL = %q", first.inputs[0].GeneratorURL)
	}
}

func TestWorkerRetriesTransientApplyFailureWithoutMarkingFailed(t *testing.T) {
	fake := newFakePendingEventStore(testRawEvent(1, time.Now().UTC(), "RetryAlert", nil))
	fake.applyFailures = 1
	worker, cancel := startTestWorker(t, fake, config.IngestConfig{SeverityLabel: "severity"})
	defer func() { cancel(); worker.Wait() }()

	receiveApplied(t, fake.appliedCh)
	if fake.attemptCount() != 2 {
		t.Fatalf("apply attempts = %d, want 2", fake.attemptCount())
	}
	if message := fake.failedMessage(1); message != "" {
		t.Fatalf("raw event was marked failed: %q", message)
	}
}

func TestWorkerRejectsAlertWithoutConfiguredFingerprintFields(t *testing.T) {
	fake := newFakePendingEventStore(testRawEvent(1, time.Now().UTC(), "MissingService", nil))
	worker, cancel := startTestWorker(t, fake, config.IngestConfig{SeverityLabel: "severity", FingerprintFields: []string{"service"}})
	defer func() { cancel(); worker.Wait() }()

	select {
	case id := <-fake.failedCh:
		if id != 1 {
			t.Fatalf("failed raw event = %d, want 1", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for raw event rejection")
	}
	if message := fake.failedMessage(1); !strings.Contains(message, "configured fingerprint fields") {
		t.Fatalf("failure message = %q", message)
	}
	if fake.attemptCount() != 0 {
		t.Fatalf("apply attempts = %d, want 0", fake.attemptCount())
	}
}

func TestWorkerDrainsMoreThanLegacyChannelCapacity(t *testing.T) {
	events := make([]store.RawEvent, 200)
	for index := range events {
		id := uint64(index + 1)
		events[index] = testRawEvent(id, time.Now().UTC(), fmt.Sprintf("BurstAlert%d", id), nil)
	}
	fake := newFakePendingEventStore(events...)
	worker, cancel := startTestWorker(t, fake, config.IngestConfig{SeverityLabel: "severity"})
	defer func() { cancel(); worker.Wait() }()

	for index := 0; index < len(events); index++ {
		receiveApplied(t, fake.appliedCh)
	}
	if fake.attemptCount() != len(events) {
		t.Fatalf("apply attempts = %d, want %d", fake.attemptCount(), len(events))
	}
}

func startTestWorker(t *testing.T, fake pendingEventStore, cfg config.IngestConfig) (*Worker, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	worker := newWorker(fake, cfg, log.New(io.Discard, "", 0), 5*time.Millisecond)
	if err := worker.Start(ctx); err != nil {
		cancel()
		t.Fatalf("Start() error = %v", err)
	}
	return worker, cancel
}

func receiveApplied(t *testing.T, applied <-chan appliedRawEvent) appliedRawEvent {
	t.Helper()
	select {
	case record := <-applied:
		return record
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for applied raw event")
		return appliedRawEvent{}
	}
}

func testRawEvent(id uint64, createdAt time.Time, alertName string, extraLabels map[string]string) store.RawEvent {
	labels := fmt.Sprintf(`"alertname":%q,"severity":"critical"`, alertName)
	for key, value := range extraLabels {
		labels += fmt.Sprintf(`,%q:%q`, key, value)
	}
	payload := fmt.Sprintf(`{"version":"4","status":"firing","alerts":[{"status":"firing","labels":{%s},"annotations":{"summary":"test"},"startsAt":"2026-08-17T09:59:00Z","endsAt":"0001-01-01T00:00:00Z","generatorURL":"http://127.0.0.1:9090/graph?g0.expr=vector(1)"}]}`, labels)
	return store.RawEvent{ID: id, Source: "alertmanager", Payload: datatypes.JSON([]byte(payload)), Status: "pending", CreatedAt: createdAt}
}
