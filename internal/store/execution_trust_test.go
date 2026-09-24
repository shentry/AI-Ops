package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"gorm.io/datatypes"
	"oncall-agent/internal/incident"
)

func executionFixture(t *testing.T, db *DB, now time.Time, dryRun bool, status string) (Approval, incident.ExecutionBinding) {
	t.Helper()
	parent := insertTestIncident(t, db, now, "execution-trust")
	fp := sha256Hex(parent.GroupKey)
	labels, _ := json.Marshal(map[string]string{"service": "sub2api", "container": "sub2api"})
	alert := Alert{Fingerprint: fp, AlertHash: md5Hex(fp), Source: "alertmanager", Name: incident.SupportedAlert, Severity: 5, Status: "firing", Labels: labels, Annotations: datatypes.JSON(`{}`), StartsAt: now, ReceivedAt: now}
	if err := db.Create(&alert).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&LastAlert{Fingerprint: fp, AlertID: alert.ID, AlertHash: alert.AlertHash, Status: "firing", Severity: 5, FirstSeen: now, LastSeen: now, IncidentID: &parent.ID}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&IncidentAlert{IncidentID: parent.ID, Fingerprint: fp, LinkedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Exec("DELETE FROM verify_task WHERE approval_id IN (SELECT id FROM approval WHERE incident_id = ?)", parent.ID)
		db.Exec("DELETE FROM agent_run_step WHERE run_id IN (SELECT id FROM agent_run WHERE incident_id = ?)", parent.ID)
		db.Exec("DELETE FROM fault_cmd_history WHERE approval_id IN (SELECT id FROM approval WHERE incident_id = ?)", parent.ID)
		db.Where("incident_id = ?", parent.ID).Delete(&Approval{})
		db.Where("incident_id = ?", parent.ID).Delete(&AgentRun{})
		db.Where("incident_id = ?", parent.ID).Delete(&IncidentAlert{})
		db.Where("fingerprint = ?", fp).Delete(&LastAlert{})
		db.Where("id = ?", alert.ID).Delete(&Alert{})
		db.Where("fingerprint = ?", incident.FaultFingerprint(parent.GroupKey, incident.SupportedAlert)).Delete(&FaultMemory{})
	})
	run, _, err := db.RequestRun(context.Background(), RunRequest{IncidentID: parent.ID, Mode: "full", Trigger: RunTriggerAlert, RequestedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, _ := json.Marshal(incident.ExecutionContext{SafetyLevel: "L2", DryRun: dryRun, Verification: incident.VerificationSpec{Kind: incident.HealthVerification, TargetName: "sub2api", BaseURL: "http://127.0.0.1:8080", MemberFingerprints: []string{fp}, IntervalSeconds: 10, WindowSeconds: 120, TimeoutSeconds: 5}})
	args := datatypes.JSON(`{"target_kind":"container","target_name":"sub2api"}`)
	hash, err := incident.PlanHash(incident.RestartAction, args, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	draft := Approval{IncidentID: parent.ID, RunID: run.ID, ToolName: incident.RestartAction, ArgsJSON: args, ExecutionContext: snapshot, PlanHash: hash, Reason: "test approval snapshot", Status: status, ExpiresAt: now.Add(time.Hour), CreatedAt: now}
	if status == "approved" {
		actor, source := "system:auto_l2", "system"
		draft.DecidedBy, draft.DecisionSource, draft.DecidedAt = &actor, &source, &now
	}
	guard := datatypes.JSON(`"decision=allow overridden=false reason="`)
	completion := RunCompletion{RunID: run.ID, Status: "succeeded", RCA: "supported outage", PlanJSON: []byte(`{"action":"docker_restart","confidence":"high"}`), FinishedAt: now, Approval: &draft, Steps: []AgentRunStep{{RunID: run.ID, Seq: 4, Kind: "guard", Name: "rules", OutputJSON: &guard, StartedAt: now, FinishedAt: &now}}}
	if err := db.CompleteRun(context.Background(), completion); err != nil {
		t.Fatal(err)
	}
	if draft.ID == 0 {
		t.Fatal("diagnosis completion did not publish its approval")
	}
	return draft, incident.ExecutionBinding{Container: "sub2api", BaseURL: "http://127.0.0.1:8080", AllowedContainers: []string{"sub2api"}, SafetyLevel: "L2", DryRun: dryRun}
}

func TestDiagnosisPublishesApprovalWithResult(t *testing.T) {
	db := openIntegrationDB(t)
	t.Cleanup(func() { db.Close() })
	draft, _ := executionFixture(t, db, time.Now().UTC().Truncate(time.Millisecond), false, "approved")
	approval, err := db.GetApproval(context.Background(), draft.ID)
	if err != nil || approval.Status != "approved" || approval.PlanHash != draft.PlanHash {
		t.Fatalf("approval=%+v err=%v", approval, err)
	}
	run, err := db.GetAgentRun(context.Background(), draft.RunID)
	if err != nil || run.Status != "succeeded" || run.PlanJSON == nil {
		t.Fatalf("run=%+v err=%v", run, err)
	}
}

func TestFinishExecutionAtomicallyQueuesVerificationAndIsIdempotent(t *testing.T) {
	for _, dryRun := range []bool{false, true} {
		t.Run(map[bool]string{false: "real", true: "simulated"}[dryRun], func(t *testing.T) {
			db := openIntegrationDB(t)
			t.Cleanup(func() { db.Close() })
			ctx := context.Background()
			now := time.Now().UTC().Truncate(time.Millisecond)
			approval, binding := executionFixture(t, db, now, dryRun, "approved")
			_, claimed, err := db.ClaimApprovalExecution(ctx, approval.ID, now, binding)
			if err != nil || !claimed {
				t.Fatalf("claim=%v %v", claimed, err)
			}
			status := "executed"
			if dryRun {
				status = "simulated"
			}
			result, _ := json.Marshal(map[string]any{"output": "ok", "dry_run": dryRun, "executed": !dryRun})
			completion := ExecutionCompletion{ApprovalID: approval.ID, Status: status, ResultJSON: result, FinishedAt: now}
			if err := db.FinishExecution(ctx, completion); err != nil {
				t.Fatal(err)
			}
			got, err := db.GetApproval(ctx, approval.ID)
			if err != nil || got.Status != status || (got.Verification != nil) == dryRun {
				t.Fatalf("approval=%+v err=%v", got, err)
			}
			before, _ := db.ListIncidentEvents(ctx, approval.IncidentID, 0, 100)
			if err := db.FinishExecution(ctx, completion); err != nil {
				t.Fatal(err)
			}
			after, _ := db.ListIncidentEvents(ctx, approval.IncidentID, 0, 100)
			if len(before) != len(after) {
				t.Fatal("duplicate completion wrote more events")
			}
			completion.ResultJSON = []byte(`{"output":"different"}`)
			if err := db.FinishExecution(ctx, completion); err == nil {
				t.Fatal("conflicting completion accepted")
			}
		})
	}
}

func TestVerificationQueueProgressAndTerminalReplay(t *testing.T) {
	db := openIntegrationDB(t)
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	approval, binding := executionFixture(t, db, now, false, "approved")
	if _, ok, err := db.ClaimApprovalExecution(ctx, approval.ID, now, binding); err != nil || !ok {
		t.Fatalf("claim=%v %v", ok, err)
	}
	if err := db.FinishExecution(ctx, ExecutionCompletion{ApprovalID: approval.ID, Status: "executed", ResultJSON: []byte(`{"output":"ok"}`), FinishedAt: now}); err != nil {
		t.Fatal(err)
	}
	task, found, err := db.NextVerificationTask(ctx, now)
	if err != nil || !found || task.ApprovalID != approval.ID {
		t.Fatalf("task=%+v found=%v err=%v", task, found, err)
	}
	task, claimed, err := db.ClaimVerificationTask(ctx, approval.ID, now)
	if err != nil || !claimed || task.ClaimedAt == nil {
		t.Fatalf("claim=%+v %v %v", task, claimed, err)
	}
	completion := VerificationCompletion{ApprovalID: approval.ID, ClaimedAt: *task.ClaimedAt, CheckedAt: now, Status: "pending", NextCheckAt: now.Add(10 * time.Second), Observation: "unhealthy", Detail: "HTTP 503", Binding: binding}
	if _, err := db.FinalizeVerification(ctx, completion); err != nil {
		t.Fatal(err)
	}
	if _, found, err := db.NextVerificationTask(ctx, now); err != nil || found {
		t.Fatalf("not due: found=%v err=%v", found, err)
	}
	task, claimed, err = db.ClaimVerificationTask(ctx, approval.ID, now.Add(10*time.Second))
	if err != nil || !claimed {
		t.Fatalf("claim=%v %v", claimed, err)
	}
	completion.ClaimedAt = *task.ClaimedAt
	completion.CheckedAt = now.Add(10 * time.Second)
	completion.Status = "passed"
	completion.Observation = "healthy"
	completion.Detail = "HTTP 200"
	result, err := db.FinalizeVerification(ctx, completion)
	if err != nil || !result.Applied || result.Status != "passed" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if result, err := db.FinalizeVerification(ctx, completion); err != nil || result.Applied {
		t.Fatalf("duplicate=%+v err=%v", result, err)
	}
	got, err := db.GetApproval(ctx, approval.ID)
	if err != nil || got.Status != "executed" || got.Verification.Status != "passed" {
		t.Fatalf("approval=%+v err=%v", got, err)
	}
}
