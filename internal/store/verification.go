package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"oncall-agent/internal/eventlog"
	incidentrule "oncall-agent/internal/incident"
)

func (db *DB) NextVerificationTask(ctx context.Context, now time.Time) (VerifyTask, bool, error) {
	var row VerifyTask
	err := db.WithContext(ctx).Where("status = ? AND next_check_at <= ?", "pending", now).Order("next_check_at ASC, approval_id ASC").First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return row, false, nil
	}
	return row, err == nil, err
}

func (db *DB) ClaimVerificationTask(ctx context.Context, id uint64, now time.Time) (task VerifyTask, claimed bool, err error) {
	if id == 0 || now.IsZero() {
		return task, false, errors.New("store: verification id and claim time required")
	}
	now = now.UTC().Truncate(time.Millisecond)
	err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		approval, _, err := lockApproval(ctx, tx, id)
		if err != nil {
			return err
		}
		query := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).Where("approval_id = ?", id).First(&task)
		if errors.Is(query.Error, gorm.ErrRecordNotFound) {
			return nil
		}
		if query.Error != nil {
			return query.Error
		}
		if task.Status != "pending" || task.NextCheckAt.After(now) {
			return nil
		}
		result := tx.WithContext(ctx).Model(&VerifyTask{}).Where("approval_id = ? AND status = ?", id, "pending").Updates(map[string]any{"status": "running", "claimed_at": now})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return nil
		}
		if err := appendApprovalEvent(ctx, tx, approval, eventlog.EventVerifyStarted, "running", "recovery verification started", now); err != nil {
			return err
		}
		task.Status, task.ClaimedAt = "running", &now
		claimed = true
		return nil
	})
	return task, claimed && err == nil, err
}

func (db *DB) RequeueStaleVerificationTasks(ctx context.Context, staleBefore time.Time) (requeued int64, err error) {
	if staleBefore.IsZero() {
		return 0, errors.New("store: verification stale time required")
	}
	var candidates []VerifyTask
	if err := db.WithContext(ctx).Where("status = ? AND claimed_at < ?", "running", staleBefore).Order("approval_id ASC").Find(&candidates).Error; err != nil {
		return 0, err
	}
	for _, candidate := range candidates {
		changed := false
		err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			approval, _, err := lockApproval(ctx, tx, candidate.ApprovalID)
			if err != nil {
				return err
			}
			result := tx.WithContext(ctx).Model(&VerifyTask{}).Where("approval_id = ? AND status = ? AND claimed_at < ?", candidate.ApprovalID, "running", staleBefore).Updates(map[string]any{"status": "pending", "claimed_at": nil})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected == 0 {
				return nil
			}
			if err := appendApprovalEvent(ctx, tx, approval, eventlog.EventVerifyQueued, "pending", "interrupted read-only verification requeued", time.Now().UTC()); err != nil {
				return err
			}
			changed = true
			return nil
		})
		if err != nil {
			return requeued, err
		}
		if changed {
			requeued++
		}
	}
	return requeued, nil
}

