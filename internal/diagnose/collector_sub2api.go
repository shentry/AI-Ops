package diagnose

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"oncall-agent/internal/config"
	"oncall-agent/internal/tools"
)

// sub2apiCollector 采集 Sub2API 网关证据：直连 /health + Prometheus 上的
// 请求量/5xx/延迟指标。base_url 未配置时记 missing，不让采集整体失败。
type sub2apiCollector struct {
	cfg        config.EvidenceConfig
	registry   *tools.Registry
	httpClient *http.Client
}

func NewSub2APICollector(cfg config.EvidenceConfig, registry *tools.Registry) Collector {
	return &sub2apiCollector{
		cfg:      cfg,
		registry: registry,
		httpClient: &http.Client{
			Timeout: time.Duration(cfg.TimeoutSeconds) * time.Second,
		},
	}
}

func (*sub2apiCollector) Name() string { return "sub2api" }

func (c *sub2apiCollector) Collect(ctx context.Context, _ Target) EvidenceItem {
	if strings.TrimSpace(c.cfg.Sub2APIBaseURL) == "" {
		return missingItem(c.Name(), "sub2api:/health", "sub2api base_url is not configured")
	}
	var body strings.Builder
	body.WriteString("health:\n")
	healthErr := c.collectHealth(ctx, &body)
	if healthErr != nil {
		fmt.Fprintf(&body, "  error: %s\n", healthErr)
	}
	body.WriteString("metrics:\n")
	c.collectMetrics(ctx, &body)
	if healthErr != nil {
		// 采集状态必须跟真实健康状态一致：/health 返回 5xx 或请求失败时
		// 报 ok 会让下游把故障证据当正常证据读（"网关是好的"），
		// 那正是诊断最不该有的误导。正文照样保留。
		return degradedItem(c.Name(), "sub2api:/health + prometheus", body.String(), healthErr)
	}
	return finishItem(c.Name(), "sub2api:/health + prometheus", body.String(), nil)
}

func (c *sub2apiCollector) collectHealth(ctx context.Context, body *strings.Builder) error {
	result := readHTTPHealth(ctx, c.httpClient, c.cfg.Sub2APIBaseURL)
	if result.statusCode != 0 {
		fmt.Fprintf(body, "  status=%d latency_ms=%d body=%q\n", result.statusCode, result.latencyMS, result.excerpt)
	}
	if result.Observation != "healthy" {
		return errors.New(result.Detail)
	}
	return nil
}

// metricQueries 是网关指标的写死模板。job 名来自配置，指标名遵循
// Prometheus HTTP 惯例；环境里没有这些指标时返回空结果而不是错误。
func (c *sub2apiCollector) metricQueries() []struct{ name, query string } {
	job := c.cfg.Sub2APIMetricsJob
	return []struct{ name, query string }{
		{"request_rate_5m", fmt.Sprintf(`sum(rate(http_requests_total{job=%q}[5m]))`, job)},
		{"error_5xx_rate_5m", fmt.Sprintf(`sum(rate(http_requests_total{job=%q,status=~"5.."}[5m]))`, job)},
		{"latency_p99_5m", fmt.Sprintf(`histogram_quantile(0.99, sum(rate(http_request_duration_seconds_bucket{job=%q}[5m])) by (le))`, job)},
	}
}

func (c *sub2apiCollector) collectMetrics(ctx context.Context, body *strings.Builder) {
	for _, m := range c.metricQueries() {
		args, _ := json.Marshal(map[string]string{"query": m.query})
		out, err := c.registry.Execute(ctx, tools.ToolPromInstantQuery, args)
		if err != nil {
			fmt.Fprintf(body, "  %s: error: %s\n", m.name, err)
			continue
		}
		fmt.Fprintf(body, "  %s: %s\n", m.name, out)
	}
}
