package api

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"gorm.io/datatypes"
	"oncall-agent/internal/incident"
	"oncall-agent/internal/store"
)

// v3Snapshot is a complete restart snapshot whose private fields (check URL,
// member fingerprints) must never reach an API response.
func v3Snapshot(mode string, compensation *incident.Compensation) datatypes.JSON {
	raw, _ := json.Marshal(incident.ExecutionContext{Version: incident.ExecutionContextVersion, Kind: incident.KindPrimary, Service: "sub2api",
		Rule:          incident.RuleRef{ID: "restart", Version: "r1@000000000000", Mode: mode, Alerts: []string{"Sub2APIDown"}},
		ActionVersion: 2, Target: incident.Object{Kind: "container", Name: "sub2api", ID: "c0ffee"}, Revision: "started_at=2026-09-24T00:00:00Z",
		PreState: json.RawMessage(`{"status":"exited"}`), Members: []string{"private-member"}, FaultAlert: "Sub2APIDown",
		ExpiresAt: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC), Compensation: compensation,
		Verification: incident.VerificationSpec{Checks: []incident.Check{{Kind: incident.CheckHealth, Params: json.RawMessage(`{"base_url":"http://private-target.invalid:8080"}`)}},
			IntervalSeconds: 10, WindowSeconds: 120, TimeoutSeconds: 5, RequiredPasses: 1}})
	return raw
}

func completeApproval(t *testing.T) store.Approval {
	t.Helper()
	row := store.Approval{ID: 7, IncidentID: 11, RunID: 9, ToolName: "docker_restart", Status: "pending", Reason: "manual approval required", ExpiresAt: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC), ArgsJSON: datatypes.JSON(`{"target_kind":"container","target_name":"sub2api"}`), ExecutionContext: v3Snapshot(incident.ModeManual, nil)}
	var err error
	row.PlanHash, err = incident.PlanHash(row.ToolName, row.ArgsJSON, row.ExecutionContext)
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func approvalJSON(t *testing.T, row store.Approval) map[string]any {
	t.Helper()
	raw, err := json.Marshal(approvalDTO(row))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"args_json", "execution_context", "base_url", "private-target.invalid", "private-member", "last_result_json", `"risk"`} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("approval leaked %q: %s", forbidden, raw)
		}
	}
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestApprovalDTOExecutionSnapshot(t *testing.T) {
	for _, mode := range []string{incident.ModeManual, incident.ModeAuto} {
		row := completeApproval(t)
		compensation := &incident.Compensation{Action: "upstream_restore", ActionVersion: 1, Args: json.RawMessage(`{"account_id":7}`), Revision: "r",
			Checks: []incident.Check{{Kind: incident.CheckAccount, Params: json.RawMessage(`{"account_id":7,"schedulable":true}`)}}}
		row.ExecutionContext = v3Snapshot(mode, compensation)
		row.PlanHash, _ = incident.PlanHash(row.ToolName, row.ArgsJSON, row.ExecutionContext)
		result := approvalJSON(t, row)
		if result["target"] != "container/sub2api" || result["target_id"] != "c0ffee" || result["rule_id"] != "restart" || result["rule_version"] != "r1@000000000000" ||
			result["mode"] != mode || result["kind"] != "primary" || result["compensation"] != "upstream_restore" || result["revision"] != "started_at=2026-09-24T00:00:00Z" {
			t.Fatalf("snapshot projection = %#v", result)
		}
		if checks := result["checks"].([]any); len(checks) != 1 || checks[0] != "health" {
			t.Fatalf("checks = %#v", result["checks"])
		}
		if result["verification"].(map[string]any)["status"] != "not_started" {
			t.Fatalf("verification = %#v", result)
		}
	}
}

