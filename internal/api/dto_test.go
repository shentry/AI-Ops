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

func completeApproval(t *testing.T) store.Approval {
	t.Helper()
	row := store.Approval{ID: 7, IncidentID: 11, RunID: 9, ToolName: "docker_restart", Status: "pending", Reason: "manual approval required", ExpiresAt: time.Now().UTC().Add(time.Hour), ArgsJSON: datatypes.JSON(`{"target_kind":"container","target_name":"sub2api"}`), ExecutionContext: datatypes.JSON(`{"safety_level":"L2","dry_run":false,"verification":{"kind":"sub2api_http_health","target_name":"sub2api","base_url":"http://private-target.invalid:8080","member_fingerprints":["private-member"],"interval_seconds":10,"window_seconds":120,"timeout_seconds":5}}`)}
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
	for _, dryRun := range []bool{false, true} {
		row := completeApproval(t)
		if dryRun {
			row.ExecutionContext = datatypes.JSON(strings.Replace(string(row.ExecutionContext), `"dry_run":false`, `"dry_run":true`, 1))
			row.PlanHash, _ = incident.PlanHash(row.ToolName, row.ArgsJSON, row.ExecutionContext)
		}
		result := approvalJSON(t, row)
		if result["target"] != "container/sub2api" || result["scope"] != "single_container" || result["safety_level"] != "L2" || result["dry_run"] != dryRun {
			t.Fatalf("snapshot projection = %#v", result)
		}
		if result["verification"].(map[string]any)["status"] != "not_started" {
			t.Fatalf("verification = %#v", result)
		}
	}
}

func TestApprovalDTOIncompleteSnapshotIsUnknown(t *testing.T) {
	for _, name := range []string{"legacy", "malformed", "missing_bool", "wrong_hash", "wrong_target"} {
		t.Run(name, func(t *testing.T) {
			row := completeApproval(t)
			switch name {
			case "legacy":
				row.ExecutionContext = nil
			case "malformed":
				row.ExecutionContext = datatypes.JSON(`{"dry_run":false}`)
			case "missing_bool":
				row.ExecutionContext = datatypes.JSON(strings.Replace(string(row.ExecutionContext), `"dry_run":false,`, "", 1))
			case "wrong_hash":
				row.PlanHash = "outdated"
			case "wrong_target":
				row.ArgsJSON = datatypes.JSON(`{"target_kind":"container","target_name":"other"}`)
			}
			result := approvalJSON(t, row)
			for _, key := range []string{"target", "scope", "safety_level", "dry_run"} {
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
	for _, status := range []string{"pending", "approved", "executing", "executed", "failed", "denied", "expired", "simulated"} {
		row := completeApproval(t)
		row.Status = status
		expected := "unknown"
		switch status {
		case "pending", "approved", "executing":
			expected = "not_started"
		case "simulated", "denied", "expired", "failed":
			expected = "not_applicable"
		}
		result := approvalJSON(t, row)
		if result["verification"].(map[string]any)["status"] != expected {
			t.Fatalf("%s no task: %#v", status, result)
		}
	}
}
