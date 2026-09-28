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
	"oncall-agent/internal/store"
	"oncall-agent/internal/tools"
)

// Fixed test fact: no emergency stop, busy service, budget use or block. This
// replay does not contact a database.
type emptyRemediationHistory struct{}

func (emptyRemediationHistory) RemediationState(context.Context, store.RemediationQuery) (store.RemediationState, error) {
	return store.RemediationState{}, nil
}

// Characterize the policy boundary using captured real-model outputs: under
// every rule mode, a restart suggested on an anonymous OOM never becomes an
// executable decision, because Guard removes it on current facts.
func TestRecordedPlansPolicyReplay(t *testing.T) {
	file, err := os.Open("results/2026-09-22-deepseek-v4-flash/results.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	registry := tools.NewRegistry()
	if err := registry.RegisterAction(sentinelRestart{t}); err != nil {
		t.Fatal(err)
	}
	var evidence diagnose.Evidence
	for _, c := range scenarios(t) {
		if c.ID == "oom_unidentified" {
			evidence = diagnose.Evidence{IncidentID: 1, Items: c.Evidence}
		}
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
		guard := diagnose.Guard(row.Result.RCA, row.Result.Plan, evidence)
		// Captured trials that suggested restarting sub2api on an OOM with no container
		// identity must be stopped: current facts do not identify the target.
		if row.Result.Plan.Action != "none" && (guard.Decision != diagnose.DecisionDeny || guard.Plan.Action != "none") {
			t.Fatalf("trial %d: anonymous OOM restart passed Guard: %+v", row.Trial, guard)
		}
		for _, mode := range []string{incident.ModeObserve, incident.ModeManual, incident.ModeAuto} {
			t.Run(fmt.Sprintf("trial_%d/%s", row.Trial, mode), func(t *testing.T) {
				remediation := config.RemediationConfig{RulesVersion: "replay", Rules: []config.RuleConfig{{ID: "restart", Action: tools.ActionDockerRestart,
					Mode: mode, Alerts: []string{"Sub2APIDown"}, MaxExecutions: 1, WindowMinutes: 60}},
					Verification: config.VerificationConfig{IntervalSeconds: 10, WindowSeconds: 120, TimeoutSeconds: 5, RequiredPasses: 1}}
				authority, err := approval.NewAuthority(config.ServiceConfig{Name: "sub2api", Env: "replay", Container: "sub2api"}, remediation, registry)
				if err != nil {
					t.Fatal(err)
				}
				input := approval.PolicyInput{IncidentID: 1, FaultAlert: "Sub2APIDown", Target: guard.Target, ObservationOK: true,
					Members: []incident.ExecutionMember{{Fingerprint: "synthetic-oom-alert", Name: "Sub2APIDown", Status: "firing", Service: "sub2api"}}}
				decision := approval.NewPolicy(authority, registry, 30*time.Minute, emptyRemediationHistory{}).Decide(context.Background(), guard.Plan, input)
				if decision.Kind != approval.DecisionNone || decision.PlanHash != "" {
					t.Fatalf("policy decision changed: got %s, want none; reassess archived report", decision.Kind)
				}
				checked++
				t.Logf("case=%s trial=%d mode=%s suggested_action=%s guard=%s policy=%s mutation_executed=false",
					row.CaseID, row.Trial, mode, row.Result.Plan.Action, guard.Decision, decision.Kind)
			})
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if captured != 3 || checked != 9 {
		t.Fatalf("incomplete replay: %d captured diagnoses, %d policy decisions", captured, checked)
	}
}
