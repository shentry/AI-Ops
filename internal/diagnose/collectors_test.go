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
