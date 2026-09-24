package approval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"oncall-agent/internal/incident"
	"oncall-agent/internal/store"
	"oncall-agent/internal/tools"
)

// fakeExecStore controls only the external persistence seam. In particular it
// does not pretend to prove MySQL locking, atomic side effects or scope checks.
// Claims are deliberately permissive to exercise the executor's fail-closed guard.
type fakeExecStore struct {
	rows          []store.Approval
	bindings      []incident.ExecutionBinding
	attempts      []store.ExecutionCompletion
	committed     map[uint64]store.ExecutionCompletion
	nextErr       error
	claimErr      error
	recoveryErr   error
	recoveryCalls int
	claim         func(*store.Approval, incident.ExecutionBinding) bool
	finish        func(store.ExecutionCompletion, int) error
}

func newFakeExecStore(rows ...store.Approval) *fakeExecStore {
	return &fakeExecStore{rows: rows, committed: make(map[uint64]store.ExecutionCompletion)}
}

func (f *fakeExecStore) NextApprovedApproval(ctx context.Context, _ time.Time) (store.Approval, bool, error) {
	if err := ctx.Err(); err != nil {
		return store.Approval{}, false, err
	}
	if f.nextErr != nil {
		return store.Approval{}, false, f.nextErr
	}
	for _, row := range f.rows {
		if row.Status == "approved" {
			return row, true, nil
		}
	}
	return store.Approval{}, false, nil
}

func (f *fakeExecStore) ClaimApprovalExecution(_ context.Context, id uint64, _ time.Time, binding incident.ExecutionBinding) (store.Approval, bool, error) {
	f.bindings = append(f.bindings, binding)
	if f.claimErr != nil {
		return store.Approval{}, false, f.claimErr
	}
	for i := range f.rows {
		row := &f.rows[i]
		if row.ID != id || row.Status != "approved" {
			continue
		}
		if f.claim != nil && !f.claim(row, binding) {
			return *row, false, nil
		}
		row.Status = "executing"
		return *row, true, nil
	}
	return store.Approval{}, false, nil
}

func (f *fakeExecStore) FinishExecution(_ context.Context, completion store.ExecutionCompletion) error {
	completion.ResultJSON = append([]byte(nil), completion.ResultJSON...)
	f.attempts = append(f.attempts, completion)
	if f.finish != nil {
		if err := f.finish(completion, len(f.attempts)); err != nil {
			return err
		}
	}
	return f.commit(completion)
}

func (f *fakeExecStore) commit(completion store.ExecutionCompletion) error {
	if previous, ok := f.committed[completion.ApprovalID]; ok {
		if previous.Status != completion.Status || !bytes.Equal(previous.ResultJSON, completion.ResultJSON) {
			return store.ErrApprovalConflict
		}
		return nil
	}
	f.committed[completion.ApprovalID] = completion
	for i := range f.rows {
		if f.rows[i].ID == completion.ApprovalID {
			f.rows[i].Status = completion.Status
		}
	}
	return nil
}

func (f *fakeExecStore) RecoverExecutingApprovals(_ context.Context, _ time.Time) (int64, error) {
	f.recoveryCalls++
	if f.recoveryErr != nil {
		return 0, f.recoveryErr
	}
	var recovered int64
	for i := range f.rows {
		if f.rows[i].Status == "executing" {
			f.rows[i].Status = "failed"
			recovered++
		}
	}
	return recovered, nil
}

func executorBinding() incident.ExecutionBinding {
	return incident.ExecutionBinding{
		Container: "sub2api", BaseURL: "http://127.0.0.1:8080",
		AllowedContainers: []string{"sub2api"}, SafetyLevel: "L2",
	}
}

