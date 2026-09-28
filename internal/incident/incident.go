// Package incident 承载 incident 生命周期的纯逻辑：归并判定、状态机推进、
// severity → 诊断模式分流、重诊准入。
//
// 本包不 import internal/store，也不碰库。规则以「当前状态 + 输入 → 新状态」
// 的纯函数形式表达，由 store 在行锁与写库之间调用。方向是单向的
// —— store 依赖 incident，反过来不行 —— 所以这里的每条规则都能脱离
// 真 MySQL 用普通单测覆盖。
package incident

import "time"

// agent_run.mode 的合法取值。memory_hit 由 D13 的记忆路径产生，
// severity 分流只会产出前三种。
const (
	ModeFull  = "full"
	ModeLight = "light"
	ModeSkip  = "skip"
)

// incident.status 的取值，对应 migrations/001_init.sql 的 ENUM。
const (
	StatusCandidate    = "candidate"
	StatusFiring       = "firing"
	StatusAcknowledged = "acknowledged"
	StatusResolved     = "resolved"
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

// QueueRun 是促发分流算出的 agent_run 队列行取值，不含表结构。
// 调用方（ingest worker）据此组装 store.AgentRun —— 本包不 import store。
type QueueRun struct {
	IncidentID uint64
	Mode       string
	Status     string
	StartedAt  time.Time
	// Finished 为 true 时调用方应把 finished_at 填成 StartedAt。
	Finished bool
}

// NewQueueRun 构造促发分流要落的 agent_run 队列行。
// skip 不进诊断队列等待消费，直接落 succeeded：也留一行可统计，
// 证明这条 incident 被分流规则看过，而不是被漏掉。
func NewQueueRun(incidentID uint64, mode string, now time.Time) QueueRun {
	run := QueueRun{
		IncidentID: incidentID,
		Mode:       mode,
		Status:     "pending",
		StartedAt:  now.UTC(),
	}
	if mode == ModeSkip {
		run.Status = "succeeded"
		run.Finished = true
	}
	return run
}

// IsOpen 判断 incident 是否还在「活着」的状态。只有 open 的 incident
// 才吸收新成员、才值得续命、才能被关单 —— acknowledged 是人工接管，
// resolved 是已关单，两者都不该被自动路径改写。
func IsOpen(status string) bool {
	return status == StatusCandidate || status == StatusFiring
}
