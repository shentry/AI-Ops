package diagnose

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"oncall-agent/internal/config"
	"oncall-agent/internal/tools"
)

// evidenceLookback 是告警前的回看窗口：故障往往在告警触发前就开始了。
const evidenceLookback = 15 * time.Minute

// dockerInspectCollector 采集目标容器的身份与状态事实。容器名来自配置
// （证据采集不允许用外部输入拼容器名）；对象身份取自 Docker 响应，
// 不取自配置或告警标签。inspect 失败就是 error，不和日志混成一个 ok。
type dockerInspectCollector struct {
	container string
	registry  *tools.Registry
}

func NewDockerInspectCollector(service config.ServiceConfig, registry *tools.Registry) Collector {
	return &dockerInspectCollector{container: service.Container, registry: registry}
}

func (*dockerInspectCollector) Name() string { return "docker_inspect" }

func (c *dockerInspectCollector) Collect(ctx context.Context, _ Target) EvidenceItem {
	const source = "docker:inspect"
	container := strings.TrimSpace(c.container)
	if container == "" {
		return missingItem(c.Name(), source, "docker container is not configured")
	}
	args, _ := json.Marshal(map[string]string{"name": container})
	out, err := c.registry.Execute(ctx, tools.ToolDockerInspect, args)
	if err != nil {
		return finishItem(c.Name(), source, "", err)
	}
	var inspect tools.ContainerInspect
	if err := json.Unmarshal([]byte(out), &inspect); err != nil {
		return finishItem(c.Name(), source, "", errors.New("docker inspect output is not decodable"))
	}
	if inspect.ID == "" || inspect.Name != container {
		return finishItem(c.Name(), source, "", errors.New("docker inspect response does not identify the configured container"))
	}
	// 事实本身就是全部内容，渲染时单独一行，不再重复放进正文。
	item := finishItem(c.Name(), source, "", nil)
	item.Object = &ObjectRef{Kind: "container", Name: inspect.Name, ID: inspect.ID}
	item.Container = &ContainerFacts{
		Status: inspect.Status, Running: inspect.Running, Restarting: inspect.Restarting,
		OOMKilled: inspect.OOMKilled, ExitCode: inspect.ExitCode, RestartCount: inspect.RestartCount,
		RestartPolicy: inspect.RestartPolicy, Health: inspect.Health,
		Image: inspect.Image, ImageID: inspect.ImageID, StartedAt: inspect.StartedAt, FinishedAt: inspect.FinishedAt,
		RepoDigests: inspect.RepoDigests,
	}
	return item
}

// dockerLogsCollector 采集目标容器从告警前回看窗口起的日志模式。
// 行数受 evidence.log_max_lines 封顶；聚合由 docker_logs 工具完成。
// 日志按容器名读取，身份只有名称：同一实例的核对以 docker_inspect 的 ID 为准。
type dockerLogsCollector struct {
	container string
	maxLines  int
	registry  *tools.Registry
}

func NewDockerLogsCollector(service config.ServiceConfig, evidence config.EvidenceConfig, registry *tools.Registry) Collector {
	return &dockerLogsCollector{container: service.Container, maxLines: evidence.LogMaxLines, registry: registry}
}

func (*dockerLogsCollector) Name() string { return "docker_logs" }

func (c *dockerLogsCollector) Collect(ctx context.Context, target Target) EvidenceItem {
	const source = "docker:logs"
	container := strings.TrimSpace(c.container)
	if container == "" {
		return missingItem(c.Name(), source, "docker container is not configured")
	}
	args, _ := json.Marshal(map[string]any{
		"name":  container,
		"tail":  c.maxLines,
		"since": target.Incident.StartedAt.Add(-evidenceLookback).UTC().Format(time.RFC3339),
	})
	logs, err := c.registry.Execute(ctx, tools.ToolDockerLogs, args)
	item := finishItem(c.Name(), source, logs, err)
	if err == nil {
		item.Object = &ObjectRef{Kind: "container", Name: container}
	}
	return item
}
