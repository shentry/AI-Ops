package acceptance

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"oncall-agent/internal/config"
	"oncall-agent/internal/tools"
)

func TestBusinessMetricsUseActualPrometheusSampleAge(t *testing.T) {
	image := os.Getenv("TEST_PROMETHEUS_IMAGE")
	if image == "" {
		t.Skip("requires TEST_PROMETHEUS_IMAGE; creates an isolated Prometheus container")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var fresh atomic.Bool
	exporter := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		at := time.Now()
		if !fresh.Load() {
			at = at.Add(-3 * time.Minute)
		}
		fmt.Fprintf(w, "sub2api_requests_5m{class=\"sla\"} 100 %d\nsub2api_errors_5m{class=\"sla\"} 0 %d\nsub2api_ops_up{endpoint=\"overview\"} 1 %d\n", at.UnixMilli(), at.UnixMilli(), at.UnixMilli())
	}))
	defer exporter.Close()
	port := strings.TrimPrefix(exporter.URL, "http://127.0.0.1:")
	path := filepath.Join(t.TempDir(), "prometheus.yml")
	body := fmt.Sprintf("global:\n  scrape_interval: 1s\nscrape_configs:\n  - job_name: fixture\n    honor_timestamps: true\n    static_configs:\n      - targets: ['host.docker.internal:%s']\n", port)
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("oncall-prometheus-%d", time.Now().UnixNano())
	out, err := exec.CommandContext(ctx, "docker", "run", "-d", "--name", name, "--user", "0", "--label", "oncall.test=unattended", "-p", "127.0.0.1::9090", "-v", path+":/etc/prometheus/prometheus.yml:ro", image).CombinedOutput()
	if err != nil {
		t.Fatalf("start Prometheus: %s %v", out, err)
	}
	id := strings.TrimSpace(string(out))
	t.Cleanup(func() {
		clean, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if out, err := exec.CommandContext(clean, "docker", "rm", "-f", "-v", id).CombinedOutput(); err != nil {
			t.Errorf("cleanup %s %v", out, err)
		}
	})
	out, err = exec.CommandContext(ctx, "docker", "port", id, "9090/tcp").Output()
	if err != nil {
		t.Fatal(err)
	}
	client, err := tools.NewPrometheusClient(config.PrometheusConfig{BaseURL: "http://" + strings.TrimSpace(string(out)), RangeMinutes: 15, MaxPoints: 100})
	if err != nil {
		t.Fatal(err)
	}
	registry := tools.NewRegistry()
	if err := client.RegisterTools(registry); err != nil {
		t.Fatal(err)
	}
	// Verify the raw instant selector returns an old scrape with a current evaluation time.
	waitUntil(t, ctx, func() bool {
		out, err := registry.Execute(ctx, tools.ToolPromInstantQuery, []byte(`{"query":"sub2api_requests_5m{class=\"sla\"}"}`))
		return err == nil && strings.Contains(out, `"100"`)
	})
	if traffic, err := tools.ReadBusinessTraffic(ctx, registry); err == nil {
		t.Fatalf("three-minute-old scrape accepted: %+v", traffic)
	}
	fresh.Store(true)
	waitUntil(t, ctx, func() bool {
		traffic, err := tools.ReadBusinessTraffic(ctx, registry)
		return err == nil && traffic.Requests == 100 && traffic.Errors == 0
	})
	t.Log("real Prometheus: stale underlying samples rejected; fresh samples accepted")
}
