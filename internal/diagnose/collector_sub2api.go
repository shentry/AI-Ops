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

// sub2apiHealthCollector 直连 /health 读一次存活状态。sub2api 的 /health
// 固定返回 ok、不检查依赖，只能证明进程在响应，不能证明业务可用。
type sub2apiHealthCollector struct {
	service    config.ServiceConfig
	httpClient *http.Client
}

func NewSub2APIHealthCollector(service config.ServiceConfig, evidence config.EvidenceConfig) Collector {
	return &sub2apiHealthCollector{service: service, httpClient: &http.Client{Timeout: time.Duration(evidence.TimeoutSeconds) * time.Second}}
}

func (*sub2apiHealthCollector) Name() string { return "sub2api_health" }

func (c *sub2apiHealthCollector) Collect(ctx context.Context, _ Target) EvidenceItem {
	const source = "sub2api:/health"
	if strings.TrimSpace(c.service.BaseURL) == "" {
		return missingItem(c.Name(), source, "service base_url is not configured")
	}
	result := readHTTPHealth(ctx, c.httpClient, c.service.BaseURL)
	body := ""
	if result.statusCode != 0 {
		body = fmt.Sprintf("status=%d latency_ms=%d body=%q\n", result.statusCode, result.latencyMS, result.excerpt)
	}
	// 采集状态必须跟真实健康状态一致：非 2xx 或请求失败时报 ok 会让下游
	// 把故障证据当正常证据读。unhealthy 与 unavailable 由事实区分。
	item := finishItem(c.Name(), source, body, nil)
	if result.Observation != "healthy" {
		item = degradedItem(c.Name(), source, body, errors.New(result.Detail))
	}
	item.Object = &ObjectRef{Kind: "service", Name: c.service.Name}
	item.Health = &HealthFacts{Observation: result.Observation, StatusCode: result.statusCode, LatencyMS: result.latencyMS}
	return item
}

// sub2apiMetricsCollector 查 ops exporter 导出的真实流量指标（sub2api 自身
// 没有 /metrics）。错误率同时给出告警前基线和当前值，便于区分突变与常态。
// sub2api_ops_up=0 表示业务数据不可用，不是零流量。
type sub2apiMetricsCollector struct {
	registry *tools.Registry
}

func NewSub2APIMetricsCollector(registry *tools.Registry) Collector {
	return &sub2apiMetricsCollector{registry: registry}
}

func (*sub2apiMetricsCollector) Name() string { return "sub2api_metrics" }

// businessQueries 是写死的查询模板；before 为真的查询额外取告警前基线。
var businessQueries = []struct {
	name, query string
	before      bool
}{
	{"sla_requests_5m", `sub2api_requests_5m{class="sla"}`, true},
	{"sla_error_ratio_5m", `sub2api_errors_5m{class="sla"} / sub2api_requests_5m{class="sla"}`, true},
	{"upstream_errors_5m", `sub2api_upstream_errors_5m`, false},
	{"top_upstream_accounts_5m", `topk(5, sub2api_account_upstream_errors_5m)`, false},
	{"group_available_accounts", `sub2api_group_accounts{state="available"}`, false},
	{"request_p95_seconds_5m", `sub2api_request_duration_p95_seconds_5m`, false},
	{"business_probe_success", `sub2api_probe_success`, false},
}

func (c *sub2apiMetricsCollector) Collect(ctx context.Context, target Target) EvidenceItem {
	const source = "prometheus:query"
	before := target.Incident.StartedAt.UTC().Add(-evidenceLookback)
	var body strings.Builder
	traffic, trafficErr := tools.ReadBusinessTraffic(ctx, c.registry)
	failed, total := 0, 1
	if trafficErr != nil {
		failed++
		fmt.Fprintf(&body, "business_traffic: error: %s\n", trafficErr)
	} else {
		fmt.Fprintf(&body, "sla_requests_5m: %.0f\nsla_error_ratio_5m: %.6f\n", traffic.Requests, traffic.ErrorRatio())
	}
	query := func(label, promql string, at time.Time) {
		total++
		args := map[string]string{"query": promql}
		if !at.IsZero() {
			args["time"] = at.Format(time.RFC3339)
		}
		raw, _ := json.Marshal(args)
		out, err := c.registry.Execute(ctx, tools.ToolPromInstantQuery, raw)
		if err != nil {
			failed++
			fmt.Fprintf(&body, "%s: error: %s\n", label, err)
			return
		}
		fmt.Fprintf(&body, "%s: %s\n", label, out)
	}
	for _, q := range businessQueries {
		if !q.before {
			query(q.name, q.query, time.Time{})
		}
		if q.before && !target.Incident.StartedAt.IsZero() {
			query(q.name+"@alert-15m", q.query, before)
		}
	}
	item := queriesItem(c.Name(), source, body.String(), failed, total)
	if trafficErr == nil {
		item.Business = &traffic
	}
	return item
}
