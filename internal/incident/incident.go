// Package incident 承载 D05 的 incident 生命周期纯逻辑：
// severity → 诊断模式分流、agent_run 队列行构造。
// DB 读写仍在 internal/store，这里只做不碰库的判断和组装。
package incident

import (
	"time"

	"oncall-agent/internal/store"
)

// agent_run.mode 的合法取值。memory_hit 由 D13 的记忆路径产生，
// severity 分流只会产出前三种。
const (
	ModeFull  = "full"
	ModeLight = "light"
	ModeSkip  = "skip"
)

// SeverityName 把 ingest 的 5..1 数值级别映回路由配置用的标签名。
// 与 ingest.Severity 的映射互为镜像：critical=5 … low=1。
// 未知值回退 warning —— 和 ingest.Severity 的缺省级别保持一致，
// 不让一个陌生级别意外拿到 full 诊断预算。
func SeverityName(severity int) string {
	switch severity {
	case 5:
		return "critical"
	case 4:
		return "high"
	case 3:
		return "warning"
	case 2:
		return "info"
	case 1:
		return "low"
	default:
		return "warning"
	}
}

// RouteMode 按 diagnose.severity_route 把 incident 级别分流成诊断模式。
// 配置缺条目时回退 light：full 浪费预算、skip 漏诊断，light 是中间档。
// 配置里的非法值按缺条目处理，同样回退 light。
func RouteMode(severity int, route map[string]string) string {
	mode := route[SeverityName(severity)]
	switch mode {
	case ModeFull, ModeLight, ModeSkip:
		return mode
	default:
		return ModeLight
	}
}

// NewQueueRun 构造促发分流要落的 agent_run 队列行。
// skip 不进诊断队列等待消费，直接落 succeeded：也留一行可统计，
// 证明这条 incident 被分流规则看过，而不是被漏掉。
func NewQueueRun(incidentID uint64, mode string, now time.Time) store.AgentRun {
	run := store.AgentRun{
		IncidentID: incidentID,
		Mode:       mode,
		Status:     "pending",
		StartedAt:  now.UTC(),
	}
	if mode == ModeSkip {
		run.Status = "succeeded"
		run.FinishedAt = &run.StartedAt
	}
	return run
}
