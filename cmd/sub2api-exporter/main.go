// sub2api-exporter turns sub2api's read-only ops API into Prometheus metrics.
// sub2api has no /metrics endpoint and no reverse proxy fronts it, so this is
// the only source of real-traffic business signals (error rate, per-upstream
// errors, account availability). Every scrape reads fresh data; a failed read
// is exported as sub2api_ops_up=0, never as zero traffic.
package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"oncall-agent/internal/sub2api"
)

// window is the trailing range every *_5m metric covers; sub2api accepts only
// fixed ranges. upstreamPageSize is sub2api's page cap for upstream errors.
// minProbeInterval bounds what the business probe can cost.
const (
	window           = "5m"
	upstreamPageSize = 500
	minProbeInterval = time.Minute
)

func main() {
	listen := envOr("LISTEN_ADDR", ":9122")
	base := os.Getenv("SUB2API_BASE_URL")
	client, err := sub2api.New(base, os.Getenv("SUB2API_ADMIN_API_KEY"), 10*time.Second,
		sub2api.OpsOverview, sub2api.OpsAvailability, sub2api.OpsUpstreamErrors)
	if err != nil {
		log.Fatalf("sub2api-exporter: %v", err)
	}
	probes := &probeState{}
	// The business probe is optional: it needs a dedicated test key and a
	// cheap model, and runs on its own schedule, never once per scrape.
	if key := os.Getenv("SUB2API_PROBE_API_KEY"); key != "" {
		probe, err := sub2api.NewProbe(base, envOr("SUB2API_PROBE_PATH", "/v1/messages"), key, os.Getenv("SUB2API_PROBE_MODEL"), 30*time.Second)
		if err != nil {
			log.Fatalf("sub2api-exporter: %v", err)
		}
		interval, err := time.ParseDuration(envOr("SUB2API_PROBE_INTERVAL", "5m"))
		if err != nil || interval < minProbeInterval {
			log.Fatalf("sub2api-exporter: SUB2API_PROBE_INTERVAL must be a duration of at least %s", minProbeInterval)
		}
		go probes.run(probe, interval)
	}
	http.Handle("/metrics", handler(client, probes))
	log.Printf("sub2api-exporter: listening on %s", listen)
	server := &http.Server{Addr: listen, ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(server.ListenAndServe())
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

type opsReader interface {
	Overview(context.Context, string) (sub2api.Overview, error)
	Availability(context.Context) (sub2api.Availability, error)
	UpstreamErrors(context.Context, string, int) (sub2api.UpstreamErrors, error)
}

// probeState holds the latest business probe result between scrapes.
type probeState struct {
	mu     sync.Mutex
	result *sub2api.ProbeResult
	at     time.Time
}

func (p *probeState) run(probe *sub2api.Probe, interval time.Duration) {
	for {
		result := probe.Run(context.Background())
		p.mu.Lock()
		p.result, p.at = &result, time.Now()
		p.mu.Unlock()
		time.Sleep(interval)
	}
}

func (p *probeState) samples() []sample {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.result == nil {
		return nil
	}
	return []sample{
		{name: "sub2api_probe_success", value: boolValue(p.result.OK)},
		{name: "sub2api_probe_duration_seconds", value: p.result.Latency.Seconds()},
		{name: "sub2api_probe_timestamp_seconds", value: float64(p.at.Unix())},
	}
}

func handler(client opsReader, probes *probeState) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		writeMetrics(w, append(collect(ctx, client), probes.samples()...))
	})
}

type sample struct {
	name   string
	labels [][2]string
	value  float64
}