func executorSnapshot(dryRun bool) incident.ExecutionContext {
	return incident.ExecutionContext{
		SafetyLevel: "L2", DryRun: dryRun,
		Verification: incident.VerificationSpec{
			Kind: incident.HealthVerification, TargetName: "sub2api", BaseURL: "http://127.0.0.1:8080",
			MemberFingerprints: []string{"fp1"}, IntervalSeconds: 10, WindowSeconds: 120, TimeoutSeconds: 5,
		},
	}
}

func approvedApproval(t *testing.T, id uint64, dryRun bool) store.Approval {
	t.Helper()
	row := store.Approval{
		ID: id, IncidentID: id + 100, RunID: id + 200, Status: "approved", ToolName: incident.RestartAction,
		ArgsJSON: []byte(`{"target_kind":"container","target_name":"sub2api"}`), ExpiresAt: time.Now().Add(time.Hour),
	}
	setExecutorSnapshot(t, &row, executorSnapshot(dryRun))
	return row
}

func setExecutorSnapshot(t *testing.T, row *store.Approval, snapshot incident.ExecutionContext) {
	t.Helper()
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	row.ExecutionContext = raw
	row.PlanHash, err = incident.PlanHash(row.ToolName, row.ArgsJSON, row.ExecutionContext)
	if err != nil {
		t.Fatal(err)
	}
}

func executorTestRegistry(t *testing.T, level tools.SafetyLevel, handler tools.Handler) *tools.Registry {
	t.Helper()
	registry := tools.NewRegistry()
	if level == "" {
		return registry
	}
	if err := registry.Register(tools.ToolSpec{
		Name: incident.RestartAction, Description: "test restart spy", Level: level,
		Timeout: time.Second, MaxOutput: 100000, Handler: handler,
	}); err != nil {
		t.Fatal(err)
	}
	return registry
}

func quietExecutor(db execStore, registry *tools.Registry, binding incident.ExecutionBinding) *Executor {
	return NewExecutor(db, registry, binding, log.New(io.Discard, "", 0))
}

func executionResult(t *testing.T, completion store.ExecutionCompletion) map[string]any {
	t.Helper()
	var result map[string]any
	if err := json.Unmarshal(completion.ResultJSON, &result); err != nil {
		t.Fatal(err)
	}
	if len(completion.ResultJSON) > 8192 || completion.FinishedAt.IsZero() {
		t.Fatalf("unbounded or undated completion: %+v", completion)
	}
	return result
}

func TestExecutorRunsDistinctIncidentsWithoutWaitingForVerification(t *testing.T) {
	calls := 0
	db := newFakeExecStore(approvedApproval(t, 1, false), approvedApproval(t, 2, false))
	registry := executorTestRegistry(t, tools.L2LowRisk, func(_ context.Context, args json.RawMessage) (string, error) {
		calls++
		if calls == 2 && db.committed[1].Status != "executed" {
			t.Fatal("next mutation started before prior result was durable")
		}
		if !bytes.Equal(args, db.rows[calls-1].ArgsJSON) {
			t.Fatalf("args = %s", args)
		}
		return "restarted", nil
	})
	executor := quietExecutor(db, registry, executorBinding())
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := executor.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(db.committed) != 2 {
		t.Fatalf("calls=%d committed=%v", calls, db.committed)
	}
	for _, completion := range db.committed {
		result := executionResult(t, completion)
		if completion.Status != "executed" || result["executed"] != true || result["dry_run"] != false || result["manual_check"] == true {
			t.Fatalf("completion=%+v result=%v", completion, result)
		}
	}
	if err := executor.RunOnce(ctx); err != nil || calls != 2 {
		t.Fatalf("queue replay: calls=%d err=%v", calls, err)
	}
}