func (db *DB) FinalizeVerification(ctx context.Context, c VerificationCompletion) (VerificationFinalization, error) {
	var final VerificationFinalization
	if c.ApprovalID == 0 || c.ClaimedAt.IsZero() || c.CheckedAt.IsZero() {
		return final, errors.New("store: verification completion identity and times required")
	}
	if c.Observation != "healthy" && c.Observation != "unhealthy" && c.Observation != "unavailable" {
		return final, errors.New("store: invalid verification observation")
	}
	now := c.CheckedAt.UTC().Truncate(time.Millisecond)
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		approval, parent, err := lockApproval(ctx, tx, c.ApprovalID)
		if err != nil {
			return err
		}
		var task VerifyTask
		if err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).First(&task, "approval_id = ?", c.ApprovalID).Error; err != nil {
			return err
		}
		final.Status, final.Phase = task.Status, task.Phase
		if task.Status != "running" || task.ClaimedAt == nil || !task.ClaimedAt.Equal(c.ClaimedAt.UTC().Truncate(time.Millisecond)) {
			return nil
		}
		snapshot, invalid := incidentrule.ParseExecutionContext(approval.ExecutionContext)
		if invalid == nil {
			hash, err := incidentrule.PlanHash(approval.ToolName, approval.ArgsJSON, approval.ExecutionContext)
			if err != nil || hash != approval.PlanHash || approval.Status != "executed" {
				invalid = errors.New("approval has no trustworthy execution to verify")
			}
		}
		if task.Phase == "watch" {
			return finalizeWatch(ctx, tx, approval, parent, task, snapshot, invalid, c, now, &final)
		}
		if invalid == nil {
			members, err := listIncidentExecutionMembers(ctx, tx, parent.ID)
			if err != nil {
				return err
			}
			invalid = snapshot.ValidateMembers(members, false)
		}
		if invalid != nil {
			c.Status, c.Observation, c.Detail = "inconclusive", "unavailable", invalid.Error()
		}
		if c.Status != "pending" && c.Status != "passed" && c.Status != "failed" && c.Status != "inconclusive" {
			return errors.New("store: invalid verification status")
		}
		if c.Status == "passed" && (!now.Before(task.DeadlineAt) || c.Observation != "healthy") {
			c.Status, c.Observation, c.Detail = "inconclusive", "unavailable", "healthy observation arrived outside verification window"
		}
		// Only an in-window observation moves the streak. Any non-healthy read,
		// including an unavailable one, breaks consecutiveness.
		passes := task.ConsecutivePasses
		if now.Before(task.DeadlineAt) {
			passes = 0
			if c.Observation == "healthy" {
				passes = task.ConsecutivePasses + 1
				if !snapshot.Verification.ObservationFresh(task.LastCheckedAt, now) {
					passes = 1
				}
			}
		}
		if c.Status == "passed" && passes < snapshot.Verification.RequiredPasses {
			return errors.New("store: passed verdict without the required consecutive healthy observations")
		}
		if c.Status == "failed" {
			var last struct {
				Observation string `json:"observation"`
			}
			fresh := task.LastCheckedAt != nil && task.LastCheckedAt.Before(task.DeadlineAt) && !task.LastCheckedAt.After(now) && now.Sub(*task.LastCheckedAt) <= time.Duration(snapshot.Verification.IntervalSeconds)*time.Second
			if now.Before(task.DeadlineAt) || !fresh || json.Unmarshal(task.LastResultJSON, &last) != nil || last.Observation != "unhealthy" {
				c.Status, c.Observation, c.Detail = "inconclusive", "unavailable", "verification window has no fresh unhealthy observation"
			}
		}
		if c.Status == "pending" && (!now.Before(task.DeadlineAt) || !c.NextCheckAt.After(now) || c.NextCheckAt.After(task.DeadlineAt)) {
			return errors.New("store: invalid next verification time")
		}
		c.Detail = truncateStoreText(c.Detail, 480)
		payload := verificationPayload(c, "verify", passes, 0)
		watching := c.Status == "passed" && snapshot.Verification.WatchSeconds > 0
		updates := map[string]any{"status": c.Status, "claimed_at": nil, "last_result_json": payload}
		// An expiry-only decision is not a new observation.
		if now.Before(task.DeadlineAt) {
			updates["last_checked_at"] = now
			updates["consecutive_passes"] = passes
		}
		switch {
		case c.Status == "pending":
			updates["next_check_at"] = c.NextCheckAt.UTC().Truncate(time.Millisecond)
		case watching:
			interval := time.Duration(snapshot.Verification.IntervalSeconds) * time.Second
			updates["status"], updates["phase"], updates["consecutive_failures"] = "pending", "watch", 0
			updates["next_check_at"] = now.Add(interval)
			updates["deadline_at"] = now.Add(time.Duration(snapshot.Verification.WatchSeconds) * time.Second)
		default:
			updates["finished_at"] = now
		}
		if err := tx.WithContext(ctx).Model(&VerifyTask{}).Where("approval_id = ? AND status = ? AND claimed_at = ?", task.ApprovalID, "running", task.ClaimedAt).Updates(updates).Error; err != nil {
			return err
		}
		eventType := eventlog.EventVerifyChecked
		if c.Status != "pending" {
			eventType = map[string]eventlog.EventType{"passed": eventlog.EventVerifyPassed, "failed": eventlog.EventVerifyFailed, "inconclusive": eventlog.EventVerifyInconclusive}[c.Status]
			if err := appendVerifyStep(ctx, tx, approval, snapshot, 90, payload, task.CreatedAt, now); err != nil {
				return err
			}
		}
		if _, err := appendIncidentEvent(ctx, tx, IncidentEvent{IncidentID: parent.ID, RunID: &approval.RunID, ApprovalID: &approval.ID, EventType: string(eventType), Phase: "verify", Status: c.Status, Summary: truncateStoreText("verification "+c.Status+": "+c.Detail, 512), PayloadJSON: &payload, CreatedAt: now}); err != nil {
			return err
		}
		if c.Status == "passed" {
			for _, code := range []string{"verify_failed", "verify_inconclusive"} {
				if _, err := resolveIncidentProblem(ctx, tx, parent.ID, code, &approval.RunID, now); err != nil {
					return err
				}
			}
			if !watching {
				if err := applyVerifiedMemory(ctx, tx, parent, snapshot, c.Memory, now); err != nil {
					return err
				}
			}
		}
		if c.Status == "failed" || c.Status == "inconclusive" {
			code := "verify_" + c.Status
			if _, err := openIncidentProblem(ctx, tx, IncidentProblem{IncidentID: parent.ID, RunID: &approval.RunID, Code: code, Severity: "warning", Summary: c.Detail, DetailJSON: &payload, FirstSeenAt: now, LastSeenAt: now}); err != nil {
				return err
			}
			if invalid == nil && snapshot.Compensation != nil {
				id, err := queueCompensation(ctx, tx, approval, snapshot, c.Status, now)
				if err != nil {
					return err
				}
				final.CompensationID = id
			}
		}
		if c.Status == "failed" {
			if c.DemoteFingerprint != "" {
				if c.DemoteFingerprint != incidentrule.FaultFingerprint(parent.GroupKey, snapshot.FaultAlert) {
					return errors.New("store: memory demotion does not belong to incident")
				}
				if err := (&DB{DB: tx}).DemoteFaultMemory(ctx, c.DemoteFingerprint, now); err != nil {
					return err
				}
			}
			if c.Retry {
				if err := requestVerificationRetry(ctx, tx, approval, parent, c.Detail, now, &final); err != nil {
					return err
				}
			}
		}
		final.Applied, final.Status, final.Phase = true, c.Status, "verify"
		if watching {
			final.Phase = "watch"
		}
		return nil
	}, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return VerificationFinalization{}, err
	}
	return final, nil
}

