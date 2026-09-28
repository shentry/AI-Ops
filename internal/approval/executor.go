package approval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"oncall-agent/internal/incident"
	"oncall-agent/internal/metrics"
	"oncall-agent/internal/store"
	"oncall-agent/internal/tools"
)

// execStore owns locked revalidation and atomic execution/task/event/history writes.
type execStore interface {
	CheckExecutionLease(context.Context) error
	NextApprovedApproval(context.Context, time.Time) (store.Approval, bool, error)
	ClaimApprovalExecution(context.Context, uint64, time.Time, store.RemediationPolicy) (store.Approval, bool, error)
	FinishExecution(context.Context, store.ExecutionCompletion) error
	ListExecutingApprovals(context.Context) ([]store.Approval, error)
}

// Executor claims approved snapshots and runs their actions. Recovery
// verification is independent: this worker never waits for an observation.
// Exactly one executor may be active (the host holds the store's executor lock).
type Executor struct {
	db        execStore
	registry  *tools.Registry
	authority *Authority
	logger    *log.Logger
	done      chan struct{}
}

func NewExecutor(db execStore, registry *tools.Registry, authority *Authority, logger *log.Logger) *Executor {
	if logger == nil {
		logger = log.Default()
	}
	return &Executor{db: db, registry: registry, authority: authority, logger: logger, done: make(chan struct{})}
}

// Start is called once, before accepting requests. Executions interrupted by
// a previous process are reconciled against the actual target state, never
// replayed. Wait also returns after failed startup.
func (e *Executor) Start(ctx context.Context) error {
	if e.db == nil || e.registry == nil || e.authority == nil {
		close(e.done)
		return errors.New("executor: store, registry and authority are required")
	}
	if err := e.db.CheckExecutionLease(ctx); err != nil {
		close(e.done)
		return err
	}
	if _, _, err := e.db.NextApprovedApproval(ctx, time.Now().UTC()); err != nil {
		close(e.done)
		return fmt.Errorf("executor: preflight approved queue: %w", err)
	}
	interrupted, err := e.db.ListExecutingApprovals(ctx)
	if err != nil {
		close(e.done)
		return fmt.Errorf("executor: list interrupted executions: %w", err)
	}
	for _, row := range interrupted {
		if err := e.persist(ctx, e.recover(ctx, row)); err != nil {
			close(e.done)
			return fmt.Errorf("executor: recover approval %d: %w", row.ID, err)
		}
	}
	go e.loop(ctx)
	return nil
}

func (e *Executor) Wait() { <-e.done }