func TestApprovalDTOIncompleteSnapshotIsUnknown(t *testing.T) {
	for _, name := range []string{"legacy", "malformed", "older_version", "wrong_hash", "wrong_target"} {
		t.Run(name, func(t *testing.T) {
			row := completeApproval(t)
			switch name {
			case "legacy":
				row.ExecutionContext = nil
			case "malformed":
				row.ExecutionContext = datatypes.JSON(`{"version":3}`)
			case "older_version":
				row.ExecutionContext = datatypes.JSON(`{"version":2,"safety_level":"L2","dry_run":false}`)
			case "wrong_hash":
				row.PlanHash = "outdated"
			case "wrong_target":
				row.ArgsJSON = datatypes.JSON(`{"target_kind":"container","target_name":"other"}`)
			}
			result := approvalJSON(t, row)
			for _, key := range []string{"target", "target_id", "rule_id", "mode", "checks"} {
				if v := result[key]; v != nil && v != "" {
					t.Fatalf("incomplete snapshot inferred %s=%v", key, v)
				}
			}
			if result["verification"].(map[string]any)["status"] != "unknown" {
				t.Fatalf("verification = %#v", result)
			}
		})
	}
}

// The structured receipt is shown; the verification phase distinguishes
// deciding recovery from watching for a recurrence.
func TestApprovalDTOReceiptAndPhase(t *testing.T) {
	row := completeApproval(t)
	row.Status = "executed"
	result := datatypes.JSON(`{"action":"docker_restart","operation_id":"op-7","written":true,"outcome":"written","before":"started_at=a","after":"started_at=b","detail":"restarted"}`)
	row.ResultJSON = &result
	row.Verification = &store.VerifyTask{ApprovalID: 7, Status: "pending", Phase: "watch", DeadlineAt: time.Date(2030, 1, 1, 0, 30, 0, 0, time.UTC)}
	got := approvalJSON(t, row)
	receipt := got["result"].(map[string]any)
	if receipt["written"] != true || receipt["outcome"] != "written" || receipt["before"] != "started_at=a" || receipt["after"] != "started_at=b" {
		t.Fatalf("receipt = %#v", receipt)
	}
	if got["verification"].(map[string]any)["phase"] != "watch" {
		t.Fatalf("verification = %#v", got["verification"])
	}
}

func TestApprovalDTOVerification(t *testing.T) {
	for _, status := range []string{"pending", "running", "passed", "failed", "inconclusive"} {
		t.Run(status, func(t *testing.T) {
			row := completeApproval(t)
			row.Status = "executed"
			checked := time.Date(2026, 8, 24, 1, 2, 3, 0, time.FixedZone("offset", 3600))
			deadline := checked.Add(time.Minute)
			row.Verification = &store.VerifyTask{ApprovalID: row.ID, Status: status, LastCheckedAt: &checked, DeadlineAt: deadline, LastResultJSON: datatypes.JSON(`{"detail":"health check token=private-secret", "base_url":"http://private-target.invalid:8080", "raw":"private-result"}`)}
			result := approvalJSON(t, row)
			verification := result["verification"].(map[string]any)
			if verification["status"] != status || verification["last_checked_at"] != checked.UTC().Format(time.RFC3339) || verification["deadline_at"] != deadline.UTC().Format(time.RFC3339) {
				t.Fatalf("verification = %#v", verification)
			}
			raw, _ := json.Marshal(verification)
			if strings.Contains(string(raw), "private-secret") || strings.Contains(string(raw), "private-result") {
				t.Fatalf("leaked task result: %s", raw)
			}
			if !strings.Contains(verification["detail"].(string), "health check") {
				t.Fatalf("missing safe detail: %s", raw)
			}
		})
	}
	for _, status := range []string{"pending", "approved", "executing", "executed", "failed", "aborted", "denied", "expired"} {
		row := completeApproval(t)
		row.Status = status
		expected := "unknown"
		switch status {
		case "pending", "approved", "executing":
			expected = "not_started"
		case "aborted", "denied", "expired", "failed":
			expected = "not_applicable"
		}
		result := approvalJSON(t, row)
		if result["verification"].(map[string]any)["status"] != expected {
			t.Fatalf("%s no task: %#v", status, result)
		}
	}
}
