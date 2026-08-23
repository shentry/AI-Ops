// Package metrics 是进程内计数器 + /metrics 文本端点（Prometheus 格式）。
// 不引 client_golang：V1 只需要单调计数器，几十行代码就够了。
package metrics

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

var counters sync.Map // name -> *atomic.Int64

// gauges 是抓取时现算的指标（队列深度等瞬时值）。
var gauges sync.Map // name -> func() int64

// Inc 给计数器加一。计数器只增不减，进程重启归零 —— 趋势由 Prometheus 抓取侧算。
func Inc(name string) {
	value, _ := counters.LoadOrStore(name, &atomic.Int64{})
	value.(*atomic.Int64).Add(1)
}

// RegisterGauge 注册抓取时现算的 gauge。注册在启动期完成，fn 必须无阻塞。
func RegisterGauge(name string, fn func() int64) {
	gauges.Store(name, fn)
}

// Write 按 Prometheus 文本格式输出全部计数器与 gauge。
func Write(w io.Writer) {
	names := make([]string, 0)
	counters.Range(func(key, _ any) bool {
		names = append(names, key.(string))
		return true
	})
	sort.Strings(names)
	for _, name := range names {
		value, _ := counters.Load(name)
		fmt.Fprintf(w, "# TYPE ai_opus_%s counter\nai_opus_%s %d\n",
			sanitizeName(name), sanitizeName(name), value.(*atomic.Int64).Load())
	}
	gaugeNames := make([]string, 0)
	gauges.Range(func(key, _ any) bool {
		gaugeNames = append(gaugeNames, key.(string))
		return true
	})
	sort.Strings(gaugeNames)
	for _, name := range gaugeNames {
		fn, _ := gauges.Load(name)
		fmt.Fprintf(w, "# TYPE ai_opus_%s gauge\nai_opus_%s %d\n",
			sanitizeName(name), sanitizeName(name), fn.(func() int64)())
	}
}
func sanitizeName(name string) string {
	return strings.ReplaceAll(name, ".", "_")
}

// 全系统的指标名注册：一处列全，防止散落拼写漂移。
const (
	WebhookReceived    = "webhook_received"
	RawEventProcessed  = "raw_event_processed"
	RawEventFailed     = "raw_event_failed"
	IncidentPromoted   = "incident_promoted"
	IncidentResolved   = "incident_resolved"
	AgentRunEnqueued   = "agent_run_enqueued"
	AgentRunSucceeded  = "agent_run_succeeded"
	AgentRunFailed     = "agent_run_failed"
	MemoryHit          = "memory_hit"
	MemoryMiss         = "memory_miss"
	ApprovalCreated    = "approval_created"
	ApprovalApproved   = "approval_approved"
	ApprovalDenied     = "approval_denied"
	ApprovalExpired    = "approval_expired"
	ApprovalExecuted   = "approval_executed"
	ApprovalFailedExec = "approval_failed_execution"
	VerifyPassed       = "verify_passed"
	VerifyFailed       = "verify_failed"
	VerifyInconclusive = "verify_inconclusive"
	EscalationSent     = "escalation_sent"
	DedupFull          = "dedup_full"
	MemoryExpired      = "memory_expired"
	MemoryDemoted      = "memory_demoted"
	PendingRawEvents   = "pending_raw_events"
	PendingAgentRuns   = "pending_agent_runs"
)
