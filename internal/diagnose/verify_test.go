package diagnose

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"oncall-agent/internal/incident"
	"oncall-agent/internal/sub2api"
	"oncall-agent/internal/tools"
)

func checkOf(kind string, params any) incident.Check {
	raw, _ := json.Marshal(params)
	return incident.Check{Kind: kind, Params: raw}
}

// verificationSnapshot is a restart snapshot verified by the service health.
func verificationSnapshot(base string, checks ...incident.Check) incident.ExecutionContext {
	if len(checks) == 0 {
		checks = []incident.Check{checkOf(incident.CheckHealth, tools.HealthCheck{BaseURL: base})}
	}
	return incident.ExecutionContext{Version: incident.ExecutionContextVersion, Kind: incident.KindPrimary, Service: "sub2api",
		Rule: incident.RuleRef{ID: "restart", Version: "r1@000000000000", Mode: incident.ModeAuto, Alerts: []string{"Sub2APIDown"}}, ActionVersion: 2,
		Target: incident.Object{Kind: "container", Name: "sub2api", ID: "c0ffee"}, Revision: "started_at=x", PreState: json.RawMessage(`{}`),
		Members: []string{"fp"}, FaultAlert: "Sub2APIDown", ExpiresAt: time.Date(2026, 8, 24, 13, 0, 0, 0, time.UTC),
		Verification: incident.VerificationSpec{Checks: checks, IntervalSeconds: 10, WindowSeconds: 120, TimeoutSeconds: 5, RequiredPasses: 1}}
}

func TestVerifierDirectHealth(t *testing.T) {
	for _, status := range []int{200, 204, 299, 302, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path != "/health" || r.Method != http.MethodGet || r.Header.Get("Authorization") != "" {
					t.Errorf("unexpected request: %v", r)
				}
				w.Header().Set("Location", "/other")
				w.WriteHeader(status)
			}))
			defer server.Close()
			got := NewVerifier(tools.NewRegistry(), nil, nil).Check(context.Background(), verificationSnapshot(server.URL), time.Minute)
			want := "healthy"
			if status >= 300 {
				want = "unhealthy"
			}
			if got.Observation != want || calls != 1 {
				t.Fatalf("result=%+v calls=%d", got, calls)
			}
		})
	}
}

func TestVerifierRejectsUnsafeHealthURLWithoutRequest(t *testing.T) {
	for _, base := range []string{"http://user:secret@host", "http://host/health", "ftp://host", "not a url"} {
		v := NewVerifier(tools.NewRegistry(), nil, nil)
		v.httpClient.Transport = healthRoundTripFunc(func(*http.Request) (*http.Response, error) { t.Fatal("unexpected request"); return nil, nil })
		if got := v.Check(context.Background(), verificationSnapshot(base), time.Minute); got.Observation != "unavailable" || strings.Contains(got.Detail, "secret") {
			t.Fatalf("%s: got %+v", base, got)
		}
	}
}

func TestVerifierBoundsTimeoutBySnapshotAndRemainingWindow(t *testing.T) {
	for _, remaining := range []time.Duration{time.Minute, 2 * time.Second} {
		v := NewVerifier(tools.NewRegistry(), nil, nil)
		v.httpClient.Transport = healthRoundTripFunc(func(r *http.Request) (*http.Response, error) {
			deadline, ok := r.Context().Deadline()
			want := min(5*time.Second, remaining)
			if !ok || time.Until(deadline) > want || time.Until(deadline) < want-time.Second {
				t.Errorf("deadline=%v want duration=%v", deadline, want)
			}
			return nil, context.DeadlineExceeded
		})
		got := v.Check(context.Background(), verificationSnapshot("http://approved.invalid"), remaining)
		if got.Observation != "unavailable" || strings.Contains(got.Detail, "approved.invalid") {
			t.Fatalf("got %+v", got)
		}
	}
}

func TestVerifierConnectionAndReadErrorsAreUnavailable(t *testing.T) {
	for _, transport := range []healthRoundTripFunc{
		func(*http.Request) (*http.Response, error) {
			return nil, errors.New("secret response from http://private.internal")
		},
		func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: brokenHealthBody{}}, nil
		},
	} {
		v := NewVerifier(tools.NewRegistry(), nil, nil)
		v.httpClient.Transport = transport
		got := v.Check(context.Background(), verificationSnapshot("http://approved.invalid"), time.Minute)
		if got.Observation != "unavailable" || strings.Contains(got.Detail, "private.internal") || strings.Contains(got.Detail, "secret") {
			t.Fatalf("got %+v", got)
		}
	}
}

