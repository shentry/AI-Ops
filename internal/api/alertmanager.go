package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/gogf/gf/v2/net/ghttp"

	"oncall-agent/internal/ingest"
	"oncall-agent/internal/metrics"
	"oncall-agent/internal/store"
)

const (
	maxWebhookBytes    = 1 << 20
	maxWebhookAlerts   = 100
	maxWebhookInflight = 8
	webhookTimeout     = 10 * time.Second
)

type rawEventStore interface {
	CreateRawEvent(context.Context, string, []byte, time.Time) (store.RawEvent, error)
}

type eventNotifier interface {
	Notify()
}

// AlertmanagerWebhook accepts alerts only from the machine identity.
type AlertmanagerWebhook struct {
	db       rawEventStore
	worker   eventNotifier
	auth     *Auth
	inflight chan struct{}
}

func NewAlertmanagerWebhook(db rawEventStore, worker eventNotifier, auth *Auth) *AlertmanagerWebhook {
	return &AlertmanagerWebhook{db: db, worker: worker, auth: auth, inflight: make(chan struct{}, maxWebhookInflight)}
}

// Handle adapts the standard HTTP handler to GoFrame routing.
func (h *AlertmanagerWebhook) Handle(r *ghttp.Request) {
	h.serveHTTP(r.Response.BufferWriter, r.Request, r.Response.RawWriter())
}

func (h *AlertmanagerWebhook) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.serveHTTP(w, r, w)
}

func (h *AlertmanagerWebhook) serveHTTP(w http.ResponseWriter, r *http.Request, rawWriter http.ResponseWriter) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if actor, ok := h.auth.Authenticate(r); !ok || !actor.Machine {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	select {
	case h.inflight <- struct{}{}:
		defer func() { <-h.inflight }()
	default:
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), webhookTimeout)
	defer cancel()
	// A context deadline alone cannot interrupt a slow request-body Read.
	controller := http.NewResponseController(rawWriter)
	deadline, _ := ctx.Deadline()
	if err := controller.SetReadDeadline(deadline); err == nil {
		defer controller.SetReadDeadline(time.Time{})
	}
	if r.ContentLength > maxWebhookBytes {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		return
	}
	payload, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		switch {
		case errors.As(err, &tooLarge):
			w.WriteHeader(http.StatusRequestEntityTooLarge)
		case ctx.Err() != nil || os.IsTimeout(err):
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
		return
	}
	var envelope struct {
		Alerts []json.RawMessage `json:"alerts"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if len(envelope.Alerts) > maxWebhookAlerts {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		return
	}
	if ctx.Err() != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if _, err := h.db.CreateRawEvent(ctx, ingest.SourceAlertmanager, payload, time.Now().UTC()); err != nil {
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
