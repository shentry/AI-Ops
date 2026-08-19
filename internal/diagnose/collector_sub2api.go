package diagnose

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
	if err := c.collectHealth(ctx, &body); err != nil {
		fmt.Fprintf(&body, "  error: %s\n", err)
	}
	body.WriteString("metrics:\n")
	c.collectMetrics(ctx, &body)
	return finishItem(c.Name(), "sub2api:/health + prometheus", body.String(), nil)
}

func (c *sub2apiCollector) collectHealth(ctx context.Context, body *strings.Builder) error {
	base, err := url.Parse(c.cfg.Sub2APIBaseURL)
	if err != nil {
		return fmt.Errorf("invalid sub2api base_url")
	}
	endpoint := base.ResolveReference(&url.URL{Path: "/health"})
	started := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("health request failed: %w", err)
	}
	defer resp.Body.Close()
	// /health 响应体只需要知道"通不通、什么码"，读前 512 字节留个痕迹即可。
	excerpt, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	fmt.Fprintf(body, "  status=%d latency_ms=%d body=%q\n",
		resp.StatusCode, time.Since(started).Milliseconds(), ToSafeText(string(excerpt)))
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