// finalizeWatch records one post-recovery observation. The same number of
// consecutive unhealthy observations that proved recovery proves a
// recurrence; unavailable data counts toward neither. The watch ends stable
// only after a fresh healthy observation; otherwise stability is unknown.
func finalizeWatch(ctx context.Context, tx *gorm.DB, approval Approval, parent Incident, task VerifyTask, snapshot incidentrule.ExecutionContext, invalid error, c VerificationCompletion, now time.Time, final *VerificationFinalization) error {
	if invalid != nil {
		c.Status, c.Observation, c.Detail = "inconclusive", "unavailable", invalid.Error()
	}
	coverageLost := !snapshot.Verification.ObservationFresh(task.LastCheckedAt, now) || c.Observation == "unavailable"
	if invalid == nil && coverageLost {
		c.Status, c.Observation, c.Detail = "inconclusive", "unavailable", "watch observations are incomplete; stability cannot be confirmed"
	}
	failures := task.ConsecutiveFailures
	if now.Before(task.DeadlineAt) {
		switch c.Observation {
		case "unhealthy":
			failures++
		case "healthy":
			failures = 0
		}
	}
	switch c.Status {
	case "pending":
		if !now.Before(task.DeadlineAt) || !c.NextCheckAt.After(now) || c.NextCheckAt.After(task.DeadlineAt) {
			return errors.New("store: invalid next watch time")
		}
	case "recurred":
		if !now.Before(task.DeadlineAt) || failures < snapshot.Verification.RequiredPasses {
			return errors.New("store: recurrence needs the required consecutive unhealthy observations inside the watch window")
		}
	case "stable":
		var last struct {
			Observation string `json:"observation"`
		}
		if now.Before(task.DeadlineAt) || task.LastCheckedAt == nil || !task.LastCheckedAt.Before(task.DeadlineAt) || json.Unmarshal(task.LastResultJSON, &last) != nil || last.Observation != "healthy" {
			return errors.New("store: stability needs the watch window to end after a fresh healthy observation")
		}
	case "inconclusive":
		if invalid == nil && !coverageLost && now.Before(task.DeadlineAt) {
			return errors.New("store: an unfinished watch is not inconclusive")
		}
	default:
		return errors.New("store: invalid watch status")
	}
	c.Detail = truncateStoreText(c.Detail, 480)
	payload := verificationPayload(c, "watch", task.ConsecutivePasses, failures)
	updates := map[string]any{"status": c.Status, "claimed_at": nil, "last_result_json": payload}
	if now.Before(task.DeadlineAt) {
		updates["last_checked_at"] = now
		updates["consecutive_failures"] = failures
	}
	if c.Status == "pending" {
		updates["next_check_at"] = c.NextCheckAt.UTC().Truncate(time.Millisecond)
	} else {
		updates["finished_at"] = now
	}
	if err := tx.WithContext(ctx).Model(&VerifyTask{}).Where("approval_id = ? AND status = ? AND claimed_at = ?", task.ApprovalID, "running", task.ClaimedAt).Updates(updates).Error; err != nil {
		return err
	}
	final.Applied, final.Status, final.Phase = true, c.Status, "watch"
	if c.Status == "pending" {
		return nil
	}
	eventType := map[string]eventlog.EventType{"stable": eventlog.EventVerifyStable, "recurred": eventlog.EventVerifyRecurred, "inconclusive": eventlog.EventVerifyInconclusive}[c.Status]
	if err := appendVerifyStep(ctx, tx, approval, snapshot, 91, payload, task.CreatedAt, now); err != nil {
		return err
	}
	if _, err := appendIncidentEvent(ctx, tx, IncidentEvent{IncidentID: parent.ID, RunID: &approval.RunID, ApprovalID: &approval.ID, EventType: string(eventType), Phase: "verify", Status: c.Status, Summary: truncateStoreText("recovery "+c.Status+": "+c.Detail, 512), PayloadJSON: &payload, CreatedAt: now}); err != nil {
		return err
	}
	switch c.Status {
	case "stable":
		return applyVerifiedMemory(ctx, tx, parent, snapshot, c.Memory, now)
	case "inconclusive":
		_, err := openIncidentProblem(ctx, tx, IncidentProblem{IncidentID: parent.ID, RunID: &approval.RunID, Code: "verify_inconclusive", Severity: "warning", Summary: "stability after recovery is unknown: " + c.Detail, DetailJSON: &payload, FirstSeenAt: now, LastSeenAt: now})
		return err
	}
	message := "fault recurred after recovery; the rule is blocked until reviewed"
	if _, err := openIncidentProblem(ctx, tx, IncidentProblem{IncidentID: parent.ID, RunID: &approval.RunID, Code: "recurred", Severity: "critical", Summary: message, DetailJSON: &payload, FirstSeenAt: now, LastSeenAt: now}); err != nil {
		return err
	}
	final.Escalated = true
	return appendApprovalEvent(ctx, tx, approval, eventlog.EventEscalationRequired, "required", message, now)
}

