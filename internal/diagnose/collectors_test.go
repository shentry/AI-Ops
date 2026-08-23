package diagnose

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"oncall-agent/internal/config"
	"oncall-agent/internal/store"
	"oncall-agent/internal/tools"
)

// stubRegistry 用假 handler 装一个真 Registry：超时/截断纪律走真实现，
// 外部调用走桩。
func stubRegistry(t *testing.T, handlers map[string]tools.Handler) *tools.Registry {
	t.Helper()
	registry := tools.NewRegistry()
	for name, handler := range handlers {
		if err := registry.Register(tools.ToolSpec{
			Name: name, Description: "stub", Level: tools.L1ReadOnly,
			Timeout: time.Second, MaxOutput: 1024, Handler: handler,
		}); err != nil {
			t.Fatal(err)
		}
	}
	return registry
}

func TestSnapshotCollectorRendersMembersAndAlerts(t *testing.T) {
	target := Target{
		Incident: store.Incident{ID: 9, GroupKey: "payments", Status: "firing", Severity: 5, AlertsCount: 1,
			StartedAt: time.Date(2026, 8, 19, 2, 0, 0, 0, time.UTC), LastSeenAt: time.Date(2026, 8, 19, 2, 1, 0, 0, time.UTC)},
		Members: []store.IncidentMember{{Fingerprint: "fp1", Name: "HighCPU", Status: "firing", Severity: 5}},
		Alerts: []store.Alert{{
			Fingerprint: "fp1", Name: "HighCPU", Status: "firing", Severity: 5,
			Labels:       []byte(`{"alertname":"HighCPU","token":"abc123"}`),
			Annotations:  []byte(`{"summary":"cpu hot"}`),
			GeneratorURL: "http://prom/graph?g0.expr=up",
		}},
	}
	item := NewSnapshotCollector().Collect(context.Background(), target)
	if item.Status != ItemOK {
		t.Fatalf("status = %s, err = %s", item.Status, item.Err)
	}
	for _, want := range []string{"group_key=payments", "fingerprint=fp1", "cpu hot", "generator_url"} {
		if !strings.Contains(item.Body, want) {
			t.Fatalf("snapshot body missing %q:\n%s", want, item.Body)
		}
	}
	// labels 里的 token 必须被脱敏。
	if strings.Contains(item.Body, "abc123") {
		t.Fatalf("snapshot leaked secret:\n%s", item.Body)
	}
}

func TestExtractGeneratorExpr(t *testing.T) {
	expr, err := ExtractGeneratorExpr("http://127.0.0.1:9090/graph?g0.expr=up%7Bjob%3D%22x%22%7D&g0.tab=1")
	if err != nil || expr != `up{job="x"}` {
		t.Fatalf("expr = %q, err = %v", expr, err)
	}
	if expr, _ := ExtractGeneratorExpr(""); expr != "" {
		t.Fatalf("empty url expr = %q", expr)
	}
	if _, err := ExtractGeneratorExpr("http://x/graph?g0.tab=1"); err == nil {
		t.Fatal("missing g0.expr should error")
	}
}

func TestPromReplayCollectorMissingGeneratorURL(t *testing.T) {
	registry := stubRegistry(t, nil)
	collector := NewPromReplayCollector(registry, 15)
	item := collector.Collect(context.Background(), Target{Incident: store.Incident{ID: 1}})
	if item.Status != ItemMissing {
		t.Fatalf("status = %s, want missing", item.Status)
	}
}

