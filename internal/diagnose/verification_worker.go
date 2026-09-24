package diagnose

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"time"

	"oncall-agent/internal/incident"
	"oncall-agent/internal/llm"
	"oncall-agent/internal/metrics"
	"oncall-agent/internal/notify"
	"oncall-agent/internal/store"
)

type verificationStore interface {
	NextVerificationTask(context.Context, time.Time) (store.VerifyTask, bool, error)
	ClaimVerificationTask(context.Context, uint64, time.Time) (store.VerifyTask, bool, error)
	RequeueStaleVerificationTasks(context.Context, time.Time) (int64, error)
	FinalizeVerification(context.Context, store.VerificationCompletion) (store.VerificationFinalization, error)
	GetApproval(context.Context, uint64) (store.Approval, error)
	GetAgentRun(context.Context, uint64) (store.AgentRun, error)
	GetIncident(context.Context, uint64) (store.Incident, error)
	ListRunSteps(context.Context, uint64) ([]store.AgentRunStep, error)
	ListIncidentExecutionMembers(context.Context, uint64) ([]incident.ExecutionMember, error)
}

// VerificationWorker consumes only due read-only work. A pending observation
// releases its claim; neither the executor nor this worker waits out the window.
type VerificationWorker struct {
	db               verificationStore
	verifier         *Verifier
	memoryTTLSeconds int
	notifier         notify.Notifier
	logger           *log.Logger
	done             chan struct{}
	now              func() time.Time
}

func NewVerificationWorker(db verificationStore, verifier *Verifier, memoryTTLSeconds int, notifier notify.Notifier, logger *log.Logger) *VerificationWorker {
	if logger == nil {
		logger = log.Default()
	}
	return &VerificationWorker{db: db, verifier: verifier, memoryTTLSeconds: memoryTTLSeconds, notifier: notifier, logger: logger, done: make(chan struct{}), now: time.Now}
}

func (w *VerificationWorker) currentTime() time.Time { return w.now().UTC().Truncate(time.Millisecond) }