func TestVerifierExpiredOrCanceledDoesNotRequest(t *testing.T) {
	v := NewVerifier(tools.NewRegistry(), nil, nil)
	v.httpClient.Transport = healthRoundTripFunc(func(*http.Request) (*http.Response, error) { t.Fatal("unexpected request"); return nil, nil })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tc := range []struct {
		ctx       context.Context
		remaining time.Duration
	}{{ctx, time.Minute}, {context.Background(), 0}} {
		if got := v.Check(tc.ctx, verificationSnapshot("http://approved.invalid"), tc.remaining); got.Observation != "unavailable" {
			t.Fatalf("got %+v", got)
		}
	}
}

func TestVerifierContainerCheck(t *testing.T) {
	started := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	live := tools.ContainerInspect{ID: "c0ffee", Name: "sub2api", Status: "running", Running: true, StartedAt: started.Add(time.Minute), RepoDigests: []string{"repo@sha256:good"}}
	for name, test := range map[string]struct {
		check  tools.ContainerCheck
		live   func(*tools.ContainerInspect)
		fail   bool
		want   string
		detail string
	}{
		"restarted":           {check: tools.ContainerCheck{Name: "sub2api", ID: "c0ffee", StartedAfter: started}, want: "healthy"},
		"approved digest":     {check: tools.ContainerCheck{Name: "sub2api", RepoDigest: "sha256:good"}, want: "healthy"},
		"other instance":      {check: tools.ContainerCheck{Name: "sub2api", ID: "other"}, want: "unhealthy", detail: "different instance"},
		"exited":              {check: tools.ContainerCheck{Name: "sub2api"}, live: func(c *tools.ContainerInspect) { c.Running, c.Status = false, "exited" }, want: "unhealthy"},
		"restarting":          {check: tools.ContainerCheck{Name: "sub2api"}, live: func(c *tools.ContainerInspect) { c.Restarting = true }, want: "unhealthy"},
		"not restarted":       {check: tools.ContainerCheck{Name: "sub2api", StartedAfter: started.Add(time.Hour)}, want: "unhealthy", detail: "has not restarted"},
		"other image":         {check: tools.ContainerCheck{Name: "sub2api", RepoDigest: "sha256:bad"}, want: "unhealthy", detail: "approved image"},
		"inspect unavailable": {check: tools.ContainerCheck{Name: "sub2api"}, fail: true, want: "unavailable"},
	} {
		t.Run(name, func(t *testing.T) {
			current := live
			if test.live != nil {
				test.live(&current)
			}
			registry := stubRegistry(t, map[string]tools.Handler{tools.ToolDockerInspect: func(context.Context, json.RawMessage) (string, error) {
				if test.fail {
					return "", errors.New("docker unavailable")
				}
				raw, _ := json.Marshal(current)
				return string(raw), nil
			}})
			got := NewVerifier(registry, nil, nil).Check(context.Background(), verificationSnapshot("", checkOf(incident.CheckContainer, test.check)), time.Minute)
			if got.Observation != test.want || !strings.Contains(got.Detail, test.detail) {
				t.Fatalf("got %+v, want %s containing %q", got, test.want, test.detail)
			}
		})
	}
}

func promScalars(t *testing.T, values map[string]string, fail bool) *tools.Registry {
	return stubRegistry(t, map[string]tools.Handler{tools.ToolPromInstantQuery: func(_ context.Context, raw json.RawMessage) (string, error) {
		if fail {
			return "", errors.New("prometheus down")
		}
		var args map[string]string
		_ = json.Unmarshal(raw, &args)
		if strings.Contains(args["query"], `sub2api_ops_up{endpoint="overview"}`) {
			return fmt.Sprintf(`{"resultType":"vector","result":[{"value":[%d,"1"]}]}`, time.Now().Unix()), nil
		}
		for fragment, value := range values {
			if strings.Contains(args["query"], fragment) {
				return fmt.Sprintf(`{"resultType":"vector","result":[{"value":[%d,%q]}]}`, time.Now().Unix(), value), nil
			}
		}
		return `{"resultType":"vector","result":[]}`, nil
	}})
}

// Too little real traffic is not a pass: recovery must be seen in requests.
func TestVerifierErrorRatioNeedsRealTraffic(t *testing.T) {
	check := checkOf(incident.CheckErrorRatio, tools.ErrorRatioCheck{MaxRatio: 0.05, MinRequests: 50})
	for name, test := range map[string]struct {
		values map[string]string
		fail   bool
		want   string
	}{
		"recovered":     {map[string]string{"requests": "200", "errors": "2"}, false, "healthy"},
		"still failing": {map[string]string{"requests": "200", "errors": "40"}, false, "unhealthy"},
		"too few":       {map[string]string{"requests": "10", "errors": "0"}, false, "unavailable"},
		"no sample":     {map[string]string{"errors": "0"}, false, "unavailable"},
		"query failed":  {nil, true, "unavailable"},
	} {
		got := NewVerifier(promScalars(t, test.values, test.fail), nil, nil).Check(context.Background(), verificationSnapshot("", check), time.Minute)
		if got.Observation != test.want {
			t.Fatalf("%s: got %+v, want %s", name, got, test.want)
		}
	}
}

