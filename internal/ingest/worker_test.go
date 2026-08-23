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

type fakeIncidentTx struct {
	mu          sync.Mutex
	assignments []store.IncidentInput
	touches     []uint64
	resolutions []uint64
	runs        []store.AgentRun
	events      []store.IncidentEvent
}

func (f *fakeIncidentTx) AssignIncident(_ context.Context, input store.IncidentInput, _ time.Duration, _ int) (store.IncidentAssignment, error) {
	f.mu.Lock()
	f.assignments = append(f.assignments, input)
	f.mu.Unlock()
	return store.IncidentAssignment{IncidentID: 1, Status: "firing", Severity: input.Severity, Created: true, Promoted: true}, nil
}

func (f *fakeIncidentTx) TouchIncident(_ context.Context, id uint64, _ time.Time, _ int) error {
	f.mu.Lock()
	f.touches = append(f.touches, id)
	f.mu.Unlock()
	return nil
}

func (f *fakeIncidentTx) ResolveIncident(_ context.Context, id uint64, _ time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolutions = append(f.resolutions, id)
	return true, nil
}

func (f *fakeIncidentTx) EnqueueAgentRun(_ context.Context, run store.AgentRun) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs = append(f.runs, run)
	return nil
}

func (f *fakeIncidentTx) AppendIncidentEvent(_ context.Context, event store.IncidentEvent) (store.IncidentEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, event)
	return event, nil
}

func (f *fakeIncidentTx) OpenIncidentProblem(_ context.Context, problem store.IncidentProblem) (store.IncidentProblem, error) {
	return problem, nil
}

func (f *fakeIncidentTx) ResolveIncidentProblem(_ context.Context, _ uint64, _ string, _ *uint64, _ time.Time) (bool, error) {
	return true, nil
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
	incidentTx    *fakeIncidentTx
	dedupResults  []store.DedupResult
	incidentID    *uint64
}

