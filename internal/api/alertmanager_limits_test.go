package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gogf/gf/v2/net/ghttp"

	"oncall-agent/internal/store"
)

func webhookRequest(body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/webhook/alertmanager", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer secret")
	return r
}

func TestWebhookBoundsBeforeDurableReceipt(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
		want int
	}{
		{"byte boundary", `{"padding":"` + strings.Repeat("x", maxWebhookBytes-len(`{"padding":""}`)) + `"}`, http.StatusAccepted},
		{"body too large", `{"padding":"` + strings.Repeat("x", maxWebhookBytes) + `"}`, http.StatusRequestEntityTooLarge},
		{"alert boundary", `{"alerts":[` + strings.TrimSuffix(strings.Repeat(`{},`, maxWebhookAlerts), ",") + `]}`, http.StatusAccepted},
		{"too many alerts", `{"alerts":[` + strings.TrimSuffix(strings.Repeat(`{},`, maxWebhookAlerts+1), ",") + `]}`, http.StatusRequestEntityTooLarge},
		{"bad json", `{"alerts":`, http.StatusBadRequest},
	} {
		for _, unknownLength := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/unknown-length=%t", test.name, unknownLength), func(t *testing.T) {
				db, notifier := &fakeRawEventStore{}, &fakeNotifier{}
				handler := NewAlertmanagerWebhook(db, notifier, testAuth(t))
				req := webhookRequest(test.body)
				if unknownLength {
					req.ContentLength = -1
				}
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, req)
				if response.Code != test.want {
					t.Fatalf("status=%d want=%d", response.Code, test.want)
				}
				wantCalls := 0
				if test.want == http.StatusAccepted {
					wantCalls = 1
				}
				if db.calls != wantCalls || notifier.calls != wantCalls {
					t.Fatalf("db=%d notify=%d want=%d", db.calls, notifier.calls, wantCalls)
				}
			})
		}
	}
}

type blockingRawStore struct {
	entered chan context.Context
	release chan struct{}
}

func (s *blockingRawStore) CreateRawEvent(ctx context.Context, _ string, _ []byte, _ time.Time) (store.RawEvent, error) {
	s.entered <- ctx
	select {
	case <-s.release:
		return store.RawEvent{ID: 1}, nil
	case <-ctx.Done():
		return store.RawEvent{}, ctx.Err()
	}
}

type concurrentNotifier struct {
	mu    sync.Mutex
	calls int
}

func (n *concurrentNotifier) Notify() {
	n.mu.Lock()
	n.calls++
	n.mu.Unlock()
}

func TestWebhookInflightLimitAndDurableReceipt(t *testing.T) {
	db := &blockingRawStore{entered: make(chan context.Context, maxWebhookInflight), release: make(chan struct{})}
	notifier := &concurrentNotifier{}
	handler := NewAlertmanagerWebhook(db, notifier, testAuth(t))
	responses := make(chan int, maxWebhookInflight)
	for i := 0; i < maxWebhookInflight; i++ {
		go func() {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, webhookRequest(`{"alerts":[]}`))
			responses <- response.Code
		}()
	}
	for i := 0; i < maxWebhookInflight; i++ {
		select {
		case ctx := <-db.entered:
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) > webhookTimeout {
				t.Fatal("durable write has no bounded deadline")
			}
		case <-time.After(time.Second):
			t.Fatal("request did not reach persistence")
		}
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, webhookRequest(`{"alerts":[]}`))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("overload status=%d", response.Code)
	}
	notifier.mu.Lock()
	calls := notifier.calls
	notifier.mu.Unlock()
	if calls != 0 || len(responses) != 0 {
		t.Fatal("receipt or notification preceded durable write")
	}
	close(db.release)
	for i := 0; i < maxWebhookInflight; i++ {
		if code := <-responses; code != http.StatusAccepted {
			t.Fatalf("persisted request status=%d", code)
		}
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, webhookRequest(`{"alerts":[]}`))
	if response.Code != http.StatusAccepted {
		t.Fatalf("released capacity status=%d", response.Code)
	}
}

func TestWebhookPropagatesEarlierDeadline(t *testing.T) {
	db := &blockingRawStore{entered: make(chan context.Context, 1), release: make(chan struct{})}
	notifier := &fakeNotifier{}
	handler := NewAlertmanagerWebhook(db, notifier, testAuth(t))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, webhookRequest(`{}`).WithContext(ctx))
	if response.Code != http.StatusServiceUnavailable || notifier.calls != 0 {
		t.Fatalf("deadline status=%d notify=%d", response.Code, notifier.calls)
	}
	if len(handler.inflight) != 0 {
		t.Fatal("timed out request retained its admission slot")
	}
}

func TestWebhookGoFrameUsesBoundedBodyReader(t *testing.T) {
	db, notifier := &fakeRawEventStore{}, &fakeNotifier{}
	handler := NewAlertmanagerWebhook(db, notifier, testAuth(t))
	server := ghttp.GetServer(t.Name())
	server.SetAddr("127.0.0.1:0")
	server.SetDumpRouterMap(false)
	server.SetLogStdout(false)
	server.BindHandler("/webhook/alertmanager", handler.Handle)
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	defer server.Shutdown()
	client := &http.Client{Timeout: 2 * time.Second}
	for _, test := range []struct {
		body string
		want int
	}{
		{`{"alerts":[]}`, http.StatusAccepted},
		{`{"padding":"` + strings.Repeat("x", maxWebhookBytes) + `"}`, http.StatusRequestEntityTooLarge},
	} {
		req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/webhook/alertmanager", server.GetListenedPort()), strings.NewReader(test.body))
		if err != nil {
			t.Fatal(err)
		}
		req.ContentLength = -1 // streaming bodies must be bounded on the actual production adapter too.
		req.Header.Set("Authorization", "Bearer secret")
		req.Header.Set("Content-Type", "application/json")
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if response.StatusCode != test.want {
			t.Fatalf("GoFrame status=%d want=%d", response.StatusCode, test.want)
		}
	}
	if db.calls != 1 || notifier.calls != 1 {
		t.Fatalf("GoFrame side effects db=%d notify=%d", db.calls, notifier.calls)
	}
}

func TestWebhookSlowBodyHasReadDeadline(t *testing.T) {
	db := &fakeRawEventStore{}
	handler := NewAlertmanagerWebhook(db, &fakeNotifier{}, testAuth(t))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 50*time.Millisecond)
		defer cancel()
		handler.ServeHTTP(w, r.WithContext(ctx))
	}))
	defer server.Close()
	reader, writer := io.Pipe()
	defer writer.Close()
	req, err := http.NewRequest(http.MethodPost, server.URL, reader)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer secret")
	client := &http.Client{Timeout: 2 * time.Second}
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable || db.calls != 0 {
		t.Fatalf("slow body status=%d writes=%d", response.StatusCode, db.calls)
	}
}
