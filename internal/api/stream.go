package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gogf/gf/v2/net/ghttp"

	"oncall-agent/internal/store"
)

// EventStore is the persistence seam for SSE. It must implement strict
// id-after pagination; the handler never uses an in-process event channel as a
// source of truth.
type EventStore interface {
	ListIncidentEvents(context.Context, uint64, uint64, int) ([]store.IncidentEvent, error)
}

// StreamConfig allows deterministic tests while production defaults remain
// one-second polling, ten-minute connection lifetime and 100 connections.
type StreamConfig struct {
	PollInterval   time.Duration
	MaxDuration    time.Duration
	MaxConnections int
}

// StreamAPI serves an Incident's public durable event stream.
type StreamAPI struct {
	db      EventStore
	auth    SessionAuthenticator
	poll    time.Duration
	maxAge  time.Duration
	maxConn int32
	active  atomic.Int32
}

func NewStreamAPI(db EventStore, authn SessionAuthenticator, configs ...StreamConfig) *StreamAPI {
	cfg := StreamConfig{PollInterval: time.Second, MaxDuration: 10 * time.Minute, MaxConnections: 100}
	if len(configs) > 0 {
		if configs[0].PollInterval > 0 {
			cfg.PollInterval = configs[0].PollInterval
		}
		if configs[0].MaxDuration > 0 {
			cfg.MaxDuration = configs[0].MaxDuration
		}
		if configs[0].MaxConnections > 0 {
			cfg.MaxConnections = configs[0].MaxConnections
		}
	}
	return &StreamAPI{db: db, auth: authn, poll: cfg.PollInterval, maxAge: cfg.MaxDuration, maxConn: int32(cfg.MaxConnections)}
}

// NewIncidentStreamAPI is a descriptive constructor alias for route assembly.
func NewIncidentStreamAPI(db EventStore, authn SessionAuthenticator, configs ...StreamConfig) *StreamAPI {
	return NewStreamAPI(db, authn, configs...)
}

func (h *StreamAPI) Handle(r *ghttp.Request) {
	if r == nil || r.Response == nil {
		return
	}
	// ghttp buffers writes by default. Adapt Flush to Response.Flush so the
	// standard-library implementation and httptest ResponseRecorder share one
	// ServeHTTP path.
	h.serveHTTP(&gframeFlushWriter{ResponseWriter: r.Response.BufferWriter, response: r.Response}, r.Request)
}

func (h *StreamAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.serveHTTP(w, r)
}

func (h *StreamAPI) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.db == nil || r == nil {
		writeNotFound(w)
		return
	}
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w)
		return
	}
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/"), "/"), "/")
	if len(parts) != 3 || parts[0] != "incidents" || parts[2] != "stream" {
		writeNotFound(w)
		return
	}
	incidentID, err := parseIncidentID(parts[1])
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid incident id")
		return
	}
	if _, _, ok := authenticateSession(h.auth, r); !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !h.tryAcquire() {
		writeError(w, http.StatusServiceUnavailable, "too many event streams")
		return
	}
	defer h.active.Add(-1)

	after, err := streamCursor(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid Last-Event-ID")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "stream flushing unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	ctx := r.Context()
	if err := h.writeEvents(w, flusher, ctx, incidentID, &after); err != nil {
		return
	}
	poll := time.NewTicker(h.poll)
	defer poll.Stop()
	deadline := time.NewTimer(h.maxAge)
	defer deadline.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-deadline.C:
			return
		case <-poll.C:
			if err := h.writeEvents(w, flusher, ctx, incidentID, &after); err != nil {
				return
			}
		}
	}
}

func (h *StreamAPI) tryAcquire() bool {
	for {
		current := h.active.Load()
		if current >= h.maxConn {
			return false
		}
		if h.active.CompareAndSwap(current, current+1) {
			return true
		}
	}
}

func streamCursor(r *http.Request) (uint64, error) {
	raw := strings.TrimSpace(r.Header.Get("Last-Event-ID"))
	if raw == "" {
		raw = strings.TrimSpace(r.URL.Query().Get("after"))
	}
	if raw == "" {
		return 0, nil
	}
	return strconv.ParseUint(raw, 10, 64)
}

func (h *StreamAPI) writeEvents(w http.ResponseWriter, flusher http.Flusher, ctx context.Context, incidentID uint64, cursor *uint64) error {
	rows, err := h.db.ListIncidentEvents(ctx, incidentID, *cursor, 100)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}
	for _, row := range rows {
		if row.ID <= *cursor {
			continue
		}
		payload, err := json.Marshal(eventDTO(row))
		if err != nil {
			return err
		}
		eventName := strings.NewReplacer("\r", "_", "\n", "_").Replace(safeText(row.EventType, 64))
		if eventName == "" {
			eventName = "incident.event"
		}
		if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", row.ID, eventName, payload); err != nil {
			return err
		}
		*cursor = row.ID
	}
	flusher.Flush()
	return nil
}

type gframeFlushWriter struct {
	http.ResponseWriter
	response *ghttp.Response
}

func (w *gframeFlushWriter) Flush() {
	if w.response != nil {
		w.response.Flush()
	}
}
