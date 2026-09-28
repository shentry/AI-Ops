package diagnose

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	binding          incident.ExecutionBinding
}

func NewVerificationWorker(db verificationStore, verifier *Verifier, memoryTTLSeconds int, notifier notify.Notifier, logger *log.Logger, binding incident.ExecutionBinding) *VerificationWorker {
	if logger == nil {
		logger = log.Default()
	}
	return &VerificationWorker{db: db, verifier: verifier, memoryTTLSeconds: memoryTTLSeconds, notifier: notifier, logger: logger, done: make(chan struct{}), now: time.Now, binding: binding}
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
		} else if approval.Status != "executed" {
			invalid = errors.New("approval has no completed execution to verify")
		}
	}
	if invalid == nil {
		invalid = snapshot.ValidateBinding(approval.ToolName, w.binding)
	}
	if invalid == nil && task.Phase != "watch" {
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
		if task.Phase == "watch" {
			completion = evaluateWatch(task, snapshot.Verification, w.currentTime(), observation)
		} else {
			completion = evaluateVerification(task, snapshot.Verification, w.currentTime(), observation)
		}
		if err := w.prepareEffects(ctx, approval, snapshot, &completion); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	completion.ApprovalID, completion.ClaimedAt = task.ApprovalID, *task.ClaimedAt
	completion.Detail = verifyText(completion.Detail)
	result, err := w.db.FinalizeVerification(ctx, completion)
	if err != nil {
		return err
	}
	// Finalization rechecks scope under locks. Only its committed status may be
	// announced, not the pre-transaction observation or proposed side effects.
	if result.Applied && (result.Status != "pending" || result.Phase != task.Phase) {
		if counter, ok := map[string]string{"passed": metrics.VerifyPassed, "failed": metrics.VerifyFailed, "inconclusive": metrics.VerifyInconclusive}[result.Status]; ok {
			metrics.Inc(counter)
		}
		if result.Status == "failed" && completion.DemoteFingerprint != "" {
			metrics.Inc(metrics.MemoryDemoted)
		}
		w.notifyCompleted(ctx, approval, snapshot, result)
	}
	return nil
}

// evaluateVerification passes only after the snapshot's required consecutive
// healthy observations, and never promotes a late result. At/after expiry only
// the last persisted pre-deadline unhealthy observation, still fresh now, can
// fail; an unfinished healthy streak is inconclusive, not recovered.
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
		passes := task.ConsecutivePasses + 1
		if !spec.ObservationFresh(task.LastCheckedAt, now) {
			passes = 1
		}
		if passes >= spec.RequiredPasses {
			result.Status = "passed"
			return result
		}
		result.Detail = fmt.Sprintf("%s; %d of %d consecutive healthy observations", observation.Detail, passes, spec.RequiredPasses)
	}
	result.Status = "pending"
	result.NextCheckAt = nextCheck(now, interval, task.DeadlineAt)
	return result
}

// evaluateWatch looks for a recurrence after recovery: the same number of
// consecutive unhealthy observations that proved recovery. The watch ends
// stable only after a healthy last observation; otherwise stability is unknown.
func evaluateWatch(task store.VerifyTask, spec incident.VerificationSpec, now time.Time, observation *VerificationObservation) store.VerificationCompletion {
	if !now.Before(task.DeadlineAt) {
		var last VerificationObservation
		if spec.ObservationFresh(task.LastCheckedAt, now) && task.LastCheckedAt.Before(task.DeadlineAt) && json.Unmarshal(task.LastResultJSON, &last) == nil && last.Observation == "healthy" {
			return store.VerificationCompletion{CheckedAt: now, Status: "stable", Observation: "healthy", Detail: "no recurrence during the watch window"}
		}
		return store.VerificationCompletion{CheckedAt: now, Status: "inconclusive", Observation: "unavailable", Detail: "watch window ended without a fresh healthy observation"}
	}
	result := store.VerificationCompletion{CheckedAt: now, Status: "pending", Observation: "unavailable", Detail: "no observation"}
	if observation != nil {
		result.Observation, result.Detail = observation.Observation, observation.Detail
	}
	if !spec.ObservationFresh(task.LastCheckedAt, now) || result.Observation == "unavailable" {
		result.Status, result.Observation, result.Detail = "inconclusive", "unavailable", "watch observations are incomplete; stability cannot be confirmed"
		return result
	}
	if result.Observation == "unhealthy" && task.ConsecutiveFailures+1 >= spec.RequiredPasses {
		result.Status = "recurred"
		return result
	}
	result.NextCheckAt = nextCheck(now, time.Duration(spec.IntervalSeconds)*time.Second, task.DeadlineAt)
	return result
}