func verificationPayload(c VerificationCompletion, phase string, passes, failures int) datatypes.JSON {
	raw, _ := json.Marshal(map[string]any{"observation": c.Observation, "detail": c.Detail, "status": c.Status, "phase": phase,
		"passed": c.Status == "passed" || c.Status == "stable", "inconclusive": c.Status == "inconclusive",
		"consecutive_passes": passes, "consecutive_failures": failures})
	return datatypes.JSON(raw)
}

// appendVerifyStep records the verdict in the run's replayable steps. Seq 90
// is the recovery verdict, 91 the watch verdict, 92 a compensation's verdict.
func appendVerifyStep(ctx context.Context, tx *gorm.DB, approval Approval, snapshot incidentrule.ExecutionContext, seq int, payload datatypes.JSON, started, now time.Time) error {
	if snapshot.Kind == incidentrule.KindCompensation {
		seq = 92
	}
	step := AgentRunStep{RunID: approval.RunID, Seq: seq, Kind: "verify", Name: approval.ToolName, OutputJSON: &payload, StartedAt: started, FinishedAt: &now}
	return tx.WithContext(ctx).Create(&step).Error
}

func applyVerifiedMemory(ctx context.Context, tx *gorm.DB, parent Incident, snapshot incidentrule.ExecutionContext, entry *FaultMemory, now time.Time) error {
	if entry == nil {
		return nil
	}
	memory := *entry
	if snapshot.Kind != incidentrule.KindPrimary || memory.Fingerprint != incidentrule.FaultFingerprint(parent.GroupKey, snapshot.FaultAlert) || memory.GroupKey != parent.GroupKey || memory.AlertName != snapshot.FaultAlert || memory.Confidence != "high" || memory.TTLSeconds <= 0 || !json.Valid(memory.PlanJSON) {
		return errors.New("store: invalid verified memory candidate")
	}
	memory.LastSuccess = now
	if memory.FirstSeen.IsZero() {
		memory.FirstSeen = now
	}
	return (&DB{DB: tx}).UpsertFaultMemory(ctx, memory)
}

