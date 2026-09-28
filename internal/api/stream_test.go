package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"oncall-agent/internal/eventlog"
	"oncall-agent/internal/store"
)

type fakeEventStore struct {
	rows []store.IncidentEvent
}

func (f fakeEventStore) ListIncidentEvents(_ context.Context, _ uint64, after uint64, _ int) ([]store.IncidentEvent, error) {
	out := make([]store.IncidentEvent, 0, len(f.rows))
	for _, row := range f.rows {
		if row.ID > after {
			out = append(out, row)
		}
	}
	return out, nil
}

func TestStreamUnauthorizedAndLastEventID(t *testing.T) {
	api := NewStreamAPI(fakeEventStore{rows: []store.IncidentEvent{{
		ID: 5, IncidentID: 1, EventType: "run.started", Phase: "diagnose", Status: "running", Summary: "started",
	}}}, testAuth(t), StreamConfig{MaxDuration: 20 * time.Millisecond, PollInterval: 5 * time.Millisecond})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/incidents/1/stream", nil)
	resp := httptest.NewRecorder()
	api.ServeHTTP(resp, req)
	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized = %d", resp.Code)
	}

	live := NewStreamAPI(fakeEventStore{rows: []store.IncidentEvent{{
		ID: 5, IncidentID: 1, EventType: "run.started", Phase: "diagnose", Status: "running", Summary: "started",
	}}}, testAuth(t), StreamConfig{MaxDuration: 30 * time.Millisecond, PollInterval: 10 * time.Millisecond})
	req = withBearer(httptest.NewRequest(http.MethodGet, "/api/v1/incidents/1/stream", nil), testViewerToken)
	req.Header.Set("Last-Event-ID", "4")
	resp = httptest.NewRecorder()
	live.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("stream = %d", resp.Code)
	}
	body := resp.Body.String()
	if !strings.Contains(body, "id: 5") || !strings.Contains(body, "event: run.started") {
		t.Fatalf("body = %q", body)
	}

	heart := NewStreamAPI(fakeEventStore{}, testAuth(t), StreamConfig{MaxDuration: 25 * time.Millisecond, PollInterval: 5 * time.Millisecond})
	req = withBearer(httptest.NewRequest(http.MethodGet, "/api/v1/incidents/1/stream", nil), testViewerToken)
	resp = httptest.NewRecorder()
	heart.ServeHTTP(resp, req)
	if !strings.Contains(resp.Body.String(), ": heartbeat") {
		t.Fatalf("missing heartbeat: %q", resp.Body.String())
	}
}

func TestStreamExecutionAndVerificationEvents(t *testing.T) {
	types := []eventlog.EventType{eventlog.EventExecutionAborted, eventlog.EventVerifyQueued, eventlog.EventVerifyStarted, eventlog.EventVerifyChecked, eventlog.EventVerifyStable, eventlog.EventVerifyRecurred, eventlog.EventCompensationQueued, eventlog.EventReviewRecorded}
	rows := make([]store.IncidentEvent, 0, len(types))
	for index, eventType := range types {
		rows = append(rows, store.IncidentEvent{ID: uint64(index + 1), IncidentID: 11, EventType: string(eventType)})
	}
	api := NewStreamAPI(fakeEventStore{rows: rows}, testAuth(t))
	response := httptest.NewRecorder()
	var cursor uint64
	if err := api.writeEvents(response, response, context.Background(), 11, &cursor); err != nil {
		t.Fatal(err)
	}
	for _, eventType := range types {
		if !strings.Contains(response.Body.String(), "event: "+string(eventType)+"\n") {
			t.Fatalf("event missing: %s", response.Body.String())
		}
	}
	if cursor != uint64(len(types)) {
		t.Fatalf("cursor = %d", cursor)
	}
}

func TestStreamConnectionLimit(t *testing.T) {
	api := NewStreamAPI(fakeEventStore{}, testAuth(t), StreamConfig{MaxConnections: 1, MaxDuration: time.Hour})
	if !api.tryAcquire() {
		t.Fatal("first acquire")
	}
	req := withBearer(httptest.NewRequest(http.MethodGet, "/api/v1/incidents/1/stream", nil), testViewerToken)
	resp := httptest.NewRecorder()
	api.ServeHTTP(resp, req)
	if resp.Code != http.StatusServiceUnavailable {
		t.Fatalf("limit = %d", resp.Code)
	}
}