func nextCheck(now time.Time, interval time.Duration, deadline time.Time) time.Time {
	if next := now.Add(interval); next.Before(deadline) {
		return next
	}
	return deadline
}

// prepareEffects describes requested effects, never executes them. The store
// applies them with the verdict, audit and unified retry admission in one tx.
// A compensation has none: it never retries, remembers or demotes.
func (w *VerificationWorker) prepareEffects(ctx context.Context, approval store.Approval, snapshot incident.ExecutionContext, completion *store.VerificationCompletion) error {
	if snapshot.Kind != incident.KindPrimary {
		return nil
	}
	remember := completion.Status == "stable" || (completion.Status == "passed" && snapshot.Verification.WatchSeconds == 0)
	if completion.Status != "failed" && !remember {
		return nil
	}
	run, err := w.db.GetAgentRun(ctx, approval.RunID)
	if err != nil {
		return err
	}
	if run.ID != approval.RunID || run.IncidentID != approval.IncidentID {
		return errors.New("verification: source run does not belong to approval incident")
	}
	inc, err := w.db.GetIncident(ctx, approval.IncidentID)
	if err != nil {
		return err
	}
	fingerprint := incident.FaultFingerprint(inc.GroupKey, snapshot.FaultAlert)
	if completion.Status == "failed" {
		completion.Retry = true
		if run.Mode == "memory_hit" {
			completion.DemoteFingerprint = fingerprint
		}
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
	rca := ""
	if run.RCAText != nil {
		rca = *run.RCAText
	}
	completion.Memory = &store.FaultMemory{
		Fingerprint: fingerprint, GroupKey: inc.GroupKey, AlertName: snapshot.FaultAlert, RCAText: rca,
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

var verificationSummaries = map[string]string{
	"passed":       "业务恢复验证连续通过；告警状态仍由告警源同步",
	"failed":       "观察窗口内业务未恢复",
	"inconclusive": "恢复情况未知，需要人工核查",
	"stable":       "恢复后观察期内未复发",
	"recurred":     "恢复后故障复发，已阻断该规则的自动执行，需要人工处理",
}

func (w *VerificationWorker) notifyCompleted(ctx context.Context, approval store.Approval, snapshot incident.ExecutionContext, result store.VerificationFinalization) {
	// Failure delivery is queued atomically with its verdict and retried by notify.Worker.
	if result.Status == "failed" || result.Status == "inconclusive" || result.Status == "recurred" {
		return
	}
	if w.notifier == nil {
		return
	}
	summary := verificationSummaries[result.Status]
	if result.Status == "pending" && result.Phase == "watch" {
		summary = verificationSummaries["passed"] + "，进入恢复后观察期"
	}
	if snapshot.Kind == incident.KindCompensation {
		summary = "补偿动作：" + summary
	}
	if result.CompensationID != 0 {
		summary += fmt.Sprintf("；已排队预授权补偿（审批 %d）", result.CompensationID)
	}
	notification := notify.Notification{
		Kind: notify.NotificationVerifyCompleted, IncidentID: approval.IncidentID,
		RunID: &approval.RunID, ApprovalID: &approval.ID, Title: "恢复验证结果", Summary: summary,
		Payload: map[string]any{"status": result.Status, "phase": result.Phase},
	}
	w.sendNotification(ctx, notification)
	if result.Escalated || snapshot.Kind == incident.KindCompensation && result.Status != "passed" {
		metrics.Inc(metrics.EscalationSent)
		notification.Kind, notification.Title = notify.NotificationEscalationRequired, "需要人工介入"
		notification.Summary = summary
		w.sendNotification(ctx, notification)
	}
}

func (w *VerificationWorker) sendNotification(ctx context.Context, notification notify.Notification) {
	if _, err := w.notifier.Send(ctx, notification); err != nil {
		metrics.Inc(metrics.NotificationFailed)
		w.logger.Printf("verification: incident %d notification failed: %s", notification.IncidentID, verifyText(err.Error()))
	}
}
