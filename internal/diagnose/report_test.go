package diagnose

import (
	"context"
	"testing"

	"oncall-agent/internal/approval"
	"oncall-agent/internal/llm"
	"oncall-agent/internal/notify"
	"oncall-agent/internal/store"
)

// capturingReporter 记录 pipeline 实际交出的报告内容。
type capturingReporter struct {
	report DiagnosisReport
}

func (c *capturingReporter) NotifyDiagnosis(_ context.Context, report DiagnosisReport) error {
	c.report = report
	return nil
}

// capturingNotifier 记录出站通知，用于断言 payload 契约。
type capturingNotifier struct {
	notification notify.Notification
}

func (c *capturingNotifier) Send(_ context.Context, n notify.Notification) (notify.Delivery, error) {
	c.notification = n
	return notify.Delivery{Provider: "capture"}, nil
}

// 审批卡片的回调按钮只携带不可变引用，plan hash 是必填项之一。
// 报告丢掉它，飞书审批按钮就会在回调字段校验处被判无效。
func TestPipelineReportCarriesApprovalPlanHash(t *testing.T) {
	db := newFakeRunStore()
	reasoner := &fakeReasoner{result: &llm.DiagnoseResult{
		RCA: "连接池耗尽", Confidence: "high",
		Plan: llm.Plan{Action: "pool_resize", Target: llm.PlanTarget{Kind: "service", Name: "sub2api"}},
	}}
	policy := &fakePolicy{decision: approval.Decision{Kind: approval.DecisionApproval, ToolName: "pool_resize", PlanHash: "abc"}}
	reporter := &capturingReporter{}
	pipeline := NewPipeline(db, fakeEvidenceBuilder{evidence: testEvidence()}, reasoner, policy, &fakeApprovals{}, reporter, nil, 0)
	run := store.AgentRun{ID: 31, IncidentID: 7, Mode: "full", Status: "running"}
	if err := pipeline.Run(context.Background(), run); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if reporter.report.ApprovalID == nil {
		t.Fatal("report.ApprovalID = nil, want the created approval")
	}
	// 必须与写入审批单的 hash 同源，否则回调侧比对必然失配。
	if reporter.report.PlanHash != "abc" {
		t.Fatalf("report.PlanHash = %q, want %q", reporter.report.PlanHash, "abc")
	}
}

func TestNotifyDiagnosisPayloadCarriesPlanHash(t *testing.T) {
	notifier := &capturingNotifier{}
	approvalID := uint64(42)
	err := NewNotifyReporter(notifier, "https://oncall.example.com").NotifyDiagnosis(context.Background(), DiagnosisReport{
		IncidentID: 7, RunID: 31, Mode: "full",
		RCA: "连接池耗尽", Confidence: "high",
		PolicyDecision: approval.DecisionApproval,
		ApprovalID:     &approvalID,
		PlanHash:       "abc",
	})
	if err != nil {
		t.Fatalf("NotifyDiagnosis() error = %v", err)
	}
	if got := notifier.notification.Payload["plan_hash"]; got != "abc" {
		t.Fatalf("payload plan_hash = %v, want %q", got, "abc")
	}
}