func TestPromReplayCollectorQueriesRange(t *testing.T) {
	var gotArgs map[string]string
	registry := stubRegistry(t, map[string]tools.Handler{
		tools.ToolPromRangeQuery: func(_ context.Context, raw json.RawMessage) (string, error) {
			if err := json.Unmarshal(raw, &gotArgs); err != nil {
				return "", err
			}
			return `{"resultType":"matrix"}`, nil
		},
	})
	startedAt := time.Date(2026, 8, 19, 3, 0, 0, 0, time.UTC)
	target := Target{
		Incident: store.Incident{ID: 1, StartedAt: startedAt},
		Alerts:   []store.Alert{{Name: "A", GeneratorURL: "http://prom/graph?g0.expr=up"}},
	}
	item := NewPromReplayCollector(registry, 15).Collect(context.Background(), target)
	if item.Status != ItemOK {
		t.Fatalf("status = %s, err = %s", item.Status, item.Err)
	}
	// range_minutes=15 → 半窗口钳制为 7.5 分钟，保证 [start,end] 不被工具截断。
	half := 7*time.Minute + 30*time.Second
	if gotArgs["query"] != "up" || gotArgs["start"] != startedAt.Add(-half).Format(time.RFC3339) || gotArgs["end"] != startedAt.Add(half).Format(time.RFC3339) {
		t.Fatalf("range args = %v", gotArgs)
	}
	if !strings.Contains(item.Body, "matrix") {
		t.Fatalf("body = %s", item.Body)
	}
}

// 窗口按每条告警自己的 firing 时刻算，不是 incident 的 StartedAt：
// 晚 20 分钟才 firing 的成员，用 incident 时刻算窗口会完全错过触发现场。
func TestPromReplayCollectorUsesPerAlertFiringTime(t *testing.T) {
	windows := map[string][2]string{}
	registry := stubRegistry(t, map[string]tools.Handler{
		tools.ToolPromRangeQuery: func(_ context.Context, raw json.RawMessage) (string, error) {
			var args map[string]string
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			windows[args["query"]] = [2]string{args["start"], args["end"]}
			return `{"resultType":"matrix"}`, nil
		},
	})
	incidentStart := time.Date(2026, 8, 19, 3, 0, 0, 0, time.UTC)
	lateFiring := incidentStart.Add(20 * time.Minute)
	target := Target{
		Incident: store.Incident{ID: 1, StartedAt: incidentStart},
		Alerts: []store.Alert{
			{Name: "First", GeneratorURL: "http://prom/graph?g0.expr=up", StartsAt: incidentStart},
			{Name: "Late", GeneratorURL: "http://prom/graph?g0.expr=down", StartsAt: lateFiring},
		},
	}
	item := NewPromReplayCollector(registry, 15).Collect(context.Background(), target)
	if item.Status != ItemOK {
		t.Fatalf("status = %s, err = %s", item.Status, item.Err)
	}
	half := 7*time.Minute + 30*time.Second
	if got := windows["down"]; got[0] != lateFiring.Add(-half).Format(time.RFC3339) || got[1] != lateFiring.Add(half).Format(time.RFC3339) {
		t.Fatalf("late alert window = %v, want centered on its own firing time %s", got, lateFiring)
	}
	if got := windows["up"]; got[0] != incidentStart.Add(-half).Format(time.RFC3339) {
		t.Fatalf("first alert window = %v", got)
	}
	// 正文里带 firing 时刻，回放时能核对窗口是怎么来的。
	if !strings.Contains(item.Body, "firing_at "+lateFiring.Format(time.RFC3339)) {
		t.Fatalf("body missing firing time:\n%s", item.Body)
	}
}

// 同一 expr 来自多条告警时取最早的 firing 时刻（第一次触发的现场）。
func TestPromReplayCollectorTakesEarliestFiringPerExpr(t *testing.T) {
	var gotStart string
	registry := stubRegistry(t, map[string]tools.Handler{
		tools.ToolPromRangeQuery: func(_ context.Context, raw json.RawMessage) (string, error) {
			var args map[string]string
			_ = json.Unmarshal(raw, &args)
			gotStart = args["start"]
			return `{"resultType":"matrix"}`, nil
		},
	})
	early := time.Date(2026, 8, 19, 3, 0, 0, 0, time.UTC)
	target := Target{
		Incident: store.Incident{ID: 1, StartedAt: early},
		Alerts: []store.Alert{
			{Name: "B", GeneratorURL: "http://prom/graph?g0.expr=up", StartsAt: early.Add(10 * time.Minute)},
			{Name: "A", GeneratorURL: "http://prom/graph?g0.expr=up", StartsAt: early},
		},
	}
	NewPromReplayCollector(registry, 15).Collect(context.Background(), target)
	half := 7*time.Minute + 30*time.Second
	if gotStart != early.Add(-half).Format(time.RFC3339) {
		t.Fatalf("start = %s, want window around the earliest firing %s", gotStart, early)
	}
}