func newFakePendingEventStore(events ...store.RawEvent) *fakePendingEventStore {
	return &fakePendingEventStore{
		events: events, failed: make(map[uint64]string),
		appliedCh: make(chan appliedRawEvent, 512), failedCh: make(chan uint64, 16),
		incidentTx: &fakeIncidentTx{},
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

func (f *fakePendingEventStore) ApplyRawEvent(ctx context.Context, id uint64, inputs []store.AlertInput, _ time.Time, hook store.RawEventApplyHook) ([]store.AlertApplyResult, error) {
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
	f.mu.Unlock()
	results := make([]store.AlertApplyResult, len(inputs))
	for index, input := range inputs {
		dedup := store.DedupNew
		if index < len(f.dedupResults) {
			dedup = f.dedupResults[index]
		}
		last := store.LastAlert{Fingerprint: input.Fingerprint}
		if f.incidentID != nil {
			incidentID := *f.incidentID
			last.IncidentID = &incidentID
		}
		results[index] = store.AlertApplyResult{Input: input, Dedup: dedup, Last: last}
		if hook != nil {
			if err := hook(ctx, f.incidentTx, results[index]); err != nil {
				return nil, err
			}
		}
	}

	f.mu.Lock()
	for index := range f.events {
		if f.events[index].ID == id {
			f.events[index].Status = "processed"
		}
	}
	record := appliedRawEvent{id: id, inputs: append([]store.AlertInput(nil), inputs...)}
	f.applied = append(f.applied, record)
	f.mu.Unlock()
	f.appliedCh <- record
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

func TestWorkerAssignsFiringAlertToIncident(t *testing.T) {
	fake := newFakePendingEventStore(testRawEvent(1, time.Now().UTC(), "CorrelateAlert", map[string]string{"service": "payments"}))
	worker, cancel := startTestWorker(t, fake, config.IngestConfig{SeverityLabel: "severity"})
	defer func() { cancel(); worker.Wait() }()
	receiveApplied(t, fake.appliedCh)
	fake.incidentTx.mu.Lock()
	assignments := append([]store.IncidentInput(nil), fake.incidentTx.assignments...)
	fake.incidentTx.mu.Unlock()
	if len(assignments) != 1 || assignments[0].GroupKey != "name:CorrelateAlert" {
		t.Fatalf("assignments = %#v", assignments)
	}
}

func TestWorkerUsesConfiguredGroupBy(t *testing.T) {
	fake := newFakePendingEventStore(testRawEvent(1, time.Now().UTC(), "ConfiguredGroupAlert", map[string]string{"service": "payments"}))
	worker, cancel := startTestWorker(t, fake, config.IngestConfig{SeverityLabel: "severity"}, config.CorrelateConfig{GroupBy: []string{"labels.service"}, WindowMinutes: 15, MinAlerts: 1})
	defer func() { cancel(); worker.Wait() }()
	receiveApplied(t, fake.appliedCh)
	fake.incidentTx.mu.Lock()
	assignments := append([]store.IncidentInput(nil), fake.incidentTx.assignments...)
	fake.incidentTx.mu.Unlock()
	if len(assignments) != 1 || assignments[0].GroupKey != "payments" {
		t.Fatalf("assignments = %#v", assignments)
	}
}

func TestWorkerTouchesFullDuplicateIncident(t *testing.T) {
	incidentID := uint64(7)
	fake := newFakePendingEventStore(testRawEvent(1, time.Now().UTC(), "HeartbeatAlert", nil))
	fake.dedupResults = []store.DedupResult{store.DedupFull}
	fake.incidentID = &incidentID
	worker, cancel := startTestWorker(t, fake, config.IngestConfig{SeverityLabel: "severity"})
	defer func() { cancel(); worker.Wait() }()
	receiveApplied(t, fake.appliedCh)
	fake.incidentTx.mu.Lock()
	touches := append([]uint64(nil), fake.incidentTx.touches...)
	assignments := append([]store.IncidentInput(nil), fake.incidentTx.assignments...)
	fake.incidentTx.mu.Unlock()
	if len(touches) != 1 || touches[0] != incidentID || len(assignments) != 0 {
		t.Fatalf("touches=%v assignments=%v", touches, assignments)
	}
}

func TestWorkerSkipsResolvedAlertForD04(t *testing.T) {
	fake := newFakePendingEventStore(resolvedRawEvent(1, time.Now().UTC(), "ResolvedAlert"))
	worker, cancel := startTestWorker(t, fake, config.IngestConfig{SeverityLabel: "severity"})
	defer func() { cancel(); worker.Wait() }()
	receiveApplied(t, fake.appliedCh)
	fake.incidentTx.mu.Lock()
	assignments := len(fake.incidentTx.assignments)
	touches := len(fake.incidentTx.touches)
	fake.incidentTx.mu.Unlock()
	if assignments != 0 || touches != 0 {
		t.Fatalf("resolved alert touched D04: assignments=%d touches=%d", assignments, touches)
	}
}

func TestWorkerPropagatesResolvedToIncident(t *testing.T) {
	incidentID := uint64(9)
	fake := newFakePendingEventStore(resolvedRawEvent(1, time.Now().UTC(), "ResolvePropAlert"))
	fake.incidentID = &incidentID
	worker, cancel := startTestWorker(t, fake, config.IngestConfig{SeverityLabel: "severity"})
	defer func() { cancel(); worker.Wait() }()
	receiveApplied(t, fake.appliedCh)
	fake.incidentTx.mu.Lock()
	resolutions := append([]uint64(nil), fake.incidentTx.resolutions...)
	assignments := len(fake.incidentTx.assignments)
	touches := len(fake.incidentTx.touches)
	fake.incidentTx.mu.Unlock()
	if len(resolutions) != 1 || resolutions[0] != incidentID || assignments != 0 || touches != 0 {
		t.Fatalf("resolutions=%v assignments=%d touches=%d", resolutions, assignments, touches)
	}
}

func TestWorkerSkipsResolvedWithoutIncidentLink(t *testing.T) {
	// resolved 告警没挂 incident（从未 firing 过）时不触发 resolved 传播。
	fake := newFakePendingEventStore(resolvedRawEvent(1, time.Now().UTC(), "OrphanResolvedAlert"))
	worker, cancel := startTestWorker(t, fake, config.IngestConfig{SeverityLabel: "severity"})
	defer func() { cancel(); worker.Wait() }()
	receiveApplied(t, fake.appliedCh)
	fake.incidentTx.mu.Lock()
	resolutions := len(fake.incidentTx.resolutions)
	fake.incidentTx.mu.Unlock()
	if resolutions != 0 {
		t.Fatalf("resolutions = %d, want 0", resolutions)
	}
}

func TestWorkerEnqueuesDiagnosisOnPromotion(t *testing.T) {
	fake := newFakePendingEventStore(testRawEvent(1, time.Now().UTC(), "DiagRouteAlert", nil))
	worker, cancel := startTestWorker(t, fake, config.IngestConfig{SeverityLabel: "severity"})
	defer func() { cancel(); worker.Wait() }()
	receiveApplied(t, fake.appliedCh)
	fake.incidentTx.mu.Lock()
	runs := append([]store.AgentRun(nil), fake.incidentTx.runs...)
	fake.incidentTx.mu.Unlock()
	// testRawEvent 的 severity 是 critical → 路由 full → pending 队列行。
	if len(runs) != 1 || runs[0].IncidentID != 1 || runs[0].Mode != "full" || runs[0].Status != "pending" || runs[0].RetryOf != nil {
		t.Fatalf("runs = %#v", runs)
	}
}

func TestWorkerEnqueuesSucceededRunForSkipSeverity(t *testing.T) {
	// info 级别路由到 skip：落一行 succeeded 供统计，不进 pending 队列。
	payload := `{"version":"4","status":"firing","alerts":[{"status":"firing","labels":{"alertname":"InfoAlert","severity":"info"},"annotations":{},"startsAt":"2026-08-17T09:59:00Z","endsAt":"0001-01-01T00:00:00Z"}]}`
	event := store.RawEvent{ID: 1, Source: "alertmanager", Payload: datatypes.JSON([]byte(payload)), Status: "pending", CreatedAt: time.Now().UTC()}
	fake := newFakePendingEventStore(event)
	worker, cancel := startTestWorker(t, fake, config.IngestConfig{SeverityLabel: "severity"})
	defer func() { cancel(); worker.Wait() }()
	receiveApplied(t, fake.appliedCh)
	fake.incidentTx.mu.Lock()
	runs := append([]store.AgentRun(nil), fake.incidentTx.runs...)
	fake.incidentTx.mu.Unlock()
	if len(runs) != 1 || runs[0].Mode != "skip" || runs[0].Status != "succeeded" || runs[0].FinishedAt == nil {
		t.Fatalf("runs = %#v", runs)
	}
}

func startTestWorker(t *testing.T, fake pendingEventStore, cfg config.IngestConfig, correlateConfigs ...config.CorrelateConfig) (*Worker, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	correlateCfg := config.CorrelateConfig{WindowMinutes: 15, MinAlerts: 1}
	if len(correlateConfigs) > 0 {
		correlateCfg = correlateConfigs[0]
	}
	severityRoute := map[string]string{
		"critical": "full",
		"high":     "full",
		"warning":  "light",
		"info":     "skip",
		"low":      "skip",
	}
	worker := newWorker(fake, cfg, correlateCfg, severityRoute, log.New(io.Discard, "", 0), 5*time.Millisecond)
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

func resolvedRawEvent(id uint64, createdAt time.Time, alertName string) store.RawEvent {
	event := testRawEvent(id, createdAt, alertName, nil)
	event.Payload = datatypes.JSON([]byte(strings.ReplaceAll(string(event.Payload), `"firing"`, `"resolved"`)))
	return event
}
