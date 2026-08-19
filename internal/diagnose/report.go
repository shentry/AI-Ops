package diagnose

import (
	"context"
	"fmt"
	"time"

	"oncall-agent/internal/notify"
)

// NotifyReporter 把诊断报告适配到 notify.Notifier，并带有限独立重试：
// 通知失败重试 3 次，仍失败把错误交还 pipeline 记进 step，
// 但绝不能反过来改变 run 的终态。
type NotifyReporter struct {
	Notifier notify.Notifier
	// BaseURL 是审批 API 的服务地址，渲染审批 curl 用。
	BaseURL string
}

func NewNotifyReporter(notifier notify.Notifier, baseURL string) *NotifyReporter {
	return &NotifyReporter{Notifier: notifier, BaseURL: baseURL}
}

func (r *NotifyReporter) NotifyDiagnosis(ctx context.Context, report DiagnosisReport) error {
	msg := notify.DiagnosisMessage{
		IncidentID:     report.IncidentID,
		RunID:          report.RunID,
		Mode:           report.Mode,
		RCA:            report.RCA,
		Confidence:     report.Confidence,
		Decision:       report.Decision,
		Overridden:     report.Overridden,
		GuardNote:      report.GuardNote,
		PlanAction:     report.Plan.Action,
		PlanTarget:     fmt.Sprintf("%s/%s", report.Plan.Target.Kind, report.Plan.Target.Name),
		PlanReason:     report.Plan.Reason,
		PolicyDecision: report.PolicyDecision,
		ApprovalID:     report.ApprovalID,
		BaseURL:        r.BaseURL,
	}
	var err error
	for attempt := range 3 {
		if err = r.Notifier.Send(ctx, msg); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(time.Duration(attempt+1) * 500 * time.Millisecond):
		}
	}
	return fmt.Errorf("notify: 3 attempts failed: %w", err)
}
