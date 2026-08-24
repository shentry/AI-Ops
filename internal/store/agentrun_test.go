package store

import (
	"context"
	"testing"
	"time"

	"oncall-agent/internal/eventlog"
)

// agent_run 队列：入队事件、终态回写、CAS 领取与超时重排。

func TestCreateAgentRunWritesQueuedEvent(t *testing.T) {
	db := openIntegrationDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	incident := insertTestIncident(t, db, now, "manual-queued-event")
	run, err := db.CreateAgentRun(ctx, AgentRun{IncidentID: incident.ID, Mode: "light", Status: "pending", StartedAt: now})
	if err != nil {
		t.Fatalf("CreateAgentRun: %v", err)
	}
	t.Cleanup(func() {
		db.Where("run_id = ?", run.ID).Delete(&IncidentEvent{})
		db.Where("id = ?", run.ID).Delete(&AgentRun{})
		db.Where("id = ?", incident.ID).Delete(&Incident{})
		db.Close()
	})
	events, err := db.ListIncidentEvents(ctx, incident.ID, 0, 10)
	if err != nil {
		t.Fatalf("ListIncidentEvents: %v", err)
	}
	if len(events) != 1 || events[0].EventType != string(eventlog.EventRunQueued) || events[0].RunID == nil || *events[0].RunID != run.ID {
		t.Fatalf("events = %#v", events)
	}
}

func TestCompleteAgentRun(t *testing.T) {
	db := openIntegrationDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	run, err := db.CreateAgentRun(ctx, AgentRun{IncidentID: 1, Mode: "full", Status: "pending", StartedAt: now})
	if err != nil {
		t.Fatalf("CreateAgentRun() error = %v", err)
	}
	t.Cleanup(func() {
		db.Where("id = ?", run.ID).Delete(&AgentRun{})
		db.Close()
	})

	// 正常结论：RCA、Plan、token、终态一起落库。
	if err := db.CompleteAgentRun(ctx, run.ID, "root cause", []byte(`{"action":"none"}`), 120, 45, "succeeded", now.Add(time.Minute)); err != nil {
		t.Fatalf("CompleteAgentRun() error = %v", err)
	}
	var got AgentRun
	if err := db.First(&got, run.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != "succeeded" || got.TokensIn != 120 || got.TokensOut != 45 || got.RCAText == nil || *got.RCAText != "root cause" || got.PlanJSON == nil || got.FinishedAt == nil {
		t.Fatalf("agent run = %#v", got)
	}

	// 终态不可覆盖：审计记录不能被改写。
	if err := db.CompleteAgentRun(ctx, run.ID, "rewrite", nil, 0, 0, "failed", now.Add(2*time.Minute)); err == nil {
		t.Fatal("CompleteAgentRun(terminal) error = nil, want refusal")
	}
	if err := db.First(&got, run.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != "succeeded" || got.TokensIn != 120 {
		t.Fatalf("terminal run was rewritten: %#v", got)
	}

	// 非法终态与缺 id 直接拒绝。
	if err := db.CompleteAgentRun(ctx, run.ID, "", nil, 0, 0, "running", now); err == nil {
		t.Fatal("CompleteAgentRun(running) error = nil, want refusal")
	}
	if err := db.CompleteAgentRun(ctx, 0, "", nil, 0, 0, "succeeded", now); err == nil {
		t.Fatal("CompleteAgentRun(id=0) error = nil, want refusal")
	}
}

func TestAgentRunQueueMethods(t *testing.T) {
	db := openIntegrationDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 19, 13, 0, 0, 0, time.UTC)
	firstIncident := insertTestIncident(t, db, now, "queue-a")
	secondIncident := insertTestIncident(t, db, now, "queue-b")
	thirdIncident := insertTestIncident(t, db, now, "queue-c")
	var runIDs []uint64
	t.Cleanup(func() {
		if len(runIDs) > 0 {
			db.Where("run_id IN ?", runIDs).Delete(&AgentRunStep{})
			db.Where("run_id IN ?", runIDs).Delete(&IncidentEvent{})
			db.Where("run_id IN ?", runIDs).Delete(&IncidentProblem{})
			db.Where("id IN ?", runIDs).Delete(&AgentRun{})
		}
		db.Close()
	})

	first, err := db.CreateAgentRun(ctx, AgentRun{IncidentID: firstIncident.ID, Mode: "full", Status: "pending", StartedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	second, err := db.CreateAgentRun(ctx, AgentRun{IncidentID: secondIncident.ID, Mode: "light", Status: "pending", StartedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	runIDs = append(runIDs, first.ID, second.ID)

	// 队首是 id 最小的 pending。
	next, found, err := db.NextPendingAgentRun(ctx)
	if err != nil || !found || next.ID != first.ID {
		t.Fatalf("NextPendingAgentRun() = %v, %v, %v", next.ID, found, err)
	}

	// 认领是原子的：第二次认领同一行失败。
	// 认领时间取 10 分钟前，让后面的超时回补测试能把这条当 stale。
	claimed, err := db.ClaimAgentRun(ctx, first.ID, now.Add(-10*time.Minute))
	if err != nil || !claimed {
		t.Fatalf("ClaimAgentRun() = %v, %v", claimed, err)
	}
	again, err := db.ClaimAgentRun(ctx, first.ID, now)
	if err != nil || again {
		t.Fatalf("re-claim = %v, %v, want false", again, err)
	}
	// 队首前进到第二条。
	next, found, err = db.NextPendingAgentRun(ctx)
	if err != nil || !found || next.ID != second.ID {
		t.Fatalf("NextPendingAgentRun() after claim = %v, %v", next.ID, found)
	}

	// 超时 running 回补 pending；未超时的不动。
	recent, err := db.CreateAgentRun(ctx, AgentRun{IncidentID: thirdIncident.ID, Mode: "full", Status: "running", StartedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	runIDs = append(runIDs, recent.ID)
	requeued, err := db.RequeueStaleAgentRuns(ctx, now.Add(-5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if requeued == 0 {
		t.Fatal("RequeueStaleAgentRuns() = 0, want >= 1")
	}
	var firstRun AgentRun
	if err := db.First(&firstRun, first.ID).Error; err != nil {
		t.Fatal(err)
	}
	if firstRun.Status != "pending" {
		t.Fatalf("stale run status = %q, want pending after requeue", firstRun.Status)
	}
	var recentRun AgentRun
	if err := db.First(&recentRun, recent.ID).Error; err != nil {
		t.Fatal(err)
	}
	if recentRun.Status != "running" {
		t.Fatalf("recent running run was requeued: %q", recentRun.Status)
	}

	// step 落库与校验。
	finishedAt := now.Add(time.Second)
	if err := db.AppendRunStep(ctx, AgentRunStep{RunID: first.ID, Seq: 1, Kind: "evidence", Name: "collect", StartedAt: now, FinishedAt: &finishedAt}); err != nil {
		t.Fatal(err)
	}

	if err := db.AppendRunStep(ctx, AgentRunStep{RunID: 0, Seq: 1, Kind: "evidence", Name: "x", StartedAt: now}); err == nil {
		t.Fatal("AppendRunStep(runID=0) error = nil, want refusal")
	}
}
