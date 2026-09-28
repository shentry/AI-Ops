package notify

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"oncall-agent/internal/config"
)

func TestWebhookAcknowledgementRequiresExplicitSuccess(t *testing.T) {
	for _, provider := range []string{"wecom", "feishu"} {
		t.Run(provider, func(t *testing.T) {
			for _, tc := range []struct {
				name, body string
				ok         bool
			}{
				{"empty", "", false},
				{"malformed", `<html>gateway error</html>`, false},
				{"truncated JSON", `{"errcode":0`, false},
				{"missing code", `{"errmsg":"ok"}`, false},
				{"null response", `null`, false},
				{"null code", `{"errcode":null,"code":null}`, false},
				{"string code", `{"errcode":"0","code":"0"}`, false},
				{"business failure", `{"errcode":93000,"code":19021}`, false},
				{"conflicting errcode", `{"errcode":93000,"code":0}`, false},
				{"conflicting code", `{"errcode":0,"code":19021}`, false},
				{"trailing garbage", `{"errcode":0,"code":0} garbage`, false},
				{"oversized", `{"errcode":0,"code":0,"message":"` + strings.Repeat("x", 4096) + `"}`, false},
				{"wecom success", `{"errcode":0,"errmsg":"ok"}`, true},
				{"feishu success", `{"code":0,"msg":"success"}`, provider == "feishu"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						w.Header().Set("Content-Type", "application/json")
						_, _ = io.WriteString(w, tc.body)
					}))
					defer server.Close()
					notifier, err := NewWebhookNotifier(config.IMConfig{Provider: provider, Webhook: server.URL})
					if err != nil {
						t.Fatal(err)
					}
					delivery, err := notifier.Send(context.Background(), Notification{Kind: NotificationEscalationRequired, IncidentID: 7, Summary: "manual investigation required"})
					if tc.ok {
						if err != nil || delivery.Provider != provider {
							t.Fatalf("explicit success rejected: delivery=%+v err=%v", delivery, err)
						}
					} else if err == nil || delivery != (Delivery{}) {
						t.Fatalf("unacknowledged notification reported delivered: delivery=%+v err=%v", delivery, err)
					}
				})
			}
		})
	}
}

// A proxy may close after a complete-looking JSON prefix. Content-Length
// exposes the incomplete response even when the bytes received parse as success.
func TestWebhookAcknowledgementRejectsTruncatedTransport(t *testing.T) {
	for _, provider := range []string{"wecom", "feishu"} {
		t.Run(provider, func(t *testing.T) {
			body := `{"errcode":0,"code":0}`
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Length", strconv.Itoa(len(body)+20))
				_, _ = io.WriteString(w, body)
			}))
			defer server.Close()
			notifier, err := NewWebhookNotifier(config.IMConfig{Provider: provider, Webhook: server.URL})
			if err != nil {
				t.Fatal(err)
			}
			if delivery, err := notifier.Send(context.Background(), Notification{}); err == nil || delivery != (Delivery{}) {
				t.Fatalf("truncated acknowledgement accepted: delivery=%+v err=%v", delivery, err)
			}
		})
	}
}
