package store

import (
	"context"
	"testing"
	"time"

	"oncall-agent/internal/eventlog"
)

func TestRequestRunWritesQueuedEvent(t *testing.T) {
	db := openIntegrationDB(t)
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	parent := insertTestIncident(t, db, now, "queued-event")
	run, _, err := db.RequestRun(ctx, RunRequest{IncidentID: parent.ID, Mode: "light", Trigger: RunTriggerManual, RequestedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Where("id = ?", run.ID).Delete(&AgentRun{}) })
	events, err := db.ListIncidentEvents(ctx, parent.ID, 0, 10)
	if err != nil || len(events) != 1 || events[0].EventType != string(eventlog.EventRunQueued) || events[0].RunID == nil || *events[0].RunID != run.ID {
		t.Fatalf("events=%+v err=%v", events, err)
	}
}

func TestCompleteRunPreservesTerminalResults(t *testing.T) {
	db := openIntegrationDB(t)
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	parent := insertTestIncident(t, db, now, "complete-run")
	run, _, err := db.RequestRun(ctx, RunRequest{IncidentID: parent.ID, Mode: "full", Trigger: RunTriggerManual, RequestedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Where("id = ?", run.ID).Delete(&AgentRun{}) })
	completion := RunCompletion{RunID: run.ID, RCA: "root cause", PlanJSON: []byte(`{"action":"none"}`), TokensIn: 120, TokensOut: 45, Status: "succeeded", FinishedAt: now.Add(time.Minute)}
	if err := db.CompleteRun(ctx, completion); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetAgentRun(ctx, run.ID)
	if err != nil || got.Status != "succeeded" || got.TokensIn != 120 || got.TokensOut != 45 || got.RCAText == nil || *got.RCAText != "root cause" || got.PlanJSON == nil || got.FinishedAt == nil {
		t.Fatalf("run=%+v err=%v", got, err)
	}
	completion.Status = "failed"
	completion.RCA = "rewrite"
	if err := db.CompleteRun(ctx, completion); err == nil {
		t.Fatal("terminal run overwritten")
	}
	got, err = db.GetAgentRun(ctx, run.ID)
	if err != nil || got.Status != "succeeded" || *got.RCAText != "root cause" {
		t.Fatalf("run=%+v err=%v", got, err)
	}
	completion.Status = "running"
	if err := db.CompleteRun(ctx, completion); err == nil {
		t.Fatal("invalid terminal status accepted")
	}
	completion.RunID = 0
	completion.Status = "succeeded"
	if err := db.CompleteRun(ctx, completion); err == nil {
		t.Fatal("empty id accepted")
	}
}

func TestAgentRunQueueMethods(t *testing.T) {
	db := openIntegrationDB(t)
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	var runs []AgentRun
	for _, suffix := range []string{"queue-a", "queue-b", "queue-c"} {
		parent := insertTestIncident(t, db, now, suffix)
		run, _, err := db.RequestRun(ctx, RunRequest{IncidentID: parent.ID, Mode: "full", Trigger: RunTriggerAlert, RequestedAt: now})
		if err != nil {
			t.Fatal(err)
		}
		runs = append(runs, run)
		t.Cleanup(func() { db.Where("id = ?", run.ID).Delete(&AgentRun{}) })
	}
	first, found, err := db.NextPendingAgentRun(ctx)
	if err != nil || !found || first.ID != runs[0].ID {
		t.Fatalf("next=%+v %v %v", first, found, err)
	}
	if claimed, err := db.ClaimAgentRun(ctx, runs[0].ID, now.Add(-10*time.Minute)); err != nil || !claimed {
		t.Fatalf("claim=%v %v", claimed, err)
	}
	if claimed, err := db.ClaimAgentRun(ctx, runs[0].ID, now); err != nil || claimed {
		t.Fatalf("duplicate claim=%v %v", claimed, err)
	}
	if claimed, err := db.ClaimAgentRun(ctx, runs[2].ID, now); err != nil || !claimed {
		t.Fatalf("recent claim=%v %v", claimed, err)
	}
	if requeued, err := db.RequeueStaleAgentRuns(ctx, now.Add(-5*time.Minute)); err != nil || requeued != 1 {
		t.Fatalf("requeue=%d %v", requeued, err)
	}
	for index, want := range []string{"pending", "pending", "running"} {
		got, err := db.GetAgentRun(ctx, runs[index].ID)
		if err != nil || got.Status != want {
			t.Fatalf("run=%+v want=%s err=%v", got, want, err)
		}
	}
	events, err := db.ListIncidentEvents(ctx, runs[0].IncidentID, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || events[2].EventType != string(eventlog.EventRunStalled) {
		t.Fatalf("events=%+v", events)
	}
}
