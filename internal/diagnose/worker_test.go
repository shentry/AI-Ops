package diagnose

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"oncall-agent/internal/store"
)

// fakeRunQueue 模拟 agent_run 队列。
type fakeRunQueue struct {
	mu       sync.Mutex
	runs     []store.AgentRun
	claimed  []uint64
	requeued []uint64
	claimErr error
}

func (f *fakeRunQueue) NextPendingAgentRun(context.Context) (store.AgentRun, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, run := range f.runs {
		if run.Status == "pending" {
			return run, true, nil
		}
	}
	return store.AgentRun{}, false, nil
}

func (f *fakeRunQueue) ClaimAgentRun(_ context.Context, id uint64, _ time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.claimErr != nil {
		return false, f.claimErr
	}
	for i := range f.runs {
		if f.runs[i].ID == id && f.runs[i].Status == "pending" {
			f.runs[i].Status = "running"
			f.claimed = append(f.claimed, id)
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeRunQueue) RequeueStaleAgentRuns(_ context.Context, _ time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var count int64
	for i := range f.runs {
		if f.runs[i].Status == "running" {
			f.runs[i].Status = "pending"
			f.requeued = append(f.requeued, f.runs[i].ID)
			count++
		}
	}
	return count, nil
}

type recordingRunner struct {
	mu   sync.Mutex
	runs []store.AgentRun
	err  error
	done chan struct{}
}

func newRecordingRunner() *recordingRunner {
	return &recordingRunner{done: make(chan struct{})}
}

func (r *recordingRunner) Run(_ context.Context, run store.AgentRun) error {
	r.mu.Lock()
	r.runs = append(r.runs, run)
	r.mu.Unlock()
	select {
	case <-r.done:
	default:
		close(r.done)
	}
	return r.err
}

func TestDiagnoseWorkerDrainsQueue(t *testing.T) {
	queue := &fakeRunQueue{runs: []store.AgentRun{
		{ID: 1, IncidentID: 7, Mode: "full", Status: "pending"},
		{ID: 2, IncidentID: 8, Mode: "light", Status: "pending"},
	}}
	runner := newRecordingRunner()
	worker := NewWorker(queue, runner, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); worker.Wait() }()
	if err := worker.Start(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-runner.done:
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not consume runs")
	}
	// 两条 pending 都应被认领并执行。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		runner.mu.Lock()
		n := len(runner.runs)
		runner.mu.Unlock()
		if n == 2 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if len(runner.runs) != 2 {
		t.Fatalf("ran %d runs, want 2", len(runner.runs))
	}
}

func TestDiagnoseWorkerContinuesAfterRunFailure(t *testing.T) {
	queue := &fakeRunQueue{runs: []store.AgentRun{
		{ID: 1, IncidentID: 7, Mode: "full", Status: "pending"},
		{ID: 2, IncidentID: 8, Mode: "light", Status: "pending"},
	}}
	runner := newRecordingRunner()
	runner.err = errors.New("pipeline boom")
	worker := NewWorker(queue, runner, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); worker.Wait() }()
	if err := worker.Start(ctx); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		runner.mu.Lock()
		n := len(runner.runs)
		runner.mu.Unlock()
		if n == 2 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("worker stopped after pipeline failure")
}

func TestDiagnoseWorkerRequeuesStaleRunning(t *testing.T) {
	queue := &fakeRunQueue{runs: []store.AgentRun{
		{ID: 9, IncidentID: 7, Mode: "full", Status: "running", StartedAt: time.Now().Add(-10 * time.Minute)},
	}}
	runner := newRecordingRunner()
	worker := NewWorker(queue, runner, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); worker.Wait() }()
	if err := worker.Start(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-runner.done:
	case <-time.After(3 * time.Second):
		t.Fatal("stale run was not requeued and consumed")
	}
}
