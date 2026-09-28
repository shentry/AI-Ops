package sub2api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func fakeAdmin(t *testing.T, handle func(w http.ResponseWriter, r *http.Request) any) (*httptest.Server, *[]string) {
	t.Helper()
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.RequestURI())
		if r.Header.Get("x-api-key") != "admin-key" {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 401, "message": "unauthorized"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "message": "success", "data": handle(w, r)})
	}))
	t.Cleanup(server.Close)
	return server, &calls
}

// The admin key has no read-only scope: a caller refused by its allowlist must
// not reach the network at all.
func TestClientRefusesEndpointsOutsideItsAllowlist(t *testing.T) {
	server, calls := fakeAdmin(t, func(http.ResponseWriter, *http.Request) any { return map[string]any{"id": 7, "schedulable": false} })
	client, err := New(server.URL, "admin-key", time.Second, ReadOnlyOps...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.SetSchedulable(context.Background(), 7, false); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("write through read-only client: %v", err)
	}
	if len(*calls) != 0 {
		t.Fatalf("refused call reached sub2api: %v", *calls)
	}
}

func TestClientDecodesOnlySchedulingFieldsAndChecksIdentity(t *testing.T) {
	server, calls := fakeAdmin(t, func(_ http.ResponseWriter, r *http.Request) any {
		if r.Method == http.MethodPost {
			var body map[string]bool
			_ = json.NewDecoder(r.Body).Decode(&body)
			return map[string]any{"id": 7, "schedulable": body["schedulable"], "credentials": map[string]any{"api_key": "sk-secret"}}
		}
		if strings.HasSuffix(r.URL.Path, "/accounts/8") {
			return map[string]any{"id": 9}
		}
		return map[string]any{"id": 7, "status": "active", "schedulable": true, "group_ids": []int64{2}, "credentials": map[string]any{"api_key": "sk-secret"}}
	})
	client, err := New(server.URL+"/", "admin-key", time.Second, GetAccount, SetSchedulable)
	if err != nil {
		t.Fatal(err)
	}
	account, err := client.Account(context.Background(), 7)
	if err != nil || !account.Schedulable || account.GroupIDs[0] != 2 {
		t.Fatalf("account = %+v %v", account, err)
	}
	if _, err := client.Account(context.Background(), 8); err == nil {
		t.Fatal("response for another account accepted")
	}
	if updated, err := client.SetSchedulable(context.Background(), 7, false); err != nil || updated.Schedulable {
		t.Fatalf("set = %+v %v", updated, err)
	}
	want := []string{"GET /api/v1/admin/accounts/7", "GET /api/v1/admin/accounts/8", "POST /api/v1/admin/accounts/7/schedulable"}
	if strings.Join(*calls, ",") != strings.Join(want, ",") {
		t.Fatalf("calls = %v", *calls)
	}
}

func TestClientReportsAPIErrorsWithoutTreatingThemAsData(t *testing.T) {
	server, _ := fakeAdmin(t, func(http.ResponseWriter, *http.Request) any { return nil })
	client, err := New(server.URL, "wrong-key", time.Second, OpsOverview)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Overview(context.Background(), "5m"); err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("unauthorized overview = %v", err)
	}
	for _, bad := range []string{"", "ftp://host", "http://user:pw@host", "http://host?x=1"} {
		if _, err := New(bad, "k", time.Second, OpsOverview); err == nil {
			t.Fatalf("base URL %q accepted", bad)
		}
	}
}

func TestProbeSendsOneTokenRequestAndReportsOnlyStatus(t *testing.T) {
	var got map[string]any
	var auth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&got)
		if r.URL.Path != "/v1/messages" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"content":[{"text":"user data"}]}`))
	}))
	defer server.Close()
	probe, err := NewProbe(server.URL, "/v1/messages", "probe-key", "cheap-model", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	result := probe.Run(context.Background())
	if !result.OK || result.StatusCode != 200 || strings.Contains(result.Detail, "user data") {
		t.Fatalf("result = %+v", result)
	}
	if auth != "Bearer probe-key" || got["model"] != "cheap-model" || got["max_tokens"] != float64(1) {
		t.Fatalf("request auth=%q body=%v", auth, got)
	}
	server.Close()
	if failed := probe.Run(context.Background()); failed.OK || failed.StatusCode != 0 {
		t.Fatalf("closed server probe = %+v", failed)
	}
}

func TestProbeRejectsFalseHTTP200Success(t *testing.T) {
	for body, ok := range map[string]bool{
		`<html>login</html>`:                      false,
		`{"error":{"message":"upstream failed"}}`: false,
		`{}`:                          false,
		`{"content":[]}`:              false,
		`{"content":[{"text":"ok"}]}`: true,
		`{"choices":[{"message":{"content":"ok"}}]}`: true,
	} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) }))
			defer server.Close()
			probe, err := NewProbe(server.URL, "/v1/messages", "fixture", "cheap", time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if result := probe.Run(context.Background()); result.OK != ok {
				t.Fatalf("result=%+v want success=%v", result, ok)
			}
		})
	}
}
