package api

import (
	"context"
	"io"
	"net/http"

	"github.com/gogf/gf/v2/net/ghttp"
	larkevent "github.com/larksuite/oapi-sdk-go/v3/event"
)

// FeishuEventDispatcher is the SDK's narrow HTTP-facing contract. The SDK
// implementation performs URL challenge handling, signature verification, and
// AES decryption; this adapter only translates request and response shapes.
type FeishuEventDispatcher interface {
	Handle(context.Context, *larkevent.EventReq) *larkevent.EventResp
}

// FeishuCallbackAPI adapts a GoFrame route to the official Lark event
// dispatcher. It intentionally has no Bearer middleware: Feishu authentication
// is performed by the SDK using the configured verification/encryption keys.
type FeishuCallbackAPI struct {
	dispatcher FeishuEventDispatcher
}

func NewFeishuCallbackAPI(dispatcher FeishuEventDispatcher) *FeishuCallbackAPI {
	return &FeishuCallbackAPI{dispatcher: dispatcher}
}

// NewFeishuCallbackHandler is a descriptive alias for route assembly.
func NewFeishuCallbackHandler(dispatcher FeishuEventDispatcher) *FeishuCallbackAPI {
	return NewFeishuCallbackAPI(dispatcher)
}

// NewFeishuCallback constructs the callback endpoint for route registration.
func NewFeishuCallback(dispatcher FeishuEventDispatcher) *FeishuCallbackAPI {
	return NewFeishuCallbackAPI(dispatcher)
}

// FeishuCallback is kept as the concise handler name for integrations.
type FeishuCallback = FeishuCallbackAPI

// Handle adapts the GoFrame request to the standard-library implementation.
func (h *FeishuCallbackAPI) Handle(r *ghttp.Request) {
	h.ServeHTTP(r.Response.BufferWriter, r.Request)
}

const maxFeishuCallbackBody = 1 << 20

func (h *FeishuCallbackAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if h == nil || h.dispatcher == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "feishu dispatcher is not configured"})
		return
	}
	if r.ContentLength > maxFeishuCallbackBody {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxFeishuCallbackBody+1))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "read request body failed"})
		return
	}
	if int64(len(body)) > maxFeishuCallbackBody {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
		return
	}
	req := &larkevent.EventReq{
		Header:     cloneHeaders(r.Header),
		Body:       body,
		RequestURI: requestURI(r),
	}
	resp := h.dispatcher.Handle(r.Context(), req)
	if resp == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "feishu dispatcher returned empty response"})
		return
	}
	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	status := resp.StatusCode
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	if len(resp.Body) > 0 {
		_, _ = w.Write(resp.Body)
	}
}

func requestURI(r *http.Request) string {
	if r == nil || r.URL == nil {
		return ""
	}
	return r.URL.RequestURI()
}

func cloneHeaders(headers http.Header) map[string][]string {
	clone := make(map[string][]string, len(headers))
	for key, values := range headers {
		clone[key] = append([]string(nil), values...)
	}
	return clone
}

var _ http.Handler = (*FeishuCallbackAPI)(nil)