func TestExecutorHonorsStoredSimulation(t *testing.T) {
	for _, globalDryRun := range []bool{false, true} {
		t.Run(fmt.Sprint(globalDryRun), func(t *testing.T) {
			db := newFakeExecStore(approvedApproval(t, 1, true))
			binding := executorBinding()
			binding.DryRun = globalDryRun
			registry := executorTestRegistry(t, tools.L2LowRisk, func(context.Context, json.RawMessage) (string, error) {
				t.Fatal("simulation invoked handler")
				return "", nil
			})
			if err := quietExecutor(db, registry, binding).RunOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			completion := db.committed[1]
			result := executionResult(t, completion)
			if completion.Status != "simulated" || result["dry_run"] != true || result["executed"] != false || result["manual_check"] == true {
				t.Fatalf("completion=%+v result=%v", completion, result)
			}
		})
	}
}

func TestExecutorPassesRuntimeLevelAndSafetySwitchToClaim(t *testing.T) {
	for _, test := range []struct {
		name   string
		level  tools.SafetyLevel
		dryRun bool
	}{
		{"removed", "", false}, {"readonly", tools.L1ReadOnly, false}, {"forbidden", tools.L4Forbidden, false},
		{"level changed", tools.L3Approval, false}, {"global dry run", tools.L2LowRisk, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := newFakeExecStore(approvedApproval(t, 1, false))
			binding := executorBinding()
			binding.DryRun = test.dryRun
			db.claim = func(row *store.Approval, received incident.ExecutionBinding) bool {
				if received.SafetyLevel != string(test.level) || received.DryRun != test.dryRun {
					t.Fatalf("claim binding = %+v", received)
				}
				snapshot, _ := incident.ParseExecutionContext(row.ExecutionContext)
				if snapshot.ValidateBinding(received, true) == nil {
					t.Fatal("unsafe binding would permit claim")
				}
				row.Status = "expired"
				return false
			}
			registry := executorTestRegistry(t, test.level, func(context.Context, json.RawMessage) (string, error) {
				t.Fatal("rejected claim invoked handler")
				return "", nil
			})
			if err := quietExecutor(db, registry, binding).RunOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(db.bindings) != 1 || len(db.attempts) != 0 || db.rows[0].Status != "expired" {
				t.Fatalf("bindings=%v attempts=%v row=%+v", db.bindings, db.attempts, db.rows[0])
			}
		})
	}
}

func TestExecutorRejectsInvalidFreshClaim(t *testing.T) {
	for _, test := range []struct {
		name   string
		level  tools.SafetyLevel
		change func(*store.Approval, *incident.ExecutionBinding)
	}{
		{"bad hash", tools.L2LowRisk, func(row *store.Approval, _ *incident.ExecutionBinding) { row.PlanHash = "old-or-tampered-hash" }},
		{"legacy context", tools.L2LowRisk, func(row *store.Approval, _ *incident.ExecutionBinding) { row.ExecutionContext = nil }},
		{"missing dry run", tools.L2LowRisk, func(row *store.Approval, _ *incident.ExecutionBinding) {
			row.ExecutionContext = bytes.Replace(row.ExecutionContext, []byte(`"dry_run":false,`), nil, 1)
		}},
		{"target mismatch", tools.L2LowRisk, func(row *store.Approval, _ *incident.ExecutionBinding) {
			row.ArgsJSON = []byte(`{"target_kind":"container","target_name":"other"}`)
		}},
		{"unknown tool", "", nil}, {"L1", tools.L1ReadOnly, nil}, {"L4", tools.L4Forbidden, nil}, {"level drift", tools.L3Approval, nil},
		{"global dry run", tools.L2LowRisk, func(_ *store.Approval, binding *incident.ExecutionBinding) { binding.DryRun = true }},
		{"URL drift", tools.L2LowRisk, func(_ *store.Approval, binding *incident.ExecutionBinding) { binding.BaseURL = "http://127.0.0.1:9000" }},
		{"target removed", tools.L2LowRisk, func(_ *store.Approval, binding *incident.ExecutionBinding) { binding.AllowedContainers = nil }},
	} {
		t.Run(test.name, func(t *testing.T) {
			row, binding := approvedApproval(t, 1, false), executorBinding()
			if test.change != nil {
				test.change(&row, &binding)
			}
			db := newFakeExecStore(row)
			registry := executorTestRegistry(t, test.level, func(context.Context, json.RawMessage) (string, error) {
				t.Fatal("invalid claimed approval invoked handler")
				return "", nil
			})
			if err := quietExecutor(db, registry, binding).RunOnce(context.Background()); err == nil {
				t.Fatal("invalid claimed approval did not return an error")
			}
			if len(db.attempts) != 0 {
				t.Fatal("invalid claim fabricated an external result")
			}
		})
	}
}

