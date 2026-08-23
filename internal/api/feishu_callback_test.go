package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	larkevent "github.com/larksuite/oapi-sdk-go/v3/event"
)

type fakeDispatcher struct {
	got *larkevent.EventReq
}

func (f *fakeDispatcher) Handle(_ context.Context, req *larkevent.EventReq) *larkevent.EventResp {
	f.got = req
	return &larkevent.EventResp{StatusCode: http.StatusOK, Body: []byte(`{"challenge":"ok"}`)}
}

func TestFeishuCallbackAdapter(t *testing.T) {
	dispatcher := &fakeDispatcher{}
	api := NewFeishuCallbackAPI(dispatcher)
	req := httptest.NewRequest(http.MethodPost, "/integrations/feishu/events", strings.NewReader(`{"challenge":"abc"}`))
	req.Header.Set("X-Lark-Signature", "sig")
	resp := httptest.NewRecorder()
	api.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("code = %d", resp.Code)
	}
	if dispatcher.got == nil || string(dispatcher.got.Body) != `{"challenge":"abc"}` {
		t.Fatalf("req = %+v", dispatcher.got)
	}
	if !strings.Contains(resp.Body.String(), "challenge") {
		t.Fatalf("body = %s", resp.Body.String())
	}

	resp = httptest.NewRecorder()
	api.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/integrations/feishu/events", nil))
	if resp.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET = %d", resp.Code)
	}
}

func TestFeishuCallbackRejectsOversizedBody(t *testing.T) {
	dispatcher := &fakeDispatcher{}
	handler := NewFeishuCallbackAPI(dispatcher)
	req := httptest.NewRequest(http.MethodPost, "/integrations/feishu/events", strings.NewReader(strings.Repeat("x", maxFeishuCallbackBody+1)))
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d", resp.Code)
	}
	if dispatcher.got != nil {
		t.Fatal("dispatcher should not receive oversized body")
	}
}

func TestFeishuCallbackMissingDispatcher(t *testing.T) {
	api := NewFeishuCallbackAPI(nil)
	resp := httptest.NewRecorder()
	api.ServeHTTP(resp, httptest.NewRequest(http.MethodPost, "/integrations/feishu/events", io.NopCloser(strings.NewReader("{}"))))
	if resp.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d", resp.Code)
	}
}
