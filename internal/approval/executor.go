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
	NextApprovedApproval(context.Context, time.Time) (store.Approval, bool, error)
	ClaimApprovalExecution(context.Context, uint64, time.Time, incident.ExecutionBinding) (store.Approval, bool, error)
	FinishExecution(context.Context, store.ExecutionCompletion) error
	RecoverExecutingApprovals(context.Context, time.Time) (int64, error)
}

// Executor claims approved mutations and persists their results. Recovery
// verification is independent: this worker never waits for a health observation.
type Executor struct {
	db       execStore
	registry *tools.Registry
	binding  incident.ExecutionBinding
	logger   *log.Logger
	done     chan struct{}
}

func NewExecutor(db execStore, registry *tools.Registry, binding incident.ExecutionBinding, logger *log.Logger) *Executor {
	if logger == nil {
		logger = log.Default()
	}
	return &Executor{db: db, registry: registry, binding: binding, logger: logger, done: make(chan struct{})}
}

// Start is called once, before accepting requests. Interrupted external actions
// are recovered conservatively by the store, never replayed. Wait also returns
// after failed startup; the host must not start overlapping executor instances.
func (e *Executor) Start(ctx context.Context) error {
	if e.db == nil || e.registry == nil {
		close(e.done)
		return errors.New("executor: store and registry are required")
	}
	if _, _, err := e.db.NextApprovedApproval(ctx, time.Now().UTC()); err != nil {
		close(e.done)
		return fmt.Errorf("executor: preflight approved queue: %w", err)
	}
	recovered, err := e.db.RecoverExecutingApprovals(ctx, time.Now().UTC())
	if err != nil {
		close(e.done)
		return fmt.Errorf("executor: recover interrupted executions: %w", err)
	}
	if recovered > 0 {
		e.logger.Printf("executor: recovered %d interrupted approvals; manual check required", recovered)
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
// may also be called by a host-owned polling loop, but not concurrently with Start
// or another RunOnce. A persistence retry holds the result, not the action.
func (e *Executor) RunOnce(ctx context.Context) error {
	if e.db == nil || e.registry == nil {
		return errors.New("executor: store and registry are required")
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		candidate, found, err := e.db.NextApprovedApproval(ctx, time.Now().UTC())
		if err != nil {
			return fmt.Errorf("executor: poll approved: %w", err)
		}
		if !found {
			return nil
		}
		// An absent or newly forbidden tool must invalidate the stored approval
		// under the store's locks, not inherit a stale constructor safety level.
		row, claimed, err := e.db.ClaimApprovalExecution(ctx, candidate.ID, time.Now().UTC(), e.currentBinding(candidate.ToolName))
		if err != nil {
			return fmt.Errorf("executor: claim approval %d: %w", candidate.ID, err)
		}
		if !claimed {
			continue
		}
		// TTL belongs to the committed claim. Rechecking a later clock here can
		// abandon executing work solely because the claim response crossed expiry.
		if row.ID == 0 || row.ID != candidate.ID || row.IncidentID == 0 || row.RunID == 0 || row.Status != "executing" {
			return fmt.Errorf("executor: invalid claimed approval %d", candidate.ID)
		}
		if err := e.execute(ctx, row); err != nil {
			return fmt.Errorf("executor: approval %d: %w", row.ID, err)
		}
	}
}

func (e *Executor) currentBinding(tool string) incident.ExecutionBinding {
	binding := e.binding
	// Get returns a zero ToolSpec for an unregistered tool, clearing the level.
	spec, _ := e.registry.Get(tool)
	binding.SafetyLevel = string(spec.Level)
	return binding
}

func (e *Executor) execute(ctx context.Context, row store.Approval) error {
	// Use only the fresh claimed row. These checks fail closed even if a broken
	// store hands us invalid content; current incident/member scope is checked
	// atomically by ClaimApprovalExecution, not with a second unlocked read here.
	snapshot, err := incident.ParseExecutionContext(row.ExecutionContext)
	if err != nil {
		return err
	}
	hash, err := incident.PlanHash(row.ToolName, row.ArgsJSON, row.ExecutionContext)
	if err != nil || hash != row.PlanHash {
		return errors.New("approved execution content does not match plan hash")
	}
	binding := e.currentBinding(row.ToolName)
	if binding.SafetyLevel != string(tools.L2LowRisk) && binding.SafetyLevel != string(tools.L3Approval) {
		return errors.New("approved mutation tool is unregistered or forbidden")
	}
	if err := snapshot.ValidateBinding(binding, true); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	status, output, message := "simulated", "", ""
	if !snapshot.DryRun {
		status = "executed"
		output, err = e.registry.Execute(ctx, row.ToolName, json.RawMessage(row.ArgsJSON))
		if err != nil {
			status, message = "failed", err.Error()
		}
	}
	// At most 1280 runes of arbitrary text: even JSON's six-byte escaping,
	// truncation markers and fixed fields remain below the store's 8 KiB cap.
	// No timestamp goes in JSON: ambiguous commits must retry identical content.
	result, _ := json.Marshal(struct {
		Tool        string `json:"tool"`
		Output      string `json:"output"`
		Error       string `json:"error,omitempty"`
		DryRun      bool   `json:"dry_run"`
		Executed    bool   `json:"executed"`
		ManualCheck bool   `json:"manual_check,omitempty"`
	}{
		Tool: row.ToolName, Output: tools.Truncate(output, 1024), Error: tools.Truncate(message, 256),
		DryRun: snapshot.DryRun, Executed: status == "executed", ManualCheck: status == "failed",
	})
	if err := e.persist(ctx, store.ExecutionCompletion{ApprovalID: row.ID, Status: status, ResultJSON: result}); err != nil {
		return err
	}
	switch status {
	case "executed":
		metrics.Inc(metrics.ApprovalExecuted)
	case "failed":
		metrics.Inc(metrics.ApprovalFailedExec)
	}
	return nil
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