func collect(ctx context.Context, client opsReader) []sample {
	var out []sample
	add := func(name string, value float64, labels ...[2]string) {
		out = append(out, sample{name: name, labels: labels, value: value})
	}
	up := func(endpoint string, err error) {
		value := 1.0
		if err != nil {
			value = 0
			log.Printf("sub2api-exporter: %s: %v", endpoint, err)
		}
		add("sub2api_ops_up", value, [2]string{"endpoint", endpoint})
	}

	overview, err := client.Overview(ctx, window)
	up("overview", err)
	if err == nil {
		add("sub2api_requests_5m", float64(overview.RequestCountTotal), [2]string{"class", "all"})
		add("sub2api_requests_5m", float64(overview.RequestCountSLA), [2]string{"class", "sla"})
		add("sub2api_errors_5m", float64(overview.ErrorCountTotal), [2]string{"class", "all"})
		add("sub2api_errors_5m", float64(overview.ErrorCountSLA), [2]string{"class", "sla"})
		add("sub2api_errors_5m", float64(overview.BusinessLimitedCount), [2]string{"class", "business_limited"})
		add("sub2api_upstream_errors_5m", float64(overview.Upstream429Count), [2]string{"status", "429"})
		add("sub2api_upstream_errors_5m", float64(overview.Upstream529Count), [2]string{"status", "529"})
		add("sub2api_upstream_errors_5m", float64(overview.UpstreamErrorCountExcl429529), [2]string{"status", "other"})
		if overview.Duration.P95 != nil {
			add("sub2api_request_duration_p95_seconds_5m", float64(*overview.Duration.P95)/1000)
		}
	}

	availability, err := client.Availability(ctx)
	up("account_availability", err)
	if err == nil {
		enabled := 0.0
		if availability.Enabled {
			enabled = 1
		}
		add("sub2api_ops_realtime_enabled", enabled)
		for _, group := range availability.Groups {
			id := [2]string{"group_id", strconv.FormatInt(group.GroupID, 10)}
			name := [2]string{"group_name", group.GroupName}
			add("sub2api_group_accounts", float64(group.TotalAccounts), id, name, [2]string{"state", "total"})
			add("sub2api_group_accounts", float64(group.AvailableCount), id, name, [2]string{"state", "available"})
			add("sub2api_group_accounts", float64(group.RateLimitCount), id, name, [2]string{"state", "rate_limited"})
			add("sub2api_group_accounts", float64(group.ErrorCount), id, name, [2]string{"state", "error"})
		}
		now := time.Now()
		for _, account := range availability.Accounts {
			labels := [][2]string{{"account_id", strconv.FormatInt(account.AccountID, 10)}, {"group_id", strconv.FormatInt(account.GroupID, 10)}}
			add("sub2api_account_available", boolValue(account.IsAvailable), labels...)
			add("sub2api_account_temp_unschedulable", boolValue(account.TempUnschedulableUntil != nil && now.Before(*account.TempUnschedulableUntil)), labels...)
		}
	}

	upstream, err := client.UpstreamErrors(ctx, window, upstreamPageSize)
	up("upstream_errors", err)
	if err == nil {
		type key struct{ account, group int64 }
		counts := map[key]int{}
		for _, item := range upstream.Items {
			if item.AccountID == nil {
				continue
			}
			k := key{account: *item.AccountID}
			if item.GroupID != nil {
				k.group = *item.GroupID
			}
			counts[k]++
		}
		for k, count := range counts {
			add("sub2api_account_upstream_errors_5m", float64(count),
				[2]string{"account_id", strconv.FormatInt(k.account, 10)}, [2]string{"group_id", strconv.FormatInt(k.group, 10)})
		}
		// A partial page undercounts per-account errors; alerts must know.
		add("sub2api_upstream_errors_sampled", boolValue(upstream.Total > int64(len(upstream.Items))))
	}
	return out
}

func boolValue(value bool) float64 {
	if value {
		return 1
	}
	return 0
}

var help = map[string]string{
	"sub2api_ops_up":                          "Whether the last read of a sub2api ops endpoint succeeded.",
	"sub2api_ops_realtime_enabled":            "Whether sub2api realtime account monitoring is enabled.",
	"sub2api_requests_5m":                     "Requests in the trailing 5 minutes.",
	"sub2api_errors_5m":                       "Error responses in the trailing 5 minutes.",
	"sub2api_upstream_errors_5m":              "Upstream provider errors in the trailing 5 minutes.",
	"sub2api_request_duration_p95_seconds_5m": "p95 request duration in the trailing 5 minutes.",
	"sub2api_group_accounts":                  "Accounts of a group by scheduling state.",
	"sub2api_account_available":               "Whether an upstream account is currently schedulable.",
	"sub2api_account_temp_unschedulable":      "Whether sub2api has temporarily unscheduled an account.",
	"sub2api_account_upstream_errors_5m":      "Upstream errors attributed to an account in the trailing 5 minutes.",
	"sub2api_upstream_errors_sampled":         "1 when per-account upstream errors come from a truncated page.",
	"sub2api_probe_success":                   "Whether the last business probe got HTTP 200.",
	"sub2api_probe_duration_seconds":          "Duration of the last business probe.",
	"sub2api_probe_timestamp_seconds":         "Unix time of the last business probe.",
}

func writeMetrics(w io.Writer, samples []sample) {
	sort.SliceStable(samples, func(i, j int) bool { return samples[i].name < samples[j].name })
	previous := ""
	for _, s := range samples {
		if s.name != previous {
			fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n", s.name, help[s.name], s.name)
			previous = s.name
		}
		fmt.Fprint(w, s.name)
		if len(s.labels) > 0 {
			parts := make([]string, len(s.labels))
			for i, label := range s.labels {
				parts[i] = label[0] + `="` + labelEscaper.Replace(label[1]) + `"`
			}
			fmt.Fprint(w, "{"+strings.Join(parts, ",")+"}")
		}
		fmt.Fprintf(w, " %s\n", strconv.FormatFloat(s.value, 'g', -1, 64))
	}
}

var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
