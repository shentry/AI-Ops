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
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"gorm.io/datatypes"

	"oncall-agent/internal/incident"
	"oncall-agent/internal/llm"
	"oncall-agent/internal/store"
	"oncall-agent/internal/tools"
)

// fakeExecStore mimics the store's claim/finish contract in memory.
type fakeExecStore struct {
	mu        sync.Mutex
	rows      []store.Approval
	policies  []store.RemediationPolicy
	claim     func(*store.Approval) bool
	finish    func(store.ExecutionCompletion, int) error
	attempts  []store.ExecutionCompletion
	committed map[uint64]store.ExecutionCompletion
	nextErr   error
	claimErr  error
	listErr   error
	leaseErr  error
}

func newFakeExecStore(rows ...store.Approval) *fakeExecStore {
	return &fakeExecStore{rows: rows, committed: map[uint64]store.ExecutionCompletion{}}
}

func (f *fakeExecStore) CheckExecutionLease(context.Context) error { return f.leaseErr }

func (f *fakeExecStore) NextApprovedApproval(ctx context.Context, _ time.Time) (store.Approval, bool, error) {
	if err := ctx.Err(); err != nil {
		return store.Approval{}, false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
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

func (f *fakeExecStore) ClaimApprovalExecution(_ context.Context, id uint64, _ time.Time, policy store.RemediationPolicy) (store.Approval, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.policies = append(f.policies, policy)
	if f.claimErr != nil {
		return store.Approval{}, false, f.claimErr
	}
	for i := range f.rows {
		if f.rows[i].ID != id {
			continue
		}
		row := f.rows[i]
		if f.claim != nil && !f.claim(&row) {
			f.rows[i].Status = "expired"
			return f.rows[i], false, nil
		}
		operation, now := fmt.Sprintf("op-%d", id), time.Now().UTC()
		row.Status, row.OperationID, row.OperationStartedAt = "executing", &operation, &now
		f.rows[i].Status = "executing"
		return row, true, nil
	}
	return store.Approval{}, false, nil
}

func (f *fakeExecStore) FinishExecution(_ context.Context, completion store.ExecutionCompletion) error {
	f.mu.Lock()
	f.attempts = append(f.attempts, completion)
	attempt, finish := len(f.attempts), f.finish
	f.mu.Unlock()
	if finish != nil {
		if err := finish(completion, attempt); err != nil {
			return err
		}
	}
	return f.commit(completion)
}

// commit is idempotent for an identical result, like the store.
func (f *fakeExecStore) commit(completion store.ExecutionCompletion) error {
	f.mu.Lock()
	defer f.mu.Unlock()
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

func (f *fakeExecStore) ListExecutingApprovals(context.Context) ([]store.Approval, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	var rows []store.Approval
	for _, row := range f.rows {
		if row.Status == "executing" {
			rows = append(rows, row)
		}
	}
	return rows, nil
}

// approvedRow freezes a real policy decision into an approved approval row.
func approvedRow(t *testing.T, id uint64) store.Approval {
	t.Helper()
	d := testPolicy(t, newRestartAction(), testRemediation(incident.ModeAuto), &fakeState{}).Decide(context.Background(), llm.Plan{Action: "docker_restart"}, testInput())
	draft, err := NewService(nil).Prepare(id, id, d, "rule authorized")
	if err != nil {
		t.Fatal(err)
	}
	draft.ID = id
	return draft
}

func testExecutor(t *testing.T, db execStore, action *fakeAction) *Executor {
	t.Helper()
	registry := testRegistry(t, action)
	if err := registry.Register(tools.ToolSpec{Name: tools.ToolPromInstantQuery, Description: "current business metrics", Timeout: time.Second, Handler: func(context.Context, json.RawMessage) (string, error) {
		return fmt.Sprintf(`{"resultType":"vector","result":[{"value":[%d,"1"]}]}`, time.Now().Unix()), nil
	}}); err != nil {
		t.Fatal(err)
	}
	return NewExecutor(db, registry, testAuthority(t, registry, testRemediation(incident.ModeAuto)), log.New(io.Discard, "", 0))
}

func resultOf(t *testing.T, completion store.ExecutionCompletion) map[string]any {
	t.Helper()
	var result map[string]any
	if err := json.Unmarshal(completion.ResultJSON, &result); err != nil {
		t.Fatalf("result %s: %v", completion.ResultJSON, err)
	}
	return result
}

func TestExecutorRunsClaimedSnapshotsUnderCurrentPolicy(t *testing.T) {
	db := newFakeExecStore(approvedRow(t, 1), approvedRow(t, 2))
	action := newRestartAction()
	executor := testExecutor(t, db, action)
	if err := executor.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(action.executed) != 2 || len(db.committed) != 2 {
		t.Fatalf("executed=%d committed=%d", len(action.executed), len(db.committed))
	}
	op := action.executed[0]
	if op.ID != "op-1" || op.Target != testTarget || op.Revision != "started_at=2026-09-24T00:00:00Z" || string(op.PreState) != `{"status":"running"}` || !strings.Contains(string(op.Args), "sub2api") {
		t.Fatalf("operation=%+v", op)
	}
	if db.policies[0].Binding.RulesVersion != executor.authority.Release() || db.policies[0].Budgets["restart"].Max != 2 {
		t.Fatalf("claim policy=%+v", db.policies[0])
	}
	result := resultOf(t, db.committed[1])
	if db.committed[1].Status != "executed" || result["written"] != true || result["outcome"] != "written" || result["operation_id"] != "op-1" || result["action"] != "docker_restart" {
		t.Fatalf("completion=%s result=%v", db.committed[1].Status, result)
	}
}

func TestExecutorOutcomes(t *testing.T) {
	for name, test := range map[string]struct {
		receipt   tools.Receipt
		execErr   error
		outcome   tools.Outcome
		reconErr  error
		status    string
		manual    bool
		reconcile bool
	}{
		"written":                    {receipt: tools.Receipt{Written: true}, status: "executed"},
		"refused before writing":     {receipt: tools.Receipt{Detail: "container identity changed"}, status: "aborted"},
		"error but applied":          {execErr: errors.New("timeout"), outcome: tools.OutcomeWritten, status: "executed", reconcile: true},
		"error and not applied":      {execErr: errors.New("connection refused"), outcome: tools.OutcomeNotWritten, status: "failed", reconcile: true},
		"error and unknown":          {execErr: errors.New("timeout"), outcome: tools.OutcomeUnknown, status: "failed", manual: true, reconcile: true},
		"error and unreadable state": {execErr: errors.New("timeout"), reconErr: errors.New("docker down"), status: "failed", manual: true, reconcile: true},
	} {
		t.Run(name, func(t *testing.T) {
			db := newFakeExecStore(approvedRow(t, 1))
			action := newRestartAction()
			action.execute = func(tools.Operation) (tools.Receipt, error) { return test.receipt, test.execErr }
			action.reconcile = func(tools.Operation) (tools.Outcome, error) { return test.outcome, test.reconErr }
			if err := testExecutor(t, db, action).RunOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			completion := db.committed[1]
			result := resultOf(t, completion)
			if completion.Status != test.status || completion.ManualCheck != test.manual || (result["manual_check"] == true) != test.manual || (len(action.reconciled) == 1) != test.reconcile {
				t.Fatalf("completion=%+v result=%v reconciled=%d", completion, result, len(action.reconciled))
			}
			if test.execErr != nil && !strings.Contains(result["error"].(string), test.execErr.Error()) {
				t.Fatalf("error lost: %v", result)
			}
			if len(action.executed) != 1 {
				t.Fatalf("executed %d times", len(action.executed))
			}
		})
	}
}

func TestExecutorRecordsDeploymentChange(t *testing.T) {
	db := newFakeExecStore(approvedRow(t, 1))
	action := newRestartAction()
	action.execute = func(tools.Operation) (tools.Receipt, error) {
		return tools.Receipt{Written: true, Change: &tools.Change{Type: "rollback", ReleaseID: "v1", Before: "repo@sha256:bad", After: "repo@sha256:good"}}, nil
	}
	if err := testExecutor(t, db, action).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	change := db.committed[1].Change
	if change == nil || change.Env != "prod" || change.Service != "sub2api" || change.ChangeType != "rollback" || *change.ReleaseID != "v1" ||
		*change.BeforeRef != "repo@sha256:bad" || *change.ImageRef != "repo@sha256:good" || change.Actor == "" {
		t.Fatalf("change=%+v", change)
	}
}

func TestExecutorAbortsContentChangedAfterClaim(t *testing.T) {
	db := newFakeExecStore(approvedRow(t, 1))
	db.claim = func(row *store.Approval) bool {
		row.ArgsJSON = datatypes.JSON(`{"target_kind":"container","target_name":"other"}`)
		return true
	}
	action := newRestartAction()
	if err := testExecutor(t, db, action).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(action.executed) != 0 || db.committed[1].Status != "aborted" || !strings.Contains(resultOf(t, db.committed[1])["error"].(string), "plan hash") {
		t.Fatalf("tampered content executed=%d completion=%+v", len(action.executed), db.committed[1])
	}
}

func TestExecutorSkipsRefusedClaim(t *testing.T) {
	db := newFakeExecStore(approvedRow(t, 1), approvedRow(t, 2))
	db.claim = func(row *store.Approval) bool { return row.ID != 1 }
	action := newRestartAction()
	if err := testExecutor(t, db, action).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(action.executed) != 1 || action.executed[0].ID != "op-2" || db.rows[0].Status != "expired" || db.rows[1].Status != "executed" {
		t.Fatalf("executed=%v rows=%+v", action.executed, db.rows)
	}
}

func TestExecutorPollAndClaimErrorsNeverExecute(t *testing.T) {
	for name, set := range map[string]func(*fakeExecStore){
		"poll":  func(f *fakeExecStore) { f.nextErr = errors.New("db down") },
		"claim": func(f *fakeExecStore) { f.claimErr = errors.New("lock wait timeout") },
	} {
		t.Run(name, func(t *testing.T) {
			db := newFakeExecStore(approvedRow(t, 1))
			set(db)
			action := newRestartAction()
			if err := testExecutor(t, db, action).RunOnce(context.Background()); err == nil || len(action.executed) != 0 {
				t.Fatalf("err=%v executed=%d", err, len(action.executed))
			}
		})
	}
}

func TestExecutorRetriesOnlyResultPersistence(t *testing.T) {
	for _, ambiguous := range []bool{false, true} {
		t.Run(fmt.Sprintf("ambiguous=%v", ambiguous), func(t *testing.T) {
			db := newFakeExecStore(approvedRow(t, 1))
			action := newRestartAction()
			db.finish = func(completion store.ExecutionCompletion, attempt int) error {
				if len(action.executed) != 1 {
					t.Fatalf("persistence retry re-executed the action: %d", len(action.executed))
				}
				if attempt == 1 {
					if ambiguous {
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
			executor := testExecutor(t, db, action)
			if err := executor.RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
			if len(action.executed) != 1 || len(db.attempts) != 2 || !bytes.Equal(db.attempts[0].ResultJSON, db.attempts[1].ResultJSON) {
				t.Fatalf("executed=%d attempts=%d", len(action.executed), len(db.attempts))
			}
			if err := executor.RunOnce(ctx); err != nil || len(action.executed) != 1 {
				t.Fatalf("terminal replay: executed=%d err=%v", len(action.executed), err)
			}
		})
	}
}

func TestExecutorPersistenceStopsOnCancellationOrConflict(t *testing.T) {
	for _, terminal := range []error{context.Canceled, store.ErrApprovalConflict, store.ErrApprovalNotFound} {
		t.Run(terminal.Error(), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			db := newFakeExecStore(approvedRow(t, 1))
			db.finish = func(store.ExecutionCompletion, int) error {
				if terminal == context.Canceled {
					cancel()
					return errors.New("database unavailable")
				}
				return terminal
			}
			action := newRestartAction()
			if err := testExecutor(t, db, action).RunOnce(ctx); !errors.Is(err, terminal) || len(action.executed) != 1 || len(db.attempts) != 1 {
				t.Fatalf("err=%v executed=%d attempts=%d", err, len(action.executed), len(db.attempts))
			}
		})
	}
}

func TestExecutorBoundsEscapedAndUnicodeResults(t *testing.T) {
	db := newFakeExecStore(approvedRow(t, 1))
	action := newRestartAction()
	action.execute = func(tools.Operation) (tools.Receipt, error) {
		return tools.Receipt{Written: true, Detail: strings.Repeat("<\"界\">", 5000)}, nil
	}
	if err := testExecutor(t, db, action).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	raw := db.committed[1].ResultJSON
	if len(raw) > 8192 || !json.Valid(raw) || !utf8.Valid(raw) {
		t.Fatalf("result bytes=%d valid=%v", len(raw), json.Valid(raw))
	}
}

// Start reconciles interrupted executions against the target; nothing is replayed.
func TestExecutorStartReconcilesInterruptedExecutions(t *testing.T) {
	outcomes := map[string]tools.Outcome{"op-1": tools.OutcomeWritten, "op-2": tools.OutcomeNotWritten, "op-3": tools.OutcomeUnknown}
	var rows []store.Approval
	for id := uint64(1); id <= 3; id++ {
		row := approvedRow(t, id)
		operation := fmt.Sprintf("op-%d", id)
		row.Status, row.OperationID = "executing", &operation
		rows = append(rows, row)
	}
	db := newFakeExecStore(rows...)
	action := newRestartAction()
	action.reconcile = func(op tools.Operation) (tools.Outcome, error) { return outcomes[op.ID], nil }
	ctx, cancel := context.WithCancel(context.Background())
	executor := testExecutor(t, db, action)
	if err := executor.Start(ctx); err != nil {
		t.Fatal(err)
	}
	cancel()
	waitExecutor(t, executor)
	if len(action.executed) != 0 || len(action.reconciled) != 3 {
		t.Fatalf("executed=%d reconciled=%d", len(action.executed), len(action.reconciled))
	}
	for id, want := range map[uint64]struct {
		status string
		manual bool
	}{1: {"executed", false}, 2: {"aborted", false}, 3: {"failed", true}} {
		if got := db.committed[id]; got.Status != want.status || got.ManualCheck != want.manual {
			t.Fatalf("approval %d: %+v; want %+v", id, got, want)
		}
	}
}

func waitExecutor(t *testing.T, executor *Executor) {
	t.Helper()
	done := make(chan struct{})
	go func() { executor.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("executor Wait hung")
	}
}

func TestExecutorStartFailureDoesNotHangWait(t *testing.T) {
	for name, set := range map[string]func(*fakeExecStore){
		"preflight": func(f *fakeExecStore) { f.nextErr = errors.New("schema missing") },
		"list":      func(f *fakeExecStore) { f.listErr = errors.New("db down") },
	} {
		t.Run(name, func(t *testing.T) {
			db := newFakeExecStore()
			set(db)
			executor := testExecutor(t, db, newRestartAction())
			if err := executor.Start(context.Background()); err == nil {
				t.Fatal("start succeeded")
			}
			waitExecutor(t, executor)
		})
	}
}
