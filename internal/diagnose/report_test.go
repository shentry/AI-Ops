package diagnose

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"oncall-agent/internal/approval"
	"oncall-agent/internal/incident"
	"oncall-agent/internal/llm"
	"oncall-agent/internal/notify"
	"oncall-agent/internal/store"
)

type capturingReporter struct{ report DiagnosisReport }

func (c *capturingReporter) NotifyDiagnosis(_ context.Context, report DiagnosisReport) error {
	c.report = report
	return nil
}

type capturingNotifier struct {
	notification notify.Notification
	calls        int
}

func (c *capturingNotifier) Send(_ context.Context, n notify.Notification) (notify.Delivery, error) {
	c.notification = n
	c.calls++
	return notify.Delivery{Provider: "capture"}, nil
}

func reportApproval(t *testing.T, dryRun bool) store.Approval {
	t.Helper()
	snapshot, err := json.Marshal(incident.ExecutionContext{SafetyLevel: "L3", DryRun: dryRun, Verification: incident.VerificationSpec{
		Kind: incident.HealthVerification, TargetName: "sub2api", BaseURL: "http://private-target.invalid:8080", MemberFingerprints: []string{"private-fingerprint"}, IntervalSeconds: 10, WindowSeconds: 120, TimeoutSeconds: 5,
	}})
	if err != nil {
		t.Fatal(err)
	}
	a := store.Approval{ID: 42, IncidentID: 7, RunID: 31, Status: "pending", ToolName: incident.RestartAction, Reason: "manual approval required", ArgsJSON: []byte(`{"target_kind":"container","target_name":"sub2api"}`), ExecutionContext: snapshot, ExpiresAt: time.Now().UTC().Add(time.Hour)}
	a.PlanHash, err = incident.PlanHash(a.ToolName, a.ArgsJSON, a.ExecutionContext)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestPipelineReportCarriesCommittedApprovalSnapshot(t *testing.T) {
	db := newFakeRunStore()
	a := reportApproval(t, true)
	policy := &fakePolicy{decision: approval.Decision{Kind: approval.DecisionApproval, ToolName: a.ToolName, Args: json.RawMessage(a.ArgsJSON), ExecutionContext: json.RawMessage(a.ExecutionContext), PlanHash: a.PlanHash, Reason: a.Reason}}
	reporter := &capturingReporter{}
	pipeline := NewPipeline(db, fakeEvidenceBuilder{evidence: testEvidence()}, &fakeReasoner{result: &llm.DiagnoseResult{RCA: "容器退出", Confidence: "high"}}, policy, approval.NewService(nil, 30), reporter, nil, 0)
	if err := pipeline.Run(context.Background(), store.AgentRun{ID: 31, IncidentID: 7, Mode: "full", Status: "running"}); err != nil {
		t.Fatal(err)
	}
	got := reporter.report.Approval
	if got == nil || got.ID != 42 || got.PlanHash != a.PlanHash || string(got.ExecutionContext) != string(a.ExecutionContext) || got != db.completions[0].Approval {
		t.Fatalf("report approval = %+v", got)
	}
}

func TestNotifyDiagnosisApprovalPayloadUsesOnlySnapshot(t *testing.T) {
	for _, dryRun := range []bool{false, true} {
		notifier := &capturingNotifier{}
		a := reportApproval(t, dryRun)
		err := NewNotifyReporter(notifier, "https://oncall.example.com").NotifyDiagnosis(context.Background(), DiagnosisReport{
			IncidentID: 7, RunID: 31, Mode: "full", RCA: "容器退出", Confidence: "high", Approval: &a,
			PolicyDecision: approval.DecisionApproval,
			Plan:           llm.Plan{Action: "model_action", Target: llm.PlanTarget{Kind: "cluster", Name: "model_target"}, Risk: "low"},
		})
		if err != nil {
			t.Fatal(err)
		}
		n := notifier.notification
		for key, want := range map[string]any{"action": a.ToolName, "tool_name": a.ToolName, "target": "container/sub2api", "scope": "single_container", "safety_level": "L3", "dry_run": dryRun, "reason": a.Reason, "plan_hash": a.PlanHash, "approval_status": "pending", "expires_at": a.ExpiresAt.UTC().Format(time.RFC3339)} {
			if got := n.Payload[key]; got != want {
				t.Fatalf("%s = %v, want %v", key, got, want)
			}
		}
		if n.ApprovalID == nil || *n.ApprovalID != a.ID || n.Kind != notify.NotificationApprovalRequired {
			t.Fatalf("notification = %+v", n)
		}
		for _, key := range []string{"risk", "plan_risk", "verification", "execution_context", "args_json"} {
			if _, exists := n.Payload[key]; exists {
				t.Fatalf("unsafe payload key %q", key)
			}
		}
		encoded, _ := json.Marshal(n.Payload)
		if strings.Contains(string(encoded), "private-target") || strings.Contains(string(encoded), "private-fingerprint") {
			t.Fatalf("private snapshot fields leaked: %s", encoded)
		}
	}
}

func TestNotifyDiagnosisSystemApprovalHasNoPendingAction(t *testing.T) {
	a := reportApproval(t, false)
	a.Status = "approved"
	notifier := &capturingNotifier{}
	if err := NewNotifyReporter(notifier, "").NotifyDiagnosis(context.Background(), DiagnosisReport{IncidentID: 7, RunID: 31, Approval: &a, PolicyDecision: approval.DecisionAutoL2}); err != nil {
		t.Fatal(err)
	}
	if notifier.notification.Kind == notify.NotificationApprovalRequired || notifier.notification.ApprovalID != nil || notifier.notification.Payload["approval_status"] != "approved" {
		t.Fatalf("system approval presented as pending: %+v", notifier.notification)
	}
}

func TestNotifyDiagnosisRejectsMalformedApprovalSnapshot(t *testing.T) {
	for _, change := range []func(*store.Approval){
		func(a *store.Approval) { a.ExecutionContext = nil },
		func(a *store.Approval) { a.ExecutionContext = []byte(`{"dry_run":false}`) },
		func(a *store.Approval) { a.PlanHash = "wrong" },
		func(a *store.Approval) { a.ID = 0 },
	} {
		a := reportApproval(t, true)
		change(&a)
		notifier := &capturingNotifier{}
		if err := NewNotifyReporter(notifier, "").NotifyDiagnosis(context.Background(), DiagnosisReport{IncidentID: 7, RunID: 31, Approval: &a}); err == nil || notifier.calls != 0 {
			t.Fatalf("invalid snapshot sent: err=%v calls=%d", err, notifier.calls)
		}
	}
}
