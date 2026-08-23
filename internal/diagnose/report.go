package diagnose

import (
	"context"
	"fmt"
	"time"

	"oncall-agent/internal/notify"
)

// NotifyReporter adapts a diagnosis report to the provider-neutral notifier.
// It retries delivery three times, but notification failure never changes the
// terminal run state; the pipeline records the returned error separately.
type NotifyReporter struct {
	Notifier notify.Notifier
	// BaseURL is retained as a sanitized display field for legacy webhook cards.
	BaseURL string
}

func NewNotifyReporter(notifier notify.Notifier, baseURL string) *NotifyReporter {
	return &NotifyReporter{Notifier: notifier, BaseURL: baseURL}
}

func (r *NotifyReporter) NotifyDiagnosis(ctx context.Context, report DiagnosisReport) error {
	runID := report.RunID
	payload := map[string]any{
		"mode":            report.Mode,
		"rca":             report.RCA,
		"confidence":      report.Confidence,
		"decision":        report.Decision,
		"overridden":      report.Overridden,
		"guard_note":      report.GuardNote,
		"plan_action":     report.Plan.Action,
		"plan_target":     fmt.Sprintf("%s/%s", report.Plan.Target.Kind, report.Plan.Target.Name),
		"plan_reason":     report.Plan.Reason,
		"policy_decision": report.PolicyDecision,
		"plan_hash":       report.PlanHash,
		"base_url":        r.BaseURL,
	}
	notification := notify.Notification{
		Kind:       notify.NotificationDiagnosisCompleted,
		IncidentID: report.IncidentID,
		RunID:      &runID,
		ApprovalID: report.ApprovalID,
		Title:      "诊断报告",
		Summary:    report.RCA,
		Payload:    payload,
	}
	var err error
	for attempt := range 3 {
		if _, err = r.Notifier.Send(ctx, notification); err == nil {
			return nil
		}
		if attempt == 2 {
			break
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(time.Duration(attempt+1) * 500 * time.Millisecond):
		}
	}
	return fmt.Errorf("notify: 3 attempts failed: %w", err)
}
