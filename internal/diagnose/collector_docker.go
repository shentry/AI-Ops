package diagnose

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"oncall-agent/internal/config"
	"oncall-agent/internal/tools"
)

// dockerCollector 采集目标容器的状态和时间窗内的受限日志。
// 容器名来自配置（证据采集不允许用外部输入拼容器名）；
// 日志窗口从 incident 开始时刻起算，行数受 evidence.log_max_lines 封顶。
type dockerCollector struct {
	cfg      config.EvidenceConfig
	registry *tools.Registry
}

func NewDockerCollector(cfg config.EvidenceConfig, registry *tools.Registry) Collector {
	return &dockerCollector{cfg: cfg, registry: registry}
}

func (*dockerCollector) Name() string { return "docker" }

func (c *dockerCollector) Collect(ctx context.Context, target Target) EvidenceItem {
	container := strings.TrimSpace(c.cfg.DockerContainer)
	if container == "" {
		return missingItem(c.Name(), "docker:inspect/logs", "docker container is not configured")
	}
	var body strings.Builder
	inspectArgs, _ := json.Marshal(map[string]string{"name": container})
	inspect, err := c.registry.Execute(ctx, tools.ToolDockerInspect, inspectArgs)
	body.WriteString("container_state:\n")
	if err != nil {
		fmt.Fprintf(&body, "  error: %s\n", err)
	} else {
		fmt.Fprintf(&body, "  %s\n", inspect)
	}

	logsArgs, _ := json.Marshal(map[string]any{
		"name":  container,
		"tail":  c.cfg.LogMaxLines,
		"since": target.Incident.StartedAt.UTC().Format(time.RFC3339),
	})
	logs, err := c.registry.Execute(ctx, tools.ToolDockerLogs, logsArgs)
	body.WriteString("recent_logs:\n")
	if err != nil {
		fmt.Fprintf(&body, "  error: %s\n", err)
	} else {
		body.WriteString(logs)
		body.WriteString("\n")
	}
	return finishItem(c.Name(), "docker:inspect/logs", body.String(), nil)
}
