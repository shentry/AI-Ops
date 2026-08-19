package api

import (
	"context"
	"crypto/subtle"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/gogf/gf/v2/net/ghttp"

	"oncall-agent/internal/ingest"
	"oncall-agent/internal/metrics"
	"oncall-agent/internal/store"
)

type rawEventStore interface {
	CreateRawEvent(context.Context, string, []byte, time.Time) (store.RawEvent, error)
}

type eventNotifier interface {
	Notify()
}

type AlertmanagerWebhook struct {
	db        rawEventStore
	worker    eventNotifier
	authToken string
}

func NewAlertmanagerWebhook(db rawEventStore, worker eventNotifier, authToken string) *AlertmanagerWebhook {
	return &AlertmanagerWebhook{db: db, worker: worker, authToken: authToken}
}

// Handle adapts the standard HTTP handler to GoFrame routing.
func (h *AlertmanagerWebhook) Handle(r *ghttp.Request) {
	h.serveHTTP(r.Response.BufferWriter, r.Request, func() ([]byte, error) { return r.GetBody(), nil })
}

func (h *AlertmanagerWebhook) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.serveHTTP(w, r, func() ([]byte, error) { return io.ReadAll(r.Body) })
}

func (h *AlertmanagerWebhook) serveHTTP(w http.ResponseWriter, r *http.Request, readBody func() ([]byte, error)) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+h.authToken)) != 1 {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	payload, err := readBody()
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if _, err := h.db.CreateRawEvent(r.Context(), ingest.SourceAlertmanager, payload, time.Now().UTC()); err != nil {
		if errors.Is(err, store.ErrInvalidRawEvent) {
			w.WriteHeader(http.StatusBadRequest)
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		return
	}
	h.worker.Notify()
	metrics.Inc(metrics.WebhookReceived)
	w.WriteHeader(http.StatusAccepted)
}