func TestGoldenMetricsCollectorPartialFailure(t *testing.T) {
	calls := 0
	registry := stubRegistry(t, map[string]tools.Handler{
		tools.ToolPromInstantQuery: func(_ context.Context, raw json.RawMessage) (string, error) {
			calls++
			var args map[string]string
			_ = json.Unmarshal(raw, &args)
			if strings.Contains(args["query"], "node_cpu") {
				return "", fmt.Errorf("prometheus down")
			}
			return `{"resultType":"vector","result":[{"value":[1,"42"]}]}`, nil
		},
	})
	item := NewGoldenMetricsCollector(registry).Collect(context.Background(), Target{})
	if item.Status != ItemOK {
		t.Fatalf("status = %s, want ok with partial failure recorded", item.Status)
	}
	if !strings.Contains(item.Body, "cpu_usage_percent") || !strings.Contains(item.Body, "error:") {
		t.Fatalf("body = %s", item.Body)
	}
	if calls != 5 {
		t.Fatalf("queries = %d, want 5", calls)
	}
}

func TestSub2APICollectorMissingConfig(t *testing.T) {
	collector := NewSub2APICollector(config.EvidenceConfig{}, stubRegistry(t, nil))
	item := collector.Collect(context.Background(), Target{})
	if item.Status != ItemMissing {
		t.Fatalf("status = %s, want missing", item.Status)
	}
}

func TestSub2APICollectorHealthAndMetrics(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			t.Errorf("path = %s", r.URL.Path)
		}
		fmt.Fprint(w, `{"status":"ok"}`)
	}))
	defer server.Close()
	registry := stubRegistry(t, map[string]tools.Handler{
		tools.ToolPromInstantQuery: func(_ context.Context, raw json.RawMessage) (string, error) {
			return `{"resultType":"vector","result":[]}`, nil
		},
	})
	cfg := config.EvidenceConfig{Sub2APIBaseURL: server.URL, Sub2APIMetricsJob: "sub2api", TimeoutSeconds: 5}
	item := NewSub2APICollector(cfg, registry).Collect(context.Background(), Target{})
	if item.Status != ItemOK {
		t.Fatalf("status = %s, err = %s", item.Status, item.Err)
	}
	for _, want := range []string{"status=200", "request_rate_5m", "error_5xx_rate_5m", "latency_p99_5m"} {
		if !strings.Contains(item.Body, want) {
			t.Fatalf("body missing %q:\n%s", want, item.Body)
		}
	}
}

