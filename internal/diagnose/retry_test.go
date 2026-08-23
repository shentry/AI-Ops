package diagnose

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"gorm.io/datatypes"

	"oncall-agent/internal/llm"
	"oncall-agent/internal/notify"
	"oncall-agent/internal/store"
)

type fakeRetryStore struct {
	runs      map[uint64]store.AgentRun
	created   []store.AgentRun
	hasActive bool
}

func newFakeRetryStore(runs ...store.AgentRun) *fakeRetryStore {
	f := &fakeRetryStore{runs: map[uint64]store.AgentRun{}}
	for _, run := range runs {
		f.runs[run.ID] = run
	}
	return f
}

func (f *fakeRetryStore) GetAgentRun(_ context.Context, id uint64) (store.AgentRun, error) {
	if run, ok := f.runs[id]; ok {
		return run, nil
	}
	return store.AgentRun{}, fmt.Errorf("store: agent run %d not found", id)
}

func (f *fakeRetryStore) CreateRetryAgentRun(_ context.Context, run store.AgentRun, _ string) (store.AgentRun, bool, error) {
	if f.hasActive {
		return store.AgentRun{}, false, nil
	}
	run.ID = uint64(len(f.runs) + len(f.created) + 1)
	f.created = append(f.created, run)
	f.runs[run.ID] = run
	return run, true, nil
}

func (f *fakeRetryStore) ListIncidentRunIDs(context.Context, uint64) ([]uint64, error) {
	ids := make([]uint64, 0, len(f.runs))
	for id := range f.runs {
		ids = append(ids, id)
	}
	return ids, nil
}

type fakeEscalationNotifier struct {
	calls    int
	messages []notify.Notification
}

func (f *fakeEscalationNotifier) Send(_ context.Context, msg notify.Notification) (notify.Delivery, error) {
	f.calls++
	f.messages = append(f.messages, msg)
	return notify.Delivery{Provider: "fake"}, nil
}

func TestScheduleRetryCreatesRetryOfRun(t *testing.T) {
	db := newFakeRetryStore(store.AgentRun{ID: 10, Status: "failed"})
	notifier := &fakeEscalationNotifier{}
	scheduler := NewRetryScheduler(db, notifier, 2)

	created, err := scheduler.ScheduleRetry(context.Background(), 7, 10, "verify failed: 1/2 members still firing")
	if err != nil || !created {
		t.Fatalf("ScheduleRetry() = %v, %v", created, err)
	}
	retry := db.created[0]
	if retry.RetryOf == nil || *retry.RetryOf != 10 || retry.Status != "pending" || retry.Mode != "full" {
		t.Fatalf("retry run = %+v", retry)
	}
	if notifier.calls != 0 {
		t.Fatalf("escalated unexpectedly: %d", notifier.calls)
	}
}

func TestScheduleRetrySkipsWhenActiveRunExists(t *testing.T) {
	db := newFakeRetryStore(store.AgentRun{ID: 10})
	db.hasActive = true
	scheduler := NewRetryScheduler(db, &fakeEscalationNotifier{}, 2)
	created, err := scheduler.ScheduleRetry(context.Background(), 7, 10, "x")
	if err != nil || created {
		t.Fatalf("ScheduleRetry() = %v, %v, want false,nil", created, err)
	}
	if len(db.created) != 0 {
		t.Fatal("concurrent retry created")
	}
}

func TestScheduleRetryEscalatesAfterChainBudget(t *testing.T) {
	// 链：10（首诊）← 11（重试 1）← 12（重试 2）。12 失败 = 第三次失败，升级人工。
	link := func(id, parent uint64) store.AgentRun {
		r := store.AgentRun{ID: id, IncidentID: 7, Status: "failed"}
		if parent != 0 {
			r.RetryOf = &parent
		}
		return r
	}
	db := newFakeRetryStore(link(10, 0), link(11, 10), link(12, 11))
	notifier := &fakeEscalationNotifier{}
	scheduler := NewRetryScheduler(db, notifier, 2)
	created, err := scheduler.ScheduleRetry(context.Background(), 7, 12, "verify failed again")
	if err != nil || created {
		t.Fatalf("ScheduleRetry() = %v, %v", created, err)
	}
	if len(db.created) != 0 {
		t.Fatal("fourth run created beyond budget")
	}
	if notifier.calls != 1 {
		t.Fatalf("escalations = %d, want 1", notifier.calls)
	}
	msg := notifier.messages[0]
	runIDs, _ := msg.Payload["run_ids"].([]uint64)
	if msg.IncidentID != 7 || len(runIDs) != 3 || msg.Summary == "" || msg.Kind != notify.NotificationEscalationRequired {
		t.Fatalf("escalation = %+v", msg)
	}
}

func TestScheduleRetryManualRunDoesNotConsumeBudget(t *testing.T) {
	// 人工重诊（retry_of 为空）各自成链：两条人工 run 不该挤占自动预算。
	db := newFakeRetryStore(
		store.AgentRun{ID: 10, IncidentID: 7, Status: "failed"},
		store.AgentRun{ID: 11, IncidentID: 7, Status: "failed"}, // 人工重诊
		store.AgentRun{ID: 12, IncidentID: 7, Status: "failed"}, // 人工重诊
	)
	notifier := &fakeEscalationNotifier{}
	scheduler := NewRetryScheduler(db, notifier, 2)
	// 对其中一条失败 run 触发重诊：链长 1，仍在预算内。
	created, err := scheduler.ScheduleRetry(context.Background(), 7, 12, "verify failed")
	if err != nil || !created {
		t.Fatalf("ScheduleRetry() = %v, %v, want created", created, err)
	}
	if notifier.calls != 0 {
		t.Fatalf("manual runs consumed auto budget, escalated %d times", notifier.calls)
	}
}

func TestPipelineInjectsRetryContext(t *testing.T) {
	db := newFakeRunStore()
	rca := "上次认为容器退出"
	planJSON := []byte(`{"action":"restart_container"}`)
	db.runs[10] = store.AgentRun{ID: 10, Status: "failed", RCAText: &rca, PlanJSON: (*datatypes.JSON)(&planJSON)}
	// 上一轮 run 的 verify step 带失败详情。
	verifyOutput := datatypes.JSON(`"passed=false detail=1/2 members still firing"`)
	db.steps = append(db.steps, store.AgentRunStep{RunID: 10, Seq: 90, Kind: "verify", Name: "last_alert_recheck", OutputJSON: &verifyOutput})
	reasoner := &fakeReasoner{result: &llm.DiagnoseResult{RCA: "新结论", Confidence: "medium"}}
	pipeline := NewPipeline(db, fakeEvidenceBuilder{evidence: testEvidence()}, reasoner, allowPolicy(), &fakeApprovals{}, &fakeReporter{}, nil, 0)
	retryOf := uint64(10)
	run := store.AgentRun{ID: 11, IncidentID: 7, Mode: "full", Status: "running", RetryOf: &retryOf}
	if err := pipeline.Run(context.Background(), run); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	ctx := pipeline.retryContext(context.Background(), 10)
	for _, want := range []string{"上次认为容器退出", "previous_run_id: 10", "restart_container", "previous_verify", "1/2 members still firing"} {
		if !strings.Contains(ctx, want) {
			t.Fatalf("retry context missing %q:\n%s", want, ctx)
		}
	}
}