func (e *Executor) loop(ctx context.Context) {
	defer close(e.done)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if err := e.RunOnce(ctx); err != nil && ctx.Err() == nil {
			e.logger.Printf("executor: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// RunOnce drains currently approved work without waiting for verification. It
// must not run concurrently with Start or another RunOnce. A persistence retry
// holds the result, never the action.
func (e *Executor) RunOnce(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := e.db.CheckExecutionLease(ctx); err != nil {
			return err
		}
		candidate, found, err := e.db.NextApprovedApproval(ctx, time.Now().UTC())
		if err != nil {
			return fmt.Errorf("executor: poll approved: %w", err)
		}
		if !found {
			return nil
		}
		now := time.Now().UTC()
		row, claimed, err := e.db.ClaimApprovalExecution(ctx, candidate.ID, now, e.authority.Policy(now))
		if err != nil {
			return fmt.Errorf("executor: claim approval %d: %w", candidate.ID, err)
		}
		if !claimed {
			continue
		}
		if row.ID != candidate.ID || row.Status != "executing" || row.OperationID == nil {
			return fmt.Errorf("executor: invalid claimed approval %d", candidate.ID)
		}
		if err := e.persist(ctx, e.execute(ctx, row)); err != nil {
			return fmt.Errorf("executor: approval %d: %w", row.ID, err)
		}
	}
}

// executionResult is the structured, persisted receipt of one attempt.
type executionResult struct {
	Action      string `json:"action"`
	OperationID string `json:"operation_id"`
	Written     bool   `json:"written"`
	Outcome     string `json:"outcome"`
	Before      string `json:"before,omitempty"`
	After       string `json:"after,omitempty"`
	Detail      string `json:"detail,omitempty"`
	Error       string `json:"error,omitempty"`
	ManualCheck bool   `json:"manual_check,omitempty"`
}

func (e *Executor) operation(row store.Approval) (tools.Action, tools.Operation, error) {
	snapshot, err := incident.ParseExecutionContext(row.ExecutionContext)
	if err != nil {
		return nil, tools.Operation{}, err
	}
	hash, err := incident.PlanHash(row.ToolName, row.ArgsJSON, row.ExecutionContext)
	if err != nil || hash != row.PlanHash {
		return nil, tools.Operation{}, errors.New("execution content does not match the approved plan hash")
	}
	if err := snapshot.ValidateBinding(row.ToolName, e.authority.Binding()); err != nil {
		return nil, tools.Operation{}, err
	}
	action, ok := e.registry.Action(row.ToolName)
	if !ok {
		return nil, tools.Operation{}, fmt.Errorf("action %s is not enabled", row.ToolName)
	}
	operation := ""
	if row.OperationID != nil {
		operation = *row.OperationID
	}
	return action, tools.Operation{ID: operation, Target: snapshot.Target, Args: json.RawMessage(row.ArgsJSON), Revision: snapshot.Revision, PreState: snapshot.PreState}, nil
}

func (e *Executor) execute(ctx context.Context, row store.Approval) store.ExecutionCompletion {
	action, op, err := e.operation(row)
	if err != nil {
		// Claim validated the same content under locks; reaching here means the
		// row changed underneath us. Nothing was written.
		return e.completion(row, "aborted", executionResult{Action: row.ToolName, OperationID: op.ID, Outcome: string(tools.OutcomeNotWritten), Error: err.Error()}, nil)
	}
	snapshot, _ := incident.ParseExecutionContext(row.ExecutionContext)
	if snapshot.Kind == incident.KindPrimary && snapshot.Rule.Mode == incident.ModeAuto {
		if _, err := tools.ReadBusinessTraffic(ctx, e.registry); err != nil {
			return e.completion(row, "aborted", executionResult{Action: row.ToolName, OperationID: op.ID, Outcome: string(tools.OutcomeNotWritten), Error: "automatic execution requires current business metrics: " + err.Error()}, nil)
		}
	}
	if err := e.db.CheckExecutionLease(ctx); err != nil {
		return e.completion(row, "aborted", executionResult{Action: row.ToolName, OperationID: op.ID, Outcome: string(tools.OutcomeNotWritten), Error: err.Error()}, nil)
	}
	runCtx, cancel := context.WithTimeout(ctx, action.Definition().Timeout)
	defer cancel()
	receipt, execErr := action.Execute(runCtx, op)
	result := executionResult{Action: row.ToolName, OperationID: op.ID, Written: receipt.Written, Before: receipt.Before, After: receipt.After, Detail: receipt.Detail}
	if execErr == nil {
		result.Outcome = string(tools.OutcomeNotWritten)
		if receipt.Written {
			result.Outcome = string(tools.OutcomeWritten)
			metrics.Inc(metrics.ApprovalExecuted)
			return e.completion(row, "executed", result, receipt.Change)
		}
		return e.completion(row, "aborted", result, nil)
	}
	// The write errored: read the target before deciding anything.
	result.Error = execErr.Error()
	reconcileCtx, cancelReconcile := context.WithTimeout(context.Background(), action.Definition().Timeout)
	defer cancelReconcile()
	return e.reconciled(row, action, op, result, reconcileCtx)
}

// recover finishes an execution interrupted by a previous process by reading
// the actual target state: applied writes enter verification, unapplied ones
// end without retry (a new decision is required), unknown ones need a person.
func (e *Executor) recover(ctx context.Context, row store.Approval) store.ExecutionCompletion {
	action, op, err := e.operation(row)
	result := executionResult{Action: row.ToolName, OperationID: op.ID, Detail: "execution interrupted by a process restart"}
	if err != nil {
		result.Outcome, result.Error, result.ManualCheck = string(tools.OutcomeUnknown), err.Error(), true
		return e.completion(row, "failed", result, nil)
	}
	reconcileCtx, cancel := context.WithTimeout(ctx, action.Definition().Timeout)
	defer cancel()
	return e.reconciled(row, action, op, result, reconcileCtx)
}

func (e *Executor) reconciled(row store.Approval, action tools.Action, op tools.Operation, result executionResult, ctx context.Context) store.ExecutionCompletion {
	reconciled, err := action.Reconcile(ctx, op)
	outcome := reconciled.Outcome
	if err != nil {
		outcome = tools.OutcomeUnknown
		result.Error = joinText(result.Error, "reconcile: "+err.Error())
	}
	result.Outcome = string(outcome)
	switch outcome {
	case tools.OutcomeWritten:
		result.Written = true
		result.Detail = joinText(result.Detail, "the target shows the write applied")
		metrics.Inc(metrics.ApprovalExecuted)
		return e.completion(row, "executed", result, reconciled.Change)
	case tools.OutcomeNotWritten:
		result.Detail = joinText(result.Detail, "the target shows no write; nothing is retried without a new decision")
		if result.Error == "" {
			return e.completion(row, "aborted", result, nil)
		}
		metrics.Inc(metrics.ApprovalFailedExec)
		return e.completion(row, "failed", result, nil)
	}
	result.ManualCheck = true
	metrics.Inc(metrics.ApprovalFailedExec)
	return e.completion(row, "failed", result, nil)
}

func joinText(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}

func (e *Executor) completion(row store.Approval, status string, result executionResult, change *tools.Change) store.ExecutionCompletion {
	// Bounded text keeps the result below the store's 8 KiB cap even after
	// JSON escaping; the structured fields are never truncated.
	result.Detail, result.Error = tools.Truncate(tools.Sanitize(result.Detail), 1024), tools.Truncate(tools.Sanitize(result.Error), 512)
	raw, _ := json.Marshal(result)
	completion := store.ExecutionCompletion{ApprovalID: row.ID, Status: status, ResultJSON: raw, ManualCheck: result.ManualCheck}
	if change != nil {
		snapshot, _ := incident.ParseExecutionContext(row.ExecutionContext)
		releaseID, before, after := change.ReleaseID, change.Before, change.After
		completion.Change = &store.ChangeEvent{Env: e.authority.env, Service: snapshot.Service, ChangeType: change.Type, ReleaseID: &releaseID,
			BeforeRef: &before, ImageRef: &after, DBMigration: "unknown", Actor: "system:approval"}
	}
	return completion
}

func (e *Executor) persist(ctx context.Context, completion store.ExecutionCompletion) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Only an uncommitted attempt gets a newer verification window start.
		// FinishExecution leaves already committed results/tasks unchanged.
		completion.FinishedAt = time.Now().UTC().Truncate(time.Millisecond)
		err := e.db.FinishExecution(ctx, completion)
		if err == nil {
			return nil
		}
		if errors.Is(err, store.ErrApprovalConflict) || errors.Is(err, store.ErrApprovalNotFound) {
			return fmt.Errorf("execution result commit rejected: %w", err)
		}
		e.logger.Printf("executor: approval %d result commit failed; retrying persistence only: %v", completion.ApprovalID, err)
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
