package effectiveness

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"oncall-agent/internal/approval"
	"oncall-agent/internal/config"
	"oncall-agent/internal/diagnose"
	"oncall-agent/internal/incident"
	"oncall-agent/internal/tools"
)

// Fixed test fact: no recent execution. This replay does not contact a database.
type emptyExecutionHistory struct{}

func (emptyExecutionHistory) CountRecentExecutions(context.Context, string, string, time.Time) (int, error) {
	return 0, nil
}

// Characterize the policy boundary using captured real-model outputs. Approval
// and auto_l2 are routing decisions, not proof of causal validity or execution.
func TestRecordedPlansPolicyReplay(t *testing.T) {
	file, err := os.Open("results/2026-09-22-deepseek-v4-flash/results.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	registry := tools.NewRegistry()
	if err := registry.Register(tools.ToolSpec{
		Name: tools.ToolDockerRestart, Description: "Unreachable mutation sentinel", Level: tools.L2LowRisk, Timeout: time.Second,
		Handler: func(context.Context, json.RawMessage) (string, error) {
			t.Fatal("policy-only replay must never execute a mutation")
			return "", nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	captured, checked := 0, 0
	for scanner.Scan() {
		var row observation
		if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
			t.Fatal(err)
		}
		if row.CaseID != "oom_unidentified" {
			continue
		}
		if row.Error != "" || row.Result == nil {
			t.Fatal("captured OOM diagnosis is unavailable")
		}
		captured++
		guard := diagnose.Guard(row.Result.RCA, row.Result.Plan)
		for _, mode := range []struct {
			name       string
			auto, dry  bool
			container  string
			wantAction string
		}{
			{"dry_manual", false, true, "sub2api", approval.DecisionApproval},
			{"live_manual", false, false, "sub2api", approval.DecisionApproval},
			{"live_auto_no_recent_execution", true, false, "sub2api", approval.DecisionAutoL2},
			{"target_mismatch", true, false, "other-container", approval.DecisionDenied},
		} {
			t.Run(fmt.Sprintf("trial_%d/%s", row.Trial, mode.name), func(t *testing.T) {
				cfg := approval.PolicyConfig{AutoExecuteL2: mode.auto, DryRun: mode.dry,
					AllowedTargets: []string{mode.container}, Container: mode.container, HealthBaseURL: "http://127.0.0.1:8080",
					Verification: config.VerificationConfig{IntervalSeconds: 10, WindowSeconds: 120, TimeoutSeconds: 5},
					RateWindow:   time.Hour, MaxPerWindow: 1}
				input := approval.PolicyInput{Members: []incident.ExecutionMember{{Fingerprint: "synthetic-oom-alert",
					Name: incident.SupportedAlert, Status: "firing", Container: "sub2api", Service: "sub2api"}}}
				decision := approval.NewPolicy(registry, cfg, emptyExecutionHistory{}).Decide(context.Background(), guard.Plan, input)
				want := mode.wantAction
				if guard.Plan.Action == "none" {
					want = approval.DecisionNone
				}
				if decision.Kind != want {
					t.Fatalf("policy decision changed: got %s, want %s; reassess archived report", decision.Kind, want)
				}
				checked++
				t.Logf("case=%s trial=%d mode=%s suggested_action=%s guard=%s policy=%s reason=%q mutation_executed=false",
					row.CaseID, row.Trial, mode.name, row.Result.Plan.Action, guard.Decision, decision.Kind, decision.Reason)
			})
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if captured != 3 || checked != 12 {
		t.Fatalf("incomplete replay: %d captured diagnoses, %d policy decisions", captured, checked)
	}
}