// /health 返回 5xx 时采集状态必须是 error，不能报 ok ——
// 否则下游会把"网关挂了"的证据当成"网关正常"的证据读。正文照样保留。
func TestSub2APICollectorUnhealthyMarksError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"status":"degraded"}`)
	}))
	defer server.Close()
	registry := stubRegistry(t, map[string]tools.Handler{
		tools.ToolPromInstantQuery: func(context.Context, json.RawMessage) (string, error) {
			return `{"resultType":"vector","result":[]}`, nil
		},
	})
	cfg := config.EvidenceConfig{Sub2APIBaseURL: server.URL, Sub2APIMetricsJob: "sub2api", TimeoutSeconds: 5}
	item := NewSub2APICollector(cfg, registry).Collect(context.Background(), Target{})
	if item.Status != ItemError {
		t.Fatalf("status = %s, want error for HTTP 503", item.Status)
	}
	if !strings.Contains(item.Err, "503") {
		t.Fatalf("err = %q, want the status code", item.Err)
	}
	// 正文不能丢：503 的响应体和旁边的指标正是要看的东西。
	for _, want := range []string{"status=503", "degraded", "request_rate_5m"} {
		if !strings.Contains(item.Body, want) {
			t.Fatalf("body missing %q:\n%s", want, item.Body)
		}
	}
}

// 连不上（数据源配置了但不可达）同样是 error，不是 ok。
func TestSub2APICollectorUnreachableMarksError(t *testing.T) {
	registry := stubRegistry(t, map[string]tools.Handler{
		tools.ToolPromInstantQuery: func(context.Context, json.RawMessage) (string, error) {
			return `{"resultType":"vector","result":[]}`, nil
		},
	})
	cfg := config.EvidenceConfig{Sub2APIBaseURL: "http://127.0.0.1:1", Sub2APIMetricsJob: "sub2api", TimeoutSeconds: 1}
	item := NewSub2APICollector(cfg, registry).Collect(context.Background(), Target{})
	if item.Status != ItemError {
		t.Fatalf("status = %s, want error for unreachable gateway", item.Status)
	}
}

func TestDockerCollectorReportsToolErrors(t *testing.T) {
	registry := stubRegistry(t, map[string]tools.Handler{
		tools.ToolDockerInspect: func(context.Context, json.RawMessage) (string, error) {
			return `{"status":"running"}`, nil
		},
		tools.ToolDockerLogs: func(context.Context, json.RawMessage) (string, error) {
			return "", fmt.Errorf("container not found")
		},
	})
	cfg := config.EvidenceConfig{DockerContainer: "sub2api", LogMaxLines: 50}
	target := Target{Incident: store.Incident{ID: 1, StartedAt: time.Date(2026, 8, 19, 4, 0, 0, 0, time.UTC)}}
	item := NewDockerCollector(cfg, registry).Collect(context.Background(), target)
	if item.Status != ItemOK {
		t.Fatalf("status = %s, want ok (partial failure recorded inline)", item.Status)
	}
	if !strings.Contains(item.Body, `"status":"running"`) || !strings.Contains(item.Body, "error: tools: docker_logs failed") {
		t.Fatalf("body = %s", item.Body)
	}
}

func TestDockerCollectorMissingContainer(t *testing.T) {
	cfg := config.EvidenceConfig{DockerContainer: ""}
	item := NewDockerCollector(cfg, stubRegistry(t, nil)).Collect(context.Background(), Target{})
	if item.Status != ItemMissing {
		t.Fatalf("status = %s, want missing", item.Status)
	}
}

func TestPostgresCollectorMissingConfig(t *testing.T) {
	item := NewPostgresCollector(config.EvidenceConfig{}).Collect(context.Background(), Target{})
	if item.Status != ItemMissing || !strings.Contains(item.Err, "not configured") {
		t.Fatalf("postgres item = %+v, want missing", item)
	}
}

func TestRedisCollectorMissingConfig(t *testing.T) {
	item := NewRedisCollector(config.EvidenceConfig{}).Collect(context.Background(), Target{})
	if item.Status != ItemMissing || !strings.Contains(item.Err, "not configured") {
		t.Fatalf("redis item = %+v, want missing", item)
	}
}

func TestRedisInfoValue(t *testing.T) {
	info := "# Memory\r\nused_memory:1048576\r\nused_memory_human:1.00M\r\n"
	if got := redisInfoValue(info, "used_memory"); got != "1048576" {
		t.Fatalf("redisInfoValue(used_memory) = %q", got)
	}
	if got := redisInfoValue(info, "missing_key"); got != "unknown" {
		t.Fatalf("redisInfoValue(missing) = %q", got)
	}
}
