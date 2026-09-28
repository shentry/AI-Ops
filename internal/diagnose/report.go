package diagnose

import (
	"context"
	"fmt"
	"time"

	"oncall-agent/internal/incident"
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
		"policy_decision": report.PolicyDecision,
		"base_url":        r.BaseURL,
	}
	notification := notify.Notification{
		Kind:       notify.NotificationDiagnosisCompleted,
		IncidentID: report.IncidentID,
		RunID:      &runID,
		Title:      "诊断报告",
		Summary:    report.RCA,
		Payload:    payload,
	}
	if a := report.Approval; a != nil {
		snapshot, err := incident.ParseExecutionContext(a.ExecutionContext)
		if err != nil {
			return fmt.Errorf("notify: invalid approval snapshot: %w", err)
		}
		hash, err := incident.PlanHash(a.ToolName, a.ArgsJSON, a.ExecutionContext)
		if err != nil || hash != a.PlanHash || a.ID == 0 {
			return fmt.Errorf("notify: approval content is not a committed immutable snapshot")
		}
		payload["action"], payload["tool_name"] = a.ToolName, a.ToolName
		payload["target"], payload["target_id"] = snapshot.Target.Kind+"/"+snapshot.Target.Name, snapshot.Target.ID
		payload["rule_id"], payload["rule_mode"] = snapshot.Rule.ID, snapshot.Rule.Mode
		payload["reason"], payload["plan_hash"] = a.Reason, a.PlanHash
		payload["approval_status"], payload["expires_at"] = a.Status, a.ExpiresAt.UTC().Format(time.RFC3339)
		if a.Status == "pending" {
			notification.ApprovalID = &a.ID
			notification.Kind = notify.NotificationApprovalRequired
		}
	} else {
		payload["plan_action"] = report.Plan.Action
		payload["plan_target"] = fmt.Sprintf("%s/%s", report.Plan.Target.Kind, report.Plan.Target.Name)
		payload["plan_reason"] = report.Plan.Reason
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
