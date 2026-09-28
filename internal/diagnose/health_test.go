package diagnose

import (
	"context"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"oncall-agent/internal/config"
)

func TestCollectorSharedHealthBlocksRedirectsAndCredentials(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/health" || r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
			t.Errorf("unexpected request: %v", r)
		}
		w.Header().Set("Location", "/redirected")
		w.WriteHeader(http.StatusFound)
	}))
	defer server.Close()
	collector := NewSub2APIHealthCollector(config.ServiceConfig{Name: "sub2api", BaseURL: server.URL}, config.EvidenceConfig{TimeoutSeconds: 1}).(*sub2apiHealthCollector)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	jar.SetCookies(u, []*http.Cookie{{Name: "session", Value: "secret"}})
	collector.httpClient.Jar = jar
	item := collector.Collect(context.Background(), Target{})
	if calls != 1 || item.Status != ItemError || !strings.Contains(item.Err, "302") {
		t.Fatalf("calls=%d item=%+v", calls, item)
	}
	collector.service.BaseURL = strings.Replace(server.URL, "://", "://user:secret@", 1)
	item = collector.Collect(context.Background(), Target{})
	if calls != 1 || item.Status != ItemError || strings.Contains(item.Err, "secret") {
		t.Fatalf("calls=%d item=%+v", calls, item)
	}
}

func TestCollectorSharedHealthExcerptIsBoundedAndSafe(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "token=private-token \x00\x1b"+strings.Repeat("x", 4096)+"TAIL")
	}))
	defer server.Close()
	item := NewSub2APIHealthCollector(config.ServiceConfig{Name: "sub2api", BaseURL: server.URL}, config.EvidenceConfig{TimeoutSeconds: 1}).Collect(context.Background(), Target{})
	if item.Status != ItemOK || len(item.Body) > 800 {
		t.Fatalf("item status=%s length=%d", item.Status, len(item.Body))
	}
	for _, forbidden := range []string{"private-token", "\\x00", "\\x1b", "TAIL"} {
		if strings.Contains(item.Body, forbidden) {
			t.Fatalf("unsafe or unbounded body: %q", item.Body)
		}
	}
}