func TestExecutorUsesFreshClaimNotPolledRow(t *testing.T) {
	for _, dryRun := range []bool{false, true} {
		t.Run(fmt.Sprint(dryRun), func(t *testing.T) {
			db := newFakeExecStore(approvedApproval(t, 1, false))
			db.claim = func(row *store.Approval, _ incident.ExecutionBinding) bool {
				if dryRun {
					setExecutorSnapshot(t, row, executorSnapshot(true))
				} else {
					row.PlanHash = "changed-after-poll"
				}
				return true
			}
			registry := executorTestRegistry(t, tools.L2LowRisk, func(context.Context, json.RawMessage) (string, error) {
				t.Fatal("polled row used instead of fresh claim")
				return "", nil
			})
			err := quietExecutor(db, registry, executorBinding()).RunOnce(context.Background())
			if dryRun && (err != nil || db.committed[1].Status != "simulated") {
				t.Fatalf("fresh simulation: err=%v committed=%v", err, db.committed)
			}
			if !dryRun && err == nil {
				t.Fatal("fresh invalid hash was not rejected")
			}
		})
	}
}

func TestExecutorScopeRejectionDoesNotBlockNextIncident(t *testing.T) {
	db := newFakeExecStore(approvedApproval(t, 1, false), approvedApproval(t, 2, false))
	db.claim = func(row *store.Approval, _ incident.ExecutionBinding) bool {
		if row.ID == 1 {
			// Store atomically rejected a recovered incident or changed member scope.
			row.Status = "expired"
			return false
		}
		return true
	}
	calls := 0
	registry := executorTestRegistry(t, tools.L2LowRisk, func(context.Context, json.RawMessage) (string, error) {
		calls++
		return "ok", nil
	})
	if err := quietExecutor(db, registry, executorBinding()).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || len(db.committed) != 1 || db.committed[2].Status != "executed" {
		t.Fatalf("calls=%d committed=%v", calls, db.committed)
	}
}

func TestExecutorAllowsApprovedL3UsingRegistryLevel(t *testing.T) {
	row := approvedApproval(t, 1, false)
	snapshot := executorSnapshot(false)
	snapshot.SafetyLevel = "L3"
	setExecutorSnapshot(t, &row, snapshot)
	db := newFakeExecStore(row)
	calls := 0
	registry := executorTestRegistry(t, tools.L3Approval, func(context.Context, json.RawMessage) (string, error) {
		calls++
		return "ok", nil
	})
	// Deliberately stale constructor level must be replaced by registry L3.
	if err := quietExecutor(db, registry, executorBinding()).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || db.bindings[0].SafetyLevel != "L3" || db.committed[1].Status != "executed" {
		t.Fatalf("calls=%d bindings=%v committed=%v", calls, db.bindings, db.committed)
	}
}