type fakeAccounts struct {
	account      sub2api.Account
	availability sub2api.Availability
	err          error
}

func (f fakeAccounts) Account(_ context.Context, id int64) (sub2api.Account, error) {
	account := f.account
	account.ID = id
	return account, f.err
}

func (f fakeAccounts) Availability(context.Context) (sub2api.Availability, error) {
	return f.availability, f.err
}

func TestVerifierAccountAndGroupChecks(t *testing.T) {
	quarantined := checkOf(incident.CheckAccount, tools.AccountCheck{AccountID: 7, Schedulable: false})
	capacity := checkOf(incident.CheckGroupAvailable, tools.GroupAvailableCheck{GroupIDs: []int64{2}, Min: 2})
	groups := func(available int64) sub2api.Availability {
		return sub2api.Availability{Enabled: true, Groups: map[string]sub2api.GroupAvailability{"2": {GroupID: 2, AvailableCount: available}}}
	}
	for name, test := range map[string]struct {
		reader AccountReader
		checks []incident.Check
		want   string
	}{
		"quarantine applied":      {fakeAccounts{account: sub2api.Account{Schedulable: false}, availability: groups(3)}, []incident.Check{quarantined, capacity}, "healthy"},
		"quarantine reverted":     {fakeAccounts{account: sub2api.Account{Schedulable: true}, availability: groups(3)}, []incident.Check{quarantined}, "unhealthy"},
		"group below floor":       {fakeAccounts{availability: groups(1)}, []incident.Check{capacity}, "unhealthy"},
		"group not reported":      {fakeAccounts{availability: sub2api.Availability{Enabled: true}}, []incident.Check{capacity}, "unavailable"},
		"realtime disabled":       {fakeAccounts{availability: sub2api.Availability{}}, []incident.Check{capacity}, "unavailable"},
		"admin api down":          {fakeAccounts{err: errors.New("502")}, []incident.Check{quarantined}, "unavailable"},
		"no admin key":            {nil, []incident.Check{quarantined}, "unavailable"},
		"unavailable dominates":   {fakeAccounts{account: sub2api.Account{Schedulable: true}, availability: sub2api.Availability{Enabled: true}}, []incident.Check{quarantined, capacity}, "unavailable"},
		"unhealthy beats healthy": {fakeAccounts{account: sub2api.Account{Schedulable: true}, availability: groups(3)}, []incident.Check{capacity, quarantined}, "unhealthy"},
	} {
		got := NewVerifier(tools.NewRegistry(), test.reader, nil).Check(context.Background(), verificationSnapshot("", test.checks...), time.Minute)
		if got.Observation != test.want {
			t.Fatalf("%s: got %+v, want %s", name, got, test.want)
		}
	}
}

func TestVerifierBusinessProbe(t *testing.T) {
	check := checkOf(incident.CheckProbe, struct{}{})
	if got := NewVerifier(tools.NewRegistry(), nil, nil).Check(context.Background(), verificationSnapshot("", check), time.Minute); got.Observation != "unavailable" {
		t.Fatalf("unconfigured probe = %+v", got)
	}
	for status, want := range map[int]string{200: "healthy", 529: "unhealthy"} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			fmt.Fprint(w, `{"content":[{"text":"ok"}]}`)
		}))
		probe, err := sub2api.NewProbe(server.URL, "/v1/messages", "probe-key", "cheap-model", 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		got := NewVerifier(tools.NewRegistry(), nil, probe).Check(context.Background(), verificationSnapshot("", check), time.Minute)
		server.Close()
		if got.Observation != want {
			t.Fatalf("status %d: got %+v, want %s", status, got, want)
		}
	}
	unreachable, err := sub2api.NewProbe("http://127.0.0.1:1", "/v1/messages", "probe-key", "cheap-model", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got := NewVerifier(tools.NewRegistry(), nil, unreachable).Check(context.Background(), verificationSnapshot("", check), time.Minute); got.Observation != "unavailable" {
		t.Fatalf("unreachable probe = %+v", got)
	}
}

type healthRoundTripFunc func(*http.Request) (*http.Response, error)

func (f healthRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type brokenHealthBody struct{}

func (brokenHealthBody) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (brokenHealthBody) Close() error             { return nil }
