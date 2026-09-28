package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"oncall-agent/internal/sub2api"
)

type fakeOps struct {
	overviewErr error
	upstream    sub2api.UpstreamErrors
}

func (f fakeOps) Overview(context.Context, string) (sub2api.Overview, error) {
	p95 := 1500
	return sub2api.Overview{RequestCountTotal: 120, RequestCountSLA: 100, ErrorCountSLA: 30, ErrorCountTotal: 35, Upstream429Count: 4, Duration: sub2api.Latency{P95: &p95}}, f.overviewErr
}

func (f fakeOps) Availability(context.Context) (sub2api.Availability, error) {
	until := time.Now().Add(time.Minute)
	return sub2api.Availability{Enabled: true,
		Groups: map[string]sub2api.GroupAvailability{"2": {GroupID: 2, GroupName: `main "pool"`, TotalAccounts: 3, AvailableCount: 2}},
		Accounts: map[string]sub2api.AccountAvailability{
			"7": {AccountID: 7, GroupID: 2, IsAvailable: false, TempUnschedulableUntil: &until},
			"8": {AccountID: 8, GroupID: 2, IsAvailable: true},
		}}, nil
}

func (f fakeOps) UpstreamErrors(context.Context, string, int) (sub2api.UpstreamErrors, error) {
	return f.upstream, nil
}

func scrape(t *testing.T, ops opsReader) string {
	t.Helper()
	resp := httptest.NewRecorder()
	probes := &probeState{result: &sub2api.ProbeResult{OK: true, Latency: 800 * time.Millisecond}, at: time.Unix(1700000000, 0)}
	handler(ops, probes).ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return resp.Body.String()
}

func TestExporterPublishesBusinessSignals(t *testing.T) {
	seven, eight, two := int64(7), int64(8), int64(2)
	body := scrape(t, fakeOps{upstream: sub2api.UpstreamErrors{Total: 3, Items: []sub2api.UpstreamError{
		{AccountID: &seven, GroupID: &two}, {AccountID: &seven, GroupID: &two}, {AccountID: &eight, GroupID: &two}, {AccountID: nil},
	}}})
	for _, want := range []string{
		`sub2api_ops_up{endpoint="overview"} 1`,
		`sub2api_requests_5m{class="sla"} 100`,
		`sub2api_errors_5m{class="sla"} 30`,
		`sub2api_upstream_errors_5m{status="429"} 4`,
		`sub2api_request_duration_p95_seconds_5m 1.5`,
		`sub2api_group_accounts{group_id="2",group_name="main \"pool\"",state="available"} 2`,
		`sub2api_account_available{account_id="7",group_id="2"} 0`,
		`sub2api_account_temp_unschedulable{account_id="7",group_id="2"} 1`,
		`sub2api_account_upstream_errors_5m{account_id="7",group_id="2"} 2`,
		`sub2api_account_upstream_errors_5m{account_id="8",group_id="2"} 1`,
		`sub2api_upstream_errors_sampled 0`,
		`sub2api_probe_success 1`,
		`sub2api_probe_duration_seconds 0.8`,
		`sub2api_probe_timestamp_seconds 1.7e+09`,
		"# TYPE sub2api_ops_up gauge",
	} {
		if !strings.Contains(body, want+"\n") && !strings.Contains(body, want) {
			t.Fatalf("missing %q in:\n%s", want, body)
		}
	}
}

// A failed read is "observation unavailable", never zero traffic or zero errors.
func TestExporterMarksFailedReadsUnavailable(t *testing.T) {
	body := scrape(t, fakeOps{overviewErr: errors.New("timeout"), upstream: sub2api.UpstreamErrors{Total: 900, Items: make([]sub2api.UpstreamError, 500)}})
	if !strings.Contains(body, `sub2api_ops_up{endpoint="overview"} 0`) || strings.Contains(body, "sub2api_requests_5m") || strings.Contains(body, "sub2api_errors_5m") {
		t.Fatalf("failed overview exported as data:\n%s", body)
	}
	if !strings.Contains(body, "sub2api_upstream_errors_sampled 1") {
		t.Fatalf("truncated page not flagged:\n%s", body)
	}
}