func TestExecutorRetriesOnlyResultPersistence(t *testing.T) {
	for _, test := range []struct {
		name      string
		ambiguous bool
		toolFails bool
	}{
		{"transient success", false, false}, {"ambiguous success", true, false},
		{"transient failure", false, true}, {"ambiguous failure", true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := newFakeExecStore(approvedApproval(t, 1, false))
			calls := 0
			registry := executorTestRegistry(t, tools.L2LowRisk, func(context.Context, json.RawMessage) (string, error) {
				calls++
				if test.toolFails {
					return "", errors.New("restart response lost; outcome unknown")
				}
				return "restarted", nil
			})
			db.finish = func(completion store.ExecutionCompletion, attempt int) error {
				if calls != 1 {
					t.Fatalf("persistence retry re-executed tool: calls=%d", calls)
				}
				if attempt == 1 {
					if test.ambiguous {
						if err := db.commit(completion); err != nil {
							t.Fatal(err)
						}
					}
					return errors.New("database response lost")
				}
				return nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			executor := quietExecutor(db, registry, executorBinding())
			if err := executor.RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
			if calls != 1 || len(db.attempts) != 2 || len(db.committed) != 1 {
				t.Fatalf("calls=%d attempts=%d committed=%v", calls, len(db.attempts), db.committed)
			}
			if !bytes.Equal(db.attempts[0].ResultJSON, db.attempts[1].ResultJSON) {
				t.Fatal("result payload changed across persistence retries")
			}
			if !test.ambiguous && !db.attempts[1].FinishedAt.After(db.attempts[0].FinishedAt) {
				t.Fatal("uncommitted result retained stale verification window start")
			}
			if test.ambiguous && !db.committed[1].FinishedAt.Equal(db.attempts[0].FinishedAt) {
				t.Fatal("ambiguous commit changed already-durable time")
			}
			result := executionResult(t, db.committed[1])
			if test.toolFails {
				if db.committed[1].Status != "failed" || result["manual_check"] != true || result["executed"] != false || !strings.Contains(result["error"].(string), "outcome unknown") {
					t.Fatalf("missing manual failure semantics: %v", result)
				}
			} else if db.committed[1].Status != "executed" || result["executed"] != true {
				t.Fatalf("successful external result lost: %v", result)
			}
			if err := executor.RunOnce(ctx); err != nil || calls != 1 {
				t.Fatalf("terminal replay: calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestExecutorPersistenceStopsOnCancellationOrConflict(t *testing.T) {
	for _, terminal := range []error{context.Canceled, store.ErrApprovalConflict, store.ErrApprovalNotFound} {
		t.Run(terminal.Error(), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			db := newFakeExecStore(approvedApproval(t, 1, false))
			calls := 0
			registry := executorTestRegistry(t, tools.L2LowRisk, func(context.Context, json.RawMessage) (string, error) {
				calls++
				return "restarted", nil
			})
			db.finish = func(store.ExecutionCompletion, int) error {
				if terminal == context.Canceled {
					cancel()
					return errors.New("database unavailable")
				}
				return fmt.Errorf("finish: %w", terminal)
			}
			executor := quietExecutor(db, registry, executorBinding())
			if err := executor.RunOnce(ctx); !errors.Is(err, terminal) {
				t.Fatalf("error=%v want %v", err, terminal)
			}
			if calls != 1 || len(db.attempts) != 1 || len(db.committed) != 0 {
				t.Fatalf("calls=%d attempts=%d committed=%v", calls, len(db.attempts), db.committed)
			}
			if err := executor.RunOnce(context.Background()); err != nil || calls != 1 {
				t.Fatalf("unknown result re-executed: calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestExecutorBoundsEscapedAndUnicodeResults(t *testing.T) {
	for _, toolFails := range []bool{false, true} {
		t.Run(fmt.Sprint(toolFails), func(t *testing.T) {
			db := newFakeExecStore(approvedApproval(t, 1, false))
			text := strings.Repeat("\x00\"\\<&界😀", 10000)
			registry := executorTestRegistry(t, tools.L2LowRisk, func(context.Context, json.RawMessage) (string, error) {
				if toolFails {
					return "", errors.New(text)
				}
				return text, nil
			})
			if err := quietExecutor(db, registry, executorBinding()).RunOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			result := executionResult(t, db.committed[1])
			field := "output"
			if toolFails {
				field = "error"
			}
			if !strings.Contains(result[field].(string), "[truncated]") {
				t.Fatal("omitted truncation indication")
			}
		})
	}
}

func TestExecutorPollAndClaimErrorsNeverInvokeHandler(t *testing.T) {
	for _, phase := range []string{"poll", "claim", "canceled after claim"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			db := newFakeExecStore(approvedApproval(t, 1, false))
			failure := errors.New("database unavailable")
			switch phase {
			case "poll":
				db.nextErr = failure
			case "claim":
				db.claimErr = failure
			default:
				failure = context.Canceled
				db.claim = func(*store.Approval, incident.ExecutionBinding) bool { cancel(); return true }
			}
			registry := executorTestRegistry(t, tools.L2LowRisk, func(context.Context, json.RawMessage) (string, error) {
				t.Fatal("failed claim invoked handler")
				return "", nil
			})
			if err := quietExecutor(db, registry, executorBinding()).RunOnce(ctx); !errors.Is(err, failure) {
				t.Fatalf("err=%v want %v", err, failure)
			}
			if len(db.attempts) != 0 {
				t.Fatal("no invocation must not fabricate a failure result")
			}
		})
	}
}

func waitExecutor(t *testing.T, executor *Executor) {
	t.Helper()
	returned := make(chan struct{})
	go func() { executor.Wait(); close(returned) }()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("Wait blocked after shutdown or failed Start")
	}
}

func TestExecutorStartPreflightAndRecoveryFailureDoNotHangWait(t *testing.T) {
	for _, phase := range []string{"poll", "recover"} {
		t.Run(phase, func(t *testing.T) {
			db := newFakeExecStore(approvedApproval(t, 1, false))
			failure := errors.New("database unavailable")
			if phase == "poll" {
				db.nextErr = failure
			} else {
				db.recoveryErr = failure
			}
			registry := executorTestRegistry(t, tools.L2LowRisk, func(context.Context, json.RawMessage) (string, error) {
				t.Fatal("failed startup invoked handler")
				return "", nil
			})
			executor := quietExecutor(db, registry, executorBinding())
			if err := executor.Start(context.Background()); !errors.Is(err, failure) {
				t.Fatalf("Start error=%v want %v", err, failure)
			}
			waitExecutor(t, executor)
		})
	}
}

func TestExecutorStartRecoversUnknownResultsWithoutReplay(t *testing.T) {
	row := approvedApproval(t, 1, false)
	row.Status = "executing"
	db := newFakeExecStore(row)
	registry := executorTestRegistry(t, tools.L2LowRisk, func(context.Context, json.RawMessage) (string, error) {
		t.Fatal("unknown external result was replayed")
		return "", nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	executor := quietExecutor(db, registry, executorBinding())
	if err := executor.Start(ctx); err != nil {
		t.Fatal(err)
	}
	cancel()
	waitExecutor(t, executor)
	if db.recoveryCalls != 1 || db.rows[0].Status != "failed" || len(db.attempts) != 0 {
		t.Fatalf("recoveryCalls=%d row=%+v attempts=%v", db.recoveryCalls, db.rows[0], db.attempts)
	}
}

func TestExecutorShutdownCancelsPersistenceRetry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db := newFakeExecStore(approvedApproval(t, 1, false))
	calls := 0
	registry := executorTestRegistry(t, tools.L2LowRisk, func(context.Context, json.RawMessage) (string, error) {
		calls++
		return "restarted", nil
	})
	attempted := make(chan struct{}, 1)
	db.finish = func(store.ExecutionCompletion, int) error {
		select {
		case attempted <- struct{}{}:
		default:
		}
		return errors.New("database unavailable")
	}
	executor := quietExecutor(db, registry, executorBinding())
	if err := executor.Start(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-attempted:
	case <-time.After(time.Second):
		t.Fatal("worker did not attempt persistence")
	}
	cancel()
	waitExecutor(t, executor)
	if calls != 1 || len(db.committed) != 0 {
		t.Fatalf("calls=%d committed=%v", calls, db.committed)
	}
}