// queueCompensation publishes the frozen undo of a failed action as a
// pre-authorized approval in the same transaction as the failed verdict.
func queueCompensation(ctx context.Context, tx *gorm.DB, parent Approval, snapshot incidentrule.ExecutionContext, status string, now time.Time) (uint64, error) {
	plan := snapshot.Compensation
	undo := incidentrule.ExecutionContext{
		Version: incidentrule.ExecutionContextVersion, Kind: incidentrule.KindCompensation, Service: snapshot.Service,
		Rule: snapshot.Rule, ActionVersion: plan.ActionVersion, Target: snapshot.Target, Revision: plan.Revision,
		PreState: snapshot.PreState,
		Verification: incidentrule.VerificationSpec{Checks: plan.Checks, IntervalSeconds: snapshot.Verification.IntervalSeconds,
			WindowSeconds: snapshot.Verification.WindowSeconds, TimeoutSeconds: snapshot.Verification.TimeoutSeconds, RequiredPasses: snapshot.Verification.RequiredPasses},
		ExpiresAt: now.Add(incidentrule.CompensationTTL),
	}
	raw, err := json.Marshal(undo)
	if err != nil {
		return 0, err
	}
	hash, err := incidentrule.PlanHash(plan.Action, plan.Args, raw)
	if err != nil {
		return 0, err
	}
	actor, source, reason := "system:compensation", "rule", fmt.Sprintf("compensation of approval %d after verification %s", parent.ID, status)
	row, err := insertApproval(ctx, tx, Approval{
		IncidentID: parent.IncidentID, RunID: parent.RunID, Service: parent.Service, RuleID: parent.RuleID, ParentApprovalID: &parent.ID,
		ToolName: plan.Action, ArgsJSON: datatypes.JSON(plan.Args), ExecutionContext: datatypes.JSON(raw), PlanHash: hash,
		Reason: reason, Status: "approved", ExpiresAt: undo.ExpiresAt, CreatedAt: now,
		DecidedBy: &actor, DecisionSource: &source, DecidedAt: &now, DecisionReason: &reason,
	})
	if err != nil {
		return 0, fmt.Errorf("store: queue compensation: %w", err)
	}
	return row.ID, appendApprovalEvent(ctx, tx, parent, eventlog.EventCompensationQueued, "queued", fmt.Sprintf("compensation approval %d queued", row.ID), now)
}

func requestVerificationRetry(ctx context.Context, tx *gorm.DB, approval Approval, parent Incident, detail string, now time.Time, final *VerificationFinalization) error {
	run, created, err := requestRun(ctx, tx, RunRequest{IncidentID: parent.ID, Mode: incidentrule.ModeFull, Trigger: RunTriggerRetry, RetryOf: &approval.RunID, Reason: detail, RequestedAt: now})
	var refusal *RunAdmissionError
	if err != nil && !errors.As(err, &refusal) {
		return err
	}
	if created {
		final.RetryRunID = run.ID
	}
	if refusal != nil && refusal.Code != "active_processing" {
		message := "automatic retry stopped: " + refusal.Code
		if _, err := openIncidentProblem(ctx, tx, IncidentProblem{IncidentID: parent.ID, RunID: &approval.RunID, Code: "manual_check", Severity: "critical", Summary: message, FirstSeenAt: now, LastSeenAt: now}); err != nil {
			return err
		}
		if err := appendApprovalEvent(ctx, tx, approval, eventlog.EventEscalationRequired, "required", message, now); err != nil {
			return err
		}
		final.Escalated = true
	}
	return nil
}
