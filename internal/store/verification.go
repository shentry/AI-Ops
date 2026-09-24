package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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
	if c.Status != "pending" && c.Status != "passed" && c.Status != "failed" && c.Status != "inconclusive" {
		return final, errors.New("store: invalid verification status")
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
		final.Status = task.Status
		if task.Status != "running" || task.ClaimedAt == nil || !task.ClaimedAt.Equal(c.ClaimedAt.UTC().Truncate(time.Millisecond)) {
			return nil
		}
		snapshot, invalid := incidentrule.ParseExecutionContext(approval.ExecutionContext)
		if invalid == nil {
			hash, err := incidentrule.PlanHash(approval.ToolName, approval.ArgsJSON, approval.ExecutionContext)
			if err != nil || hash != approval.PlanHash || approval.Status != "executed" || snapshot.DryRun {
				invalid = errors.New("approval has no trustworthy real execution to verify")
			}
		}
		if invalid == nil {
			invalid = snapshot.ValidateBinding(c.Binding, false)
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
		if c.Status == "passed" && (!now.Before(task.DeadlineAt) || c.Observation != "healthy") {
			c.Status, c.Observation, c.Detail = "inconclusive", "unavailable", "healthy observation arrived outside verification window"
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
		payloadBytes, _ := json.Marshal(map[string]any{"observation": c.Observation, "detail": c.Detail, "status": c.Status, "passed": c.Status == "passed", "inconclusive": c.Status == "inconclusive"})
		payload := datatypes.JSON(payloadBytes)
		updates := map[string]any{"status": c.Status, "claimed_at": nil, "last_result_json": payload}
		// An expiry-only decision is not a new HTTP observation.
		if now.Before(task.DeadlineAt) {
			updates["last_checked_at"] = now
		}
		if c.Status == "pending" {
			updates["next_check_at"] = c.NextCheckAt.UTC().Truncate(time.Millisecond)
		} else {
			updates["finished_at"] = now
		}
		if err := tx.WithContext(ctx).Model(&VerifyTask{}).Where("approval_id = ? AND status = ? AND claimed_at = ?", task.ApprovalID, "running", task.ClaimedAt).Updates(updates).Error; err != nil {
			return err
		}
		eventType := eventlog.EventVerifyChecked
		if c.Status != "pending" {
			eventType = map[string]eventlog.EventType{"passed": eventlog.EventVerifyPassed, "failed": eventlog.EventVerifyFailed, "inconclusive": eventlog.EventVerifyInconclusive}[c.Status]
			step := AgentRunStep{RunID: approval.RunID, Seq: 90, Kind: "verify", Name: incidentrule.HealthVerification, OutputJSON: &payload, StartedAt: task.CreatedAt, FinishedAt: &now}
			if err := tx.WithContext(ctx).Create(&step).Error; err != nil {
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
			if c.Memory != nil {
				entry := *c.Memory
				if entry.Fingerprint != incidentrule.FaultFingerprint(parent.GroupKey, incidentrule.SupportedAlert) || entry.GroupKey != parent.GroupKey || entry.AlertName != incidentrule.SupportedAlert || entry.Confidence != "high" || entry.TTLSeconds <= 0 || !json.Valid(entry.PlanJSON) {
					return errors.New("store: invalid verified memory candidate")
				}
				entry.LastSuccess = now
				if entry.FirstSeen.IsZero() {
					entry.FirstSeen = now
				}
				if err := (&DB{DB: tx}).UpsertFaultMemory(ctx, entry); err != nil {
					return err
				}
			}
		}
		if c.Status == "failed" || c.Status == "inconclusive" {
			code := "verify_" + c.Status
			if _, err := openIncidentProblem(ctx, tx, IncidentProblem{IncidentID: parent.ID, RunID: &approval.RunID, Code: code, Severity: "warning", Summary: c.Detail, DetailJSON: &payload, FirstSeenAt: now, LastSeenAt: now}); err != nil {
				return err
			}
		}
		if c.Status == "failed" {
			if c.DemoteFingerprint != "" {
				if c.DemoteFingerprint != incidentrule.FaultFingerprint(parent.GroupKey, incidentrule.SupportedAlert) {
					return errors.New("store: memory demotion does not belong to incident")
				}
				if err := (&DB{DB: tx}).DemoteFaultMemory(ctx, c.DemoteFingerprint, now); err != nil {
					return err
				}
			}
			if c.Retry {
				run, created, err := requestRun(ctx, tx, RunRequest{IncidentID: parent.ID, Mode: incidentrule.ModeFull, Trigger: RunTriggerRetry, RetryOf: &approval.RunID, Reason: c.Detail, RequestedAt: now})
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
			}
		}
		final.Applied, final.Status = true, c.Status
		return nil
	}, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return VerificationFinalization{}, err
	}
	return final, nil
}