func (w *VerificationWorker) Start(ctx context.Context) error {
	if _, _, err := w.db.NextVerificationTask(ctx, w.currentTime()); err != nil {
		return err
	}
	if _, err := w.db.RequeueStaleVerificationTasks(ctx, w.currentTime().Add(-incident.VerificationLease)); err != nil {
		return err
	}
	go func() {
		defer close(w.done)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			if err := w.RunOnce(ctx); err != nil && ctx.Err() == nil {
				w.logger.Printf("verification: %s", verifyText(err.Error()))
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return nil
}

func (w *VerificationWorker) Wait() { <-w.done }

// RunOnce recovers stale claims and processes at most one due task. All reads
// and finalization errors propagate; a failed/canceled cycle leaves a recoverable
// claim, never an invented terminal verdict or independently committed effects.
func (w *VerificationWorker) RunOnce(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	now := w.currentTime()
	if _, err := w.db.RequeueStaleVerificationTasks(ctx, now.Add(-incident.VerificationLease)); err != nil {
		return err
	}
	task, found, err := w.db.NextVerificationTask(ctx, now)
	if err != nil || !found {
		return err
	}
	task, claimed, err := w.db.ClaimVerificationTask(ctx, task.ApprovalID, w.currentTime())
	if err != nil || !claimed {
		return err
	}
	if task.ClaimedAt == nil {
		return errors.New("verification: claimed task is missing claimed_at")
	}
	approval, err := w.db.GetApproval(ctx, task.ApprovalID)
	if err != nil {
		return err
	}
	snapshot, invalid := incident.ParseExecutionContext(approval.ExecutionContext)
	if invalid == nil {
		hash, hashErr := incident.PlanHash(approval.ToolName, approval.ArgsJSON, approval.ExecutionContext)
		if hashErr != nil || hash != approval.PlanHash {
			invalid = errors.New("approval execution content does not match approved hash")
		} else if approval.Status != "executed" || snapshot.DryRun {
			invalid = errors.New("approval has no completed real execution to verify")
		} else {
			invalid = snapshot.ValidateBinding(w.verifier.binding, false)
		}
	}
	if invalid == nil {
		members, err := w.db.ListIncidentExecutionMembers(ctx, approval.IncidentID)
		if err != nil {
			return err
		}
		invalid = snapshot.ValidateMembers(members, false)
	}
	var completion store.VerificationCompletion
	if invalid != nil {
		completion = store.VerificationCompletion{Status: "inconclusive", Observation: "unavailable", Detail: invalid.Error(), CheckedAt: w.currentTime()}
	} else {
		var observation *VerificationObservation
		if remaining := task.DeadlineAt.Sub(w.currentTime()); remaining > 0 {
			result := w.verifier.Check(ctx, snapshot, remaining)
			observation = &result
		}
		// Shutdown cancellation is not an observation. Do not detach context or
		// terminalize: the normal stale-claim recovery will resume this task.
		if err := ctx.Err(); err != nil {
			return err
		}
		completion = evaluateVerification(task, snapshot.Verification, w.currentTime(), observation)
		if completion.Status == "passed" || completion.Status == "failed" {
			if err := w.prepareEffects(ctx, approval, &completion); err != nil {
				return err
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	completion.ApprovalID, completion.ClaimedAt = task.ApprovalID, *task.ClaimedAt
	completion.Binding = w.verifier.binding
	completion.Detail = verifyText(completion.Detail)
	result, err := w.db.FinalizeVerification(ctx, completion)
	if err != nil {
		return err
	}
	// Finalization rechecks scope under locks. Only its committed status may be
	// announced, not the pre-transaction HTTP result or proposed side effects.
	if result.Applied && result.Status != "pending" {
		if result.Status == "failed" && completion.DemoteFingerprint != "" {
			metrics.Inc(metrics.MemoryDemoted)
		}
		w.notifyCompleted(ctx, approval, result)
	}
	return nil
}

// evaluateVerification never promotes a late result. At/after expiry only the
// last persisted pre-deadline unhealthy observation, still fresh now, can fail.
func evaluateVerification(task store.VerifyTask, spec incident.VerificationSpec, now time.Time, observation *VerificationObservation) store.VerificationCompletion {
	result := store.VerificationCompletion{CheckedAt: now, Status: "inconclusive", Observation: "unavailable", Detail: "verification window expired without a fresh unhealthy observation"}
	interval := time.Duration(spec.IntervalSeconds) * time.Second
	if !now.Before(task.DeadlineAt) {
		last := task.LastCheckedAt
		if last != nil && last.Before(task.DeadlineAt) && !last.After(now) && now.Sub(*last) <= interval {
			var previous VerificationObservation
			if json.Unmarshal(task.LastResultJSON, &previous) == nil && previous.Observation == "unhealthy" {
				result.Status, result.Observation, result.Detail = "failed", "unhealthy", previous.Detail
			}
		}
		return result
	}
	if observation == nil {
		return result
	}
	result.Observation, result.Detail = observation.Observation, observation.Detail
	if observation.Observation == "healthy" {
		result.Status = "passed"
		return result
	}
	result.Status = "pending"
	result.NextCheckAt = now.Add(interval)
	if result.NextCheckAt.After(task.DeadlineAt) {
		result.NextCheckAt = task.DeadlineAt
	}
	return result
}

// prepareEffects describes requested effects, never executes them. The store
// applies them with the task verdict, audit and unified retry admission in one tx.
func (w *VerificationWorker) prepareEffects(ctx context.Context, approval store.Approval, completion *store.VerificationCompletion) error {
	run, err := w.db.GetAgentRun(ctx, approval.RunID)
	if err != nil {
		return err
	}
	if run.ID != approval.RunID || run.IncidentID != approval.IncidentID {
		return errors.New("verification: source run does not belong to approval incident")
	}
	if completion.Status == "failed" {
		completion.Retry = true
		if run.Mode != "memory_hit" {
			return nil
		}
		inc, err := w.db.GetIncident(ctx, approval.IncidentID)
		if err != nil {
			return err
		}
		completion.DemoteFingerprint = incident.FaultFingerprint(inc.GroupKey, incident.SupportedAlert)
		return nil
	}
	if run.RetryOf != nil || run.Mode == "memory_hit" || run.PlanJSON == nil {
		return nil
	}
	var plan llm.Plan
	if json.Unmarshal(*run.PlanJSON, &plan) != nil || plan.Confidence != "high" {
		return nil
	}
	steps, err := w.db.ListRunSteps(ctx, run.ID)
	if err != nil {
		return err
	}
	if !hasExplicitUnchangedGuard(steps) {
		return nil
	}
	inc, err := w.db.GetIncident(ctx, approval.IncidentID)
	if err != nil {
		return err
	}
	rca := ""
	if run.RCAText != nil {
		rca = *run.RCAText
	}
	completion.Memory = &store.FaultMemory{
		Fingerprint: incident.FaultFingerprint(inc.GroupKey, incident.SupportedAlert),
		GroupKey:    inc.GroupKey, AlertName: incident.SupportedAlert, RCAText: rca,
		PlanJSON: *run.PlanJSON, Confidence: "high", FirstSeen: completion.CheckedAt,
		LastSuccess: completion.CheckedAt, TTLSeconds: w.memoryTTLSeconds,
	}
	return nil
}

// Guard steps are JSON strings emitted by Pipeline. Require the complete trusted
// prefix, not a substring from a reason, and never treat a missing guard as allow.
func hasExplicitUnchangedGuard(steps []store.AgentRunStep) bool {
	found := false
	for _, step := range steps {
		if step.Kind != "guard" {
			continue
		}
		var output string
		if step.Error != nil || step.FinishedAt == nil || step.OutputJSON == nil || json.Unmarshal(*step.OutputJSON, &output) != nil || !strings.HasPrefix(output, "decision=allow overridden=false reason=") {
			return false
		}
		found = true
	}
	return found
}

func (w *VerificationWorker) notifyCompleted(ctx context.Context, approval store.Approval, result store.VerificationFinalization) {
	if w.notifier == nil {
		return
	}
	summary := "恢复情况未知，需要人工核查"
	switch result.Status {
	case "passed":
		summary = "目标健康检查通过；告警状态仍由告警源同步"
	case "failed":
		summary = "观察窗口内目标健康检查未恢复"
	}
	notification := notify.Notification{
		Kind: notify.NotificationVerifyCompleted, IncidentID: approval.IncidentID,
		RunID: &approval.RunID, ApprovalID: &approval.ID, Title: "恢复验证结果", Summary: summary,
		Payload: map[string]any{"status": result.Status},
	}
	w.sendNotification(ctx, notification)
	if result.Escalated {
		notification.Kind, notification.Title = notify.NotificationEscalationRequired, "需要人工介入"
		notification.Summary = "恢复验证失败，自动重诊预算已耗尽，需要人工核查"
		w.sendNotification(ctx, notification)
	}
}

func (w *VerificationWorker) sendNotification(ctx context.Context, notification notify.Notification) {
	if _, err := w.notifier.Send(ctx, notification); err != nil {
		w.logger.Printf("verification: incident %d notification failed: %s", notification.IncidentID, verifyText(err.Error()))
	}
}
