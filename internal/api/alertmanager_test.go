package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"oncall-agent/internal/store"
)

type fakeRawEventStore struct {
	err          error
	calls        int
	source       string
	payload      []byte
	contextValue any
}

func (f *fakeRawEventStore) CreateRawEvent(ctx context.Context, source string, payload []byte, _ time.Time) (store.RawEvent, error) {
	f.calls++
	f.source = source
	f.payload = append([]byte(nil), payload...)
	f.contextValue = ctx.Value(webhookContextKey{})
	if f.err != nil {
		return store.RawEvent{}, f.err
	}
	return store.RawEvent{ID: 1, Status: "pending"}, nil
}

type fakeNotifier struct{ calls int }

func (f *fakeNotifier) Notify() { f.calls++ }

type webhookContextKey struct{}

func TestAlertmanagerWebhookRejectsMethodAndTokenBeforePersistence(t *testing.T) {
	tests := []struct {
		name   string
		method string
		token  string
		want   int
	}{
		{name: "wrong method", method: http.MethodGet, token: "Bearer secret", want: http.StatusMethodNotAllowed},
		{name: "missing token", method: http.MethodPost, want: http.StatusUnauthorized},
		{name: "wrong token", method: http.MethodPost, token: "Bearer wrong", want: http.StatusUnauthorized},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db := &fakeRawEventStore{}
			notifier := &fakeNotifier{}
			handler := NewAlertmanagerWebhook(db, notifier, "secret")
			req := httptest.NewRequest(test.method, "/webhook/alertmanager", strings.NewReader(`{"version":"4"}`))
			req.Header.Set("Authorization", test.token)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, req)
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d", response.Code, test.want)
			}
			if db.calls != 0 || notifier.calls != 0 {
				t.Fatalf("side effects: db=%d notify=%d", db.calls, notifier.calls)
			}
		})
	}
}

func TestAlertmanagerWebhookClassifiesInputAndStorageErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{name: "invalid json", err: store.ErrInvalidRawEvent, want: http.StatusBadRequest},
		{name: "storage unavailable", err: errors.New("database unavailable"), want: http.StatusServiceUnavailable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db := &fakeRawEventStore{err: test.err}
			notifier := &fakeNotifier{}
			handler := NewAlertmanagerWebhook(db, notifier, "secret")
			req := httptest.NewRequest(http.MethodPost, "/webhook/alertmanager", strings.NewReader(`{"version":"4"}`))
			req.Header.Set("Authorization", "Bearer secret")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, req)
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d", response.Code, test.want)
			}
			if db.calls != 1 || notifier.calls != 0 {
				t.Fatalf("side effects: db=%d notify=%d", db.calls, notifier.calls)
			}
		})
	}
}

func TestAlertmanagerWebhookPersistsThenNotifies(t *testing.T) {
	db := &fakeRawEventStore{}
	notifier := &fakeNotifier{}
	handler := NewAlertmanagerWebhook(db, notifier, "secret")
	payload := `{"version":"4","alerts":[]}`
	ctx := context.WithValue(context.Background(), webhookContextKey{}, "request-context")
	req := httptest.NewRequest(http.MethodPost, "/webhook/alertmanager", strings.NewReader(payload)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, req)

	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", response.Code)
	}
	if db.calls != 1 || notifier.calls != 1 {
		t.Fatalf("side effects: db=%d notify=%d", db.calls, notifier.calls)
	}
	if db.source != "alertmanager" || string(db.payload) != payload {
		t.Fatalf("persisted source=%q payload=%q", db.source, db.payload)
	}
	if db.contextValue != "request-context" {
		t.Fatalf("request context value = %#v", db.contextValue)
	}
}
