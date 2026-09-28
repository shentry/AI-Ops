package diagnose

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"oncall-agent/internal/tools"
)

// goldenQueries 是宿主机黄金指标的写死 PromQL 模板（CPU/内存/磁盘/网络）。
// 模板不允许外部输入拼接 —— 查询文本是代码的一部分，不是运行时数据。
var goldenQueries = []struct {
	name  string
	query string
}{
	{"cpu_usage_percent", `100 - (avg(rate(node_cpu_seconds_total{mode="idle"}[5m])) * 100)`},
	{"memory_usage_percent", `100 * (1 - node_memory_MemAvailable_bytes / node_memory_MemTotal_bytes)`},
	{"disk_root_usage_percent", `100 * (1 - node_filesystem_avail_bytes{mountpoint="/"} / node_filesystem_size_bytes{mountpoint="/"})`},
	{"network_receive_bytes_per_sec", `sum(rate(node_network_receive_bytes_total{device!="lo"}[5m]))`},
	{"network_transmit_bytes_per_sec", `sum(rate(node_network_transmit_bytes_total{device!="lo"}[5m]))`},
}

// goldenMetricsCollector 查宿主机黄金指标。单条失败记进正文，
// 不阻断其余指标；部分失败报 partial，全部失败报 error。
type goldenMetricsCollector struct {
	registry *tools.Registry
}

func NewGoldenMetricsCollector(registry *tools.Registry) Collector {
	return goldenMetricsCollector{registry: registry}
}

func (goldenMetricsCollector) Name() string { return "golden_metrics" }

func (c goldenMetricsCollector) Collect(ctx context.Context, target Target) EvidenceItem {
	var body strings.Builder
	failures := 0
	for _, g := range goldenQueries {
		args, _ := json.Marshal(map[string]string{"query": g.query})
		out, err := c.registry.Execute(ctx, tools.ToolPromInstantQuery, args)
		fmt.Fprintf(&body, "%s:\n", g.name)
		if err != nil {
			fmt.Fprintf(&body, "  error: %s\n", err)
			failures++
			continue
		}
		fmt.Fprintf(&body, "  %s\n", out)
	}
	return queriesItem(c.Name(), "prometheus:query", body.String(), failures, len(goldenQueries))
}
