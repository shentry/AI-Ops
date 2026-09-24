package diagnose

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"oncall-agent/internal/incident"
)

func verificationSnapshot(base string) incident.ExecutionContext {
	return incident.ExecutionContext{SafetyLevel: "L2", Verification: incident.VerificationSpec{
		Kind: incident.HealthVerification, TargetName: "sub2api", BaseURL: base,
		MemberFingerprints: []string{"fp"}, IntervalSeconds: 10, WindowSeconds: 120, TimeoutSeconds: 5,
	}}
}

func verificationBinding(base string) incident.ExecutionBinding {
	return incident.ExecutionBinding{Container: "sub2api", BaseURL: base, AllowedContainers: []string{"sub2api"}, SafetyLevel: "L2"}
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
			got := NewVerifier(verificationBinding(server.URL)).Check(context.Background(), verificationSnapshot(server.URL), time.Minute)
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

func TestVerifierRejectsDriftAndUnsafeURLsWithoutRequest(t *testing.T) {
	for _, change := range []struct {
		name  string
		apply func(*incident.ExecutionContext, *incident.ExecutionBinding)
	}{
		{"address", func(_ *incident.ExecutionContext, b *incident.ExecutionBinding) { b.BaseURL = "http://new.invalid" }},
		{"container", func(_ *incident.ExecutionContext, b *incident.ExecutionBinding) { b.Container = "other" }},
		{"allowlist", func(_ *incident.ExecutionContext, b *incident.ExecutionBinding) { b.AllowedContainers = nil }},
		{"level", func(_ *incident.ExecutionContext, b *incident.ExecutionBinding) { b.SafetyLevel = "L3" }},
		{"credentials", func(s *incident.ExecutionContext, b *incident.ExecutionBinding) {
			s.Verification.BaseURL = "http://user:secret@host"
			b.BaseURL = s.Verification.BaseURL
		}},
		{"unsupported", func(s *incident.ExecutionContext, _ *incident.ExecutionBinding) { s.Verification.Kind = "other" }},
		{"dry_run", func(s *incident.ExecutionContext, _ *incident.ExecutionBinding) { s.DryRun = true }},
	} {
		t.Run(change.name, func(t *testing.T) {
			snapshot := verificationSnapshot("http://approved.invalid")
			binding := verificationBinding(snapshot.Verification.BaseURL)
			change.apply(&snapshot, &binding)
			v := NewVerifier(binding)
			v.httpClient.Transport = healthRoundTripFunc(func(*http.Request) (*http.Response, error) { t.Fatal("unexpected request"); return nil, nil })
			if got := v.Check(context.Background(), snapshot, time.Minute); got.Observation != "unavailable" {
				t.Fatalf("got %+v", got)
			}
		})
	}
}

func TestVerifierBoundsTimeoutBySnapshotAndRemainingWindow(t *testing.T) {
	for _, remaining := range []time.Duration{time.Minute, 2 * time.Second} {
		v := NewVerifier(verificationBinding("http://approved.invalid"))
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
		v := NewVerifier(verificationBinding("http://approved.invalid"))
		v.httpClient.Transport = transport
		got := v.Check(context.Background(), verificationSnapshot("http://approved.invalid"), time.Minute)
		if got.Observation != "unavailable" || strings.Contains(got.Detail, "private.internal") || strings.Contains(got.Detail, "secret") {
			t.Fatalf("got %+v", got)
		}
	}
}

func TestVerifierExpiredOrCanceledDoesNotRequest(t *testing.T) {
	v := NewVerifier(verificationBinding("http://approved.invalid"))
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

type healthRoundTripFunc func(*http.Request) (*http.Response, error)

func (f healthRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type brokenHealthBody struct{}

func (brokenHealthBody) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (brokenHealthBody) Close() error             { return nil }
