package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
	}}}, fakeSessionAuth{unauth: true}, StreamConfig{MaxDuration: 20 * time.Millisecond, PollInterval: 5 * time.Millisecond})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/incidents/1/stream", nil)
	resp := httptest.NewRecorder()
	api.ServeHTTP(resp, req)
	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized = %d", resp.Code)
	}

	live := NewStreamAPI(fakeEventStore{rows: []store.IncidentEvent{{
		ID: 5, IncidentID: 1, EventType: "run.started", Phase: "diagnose", Status: "running", Summary: "started",
	}}}, fakeSessionAuth{}, StreamConfig{MaxDuration: 30 * time.Millisecond, PollInterval: 10 * time.Millisecond})
	req = httptest.NewRequest(http.MethodGet, "/api/v1/incidents/1/stream", nil)
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

	heart := NewStreamAPI(fakeEventStore{}, fakeSessionAuth{}, StreamConfig{MaxDuration: 25 * time.Millisecond, PollInterval: 5 * time.Millisecond})
	req = httptest.NewRequest(http.MethodGet, "/api/v1/incidents/1/stream", nil)
	resp = httptest.NewRecorder()
	heart.ServeHTTP(resp, req)
	if !strings.Contains(resp.Body.String(), ": heartbeat") {
		t.Fatalf("missing heartbeat: %q", resp.Body.String())
	}
}

func TestStreamConnectionLimit(t *testing.T) {
	api := NewStreamAPI(fakeEventStore{}, fakeSessionAuth{}, StreamConfig{MaxConnections: 1, MaxDuration: time.Hour})
	if !api.tryAcquire() {
		t.Fatal("first acquire")
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/incidents/1/stream", nil)
	resp := httptest.NewRecorder()
	api.ServeHTTP(resp, req)
	if resp.Code != http.StatusServiceUnavailable {
		t.Fatalf("limit = %d", resp.Code)
	}
}
