package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"oncall-agent/internal/eventlog"
	incidentrule "oncall-agent/internal/incident"
)

// ExecutionCompletion records an external result; retrying it never retries the action.
// Status is executed (the write happened: verification follows), failed (the
// write errored or its effect is unknown) or aborted (refused before writing).
type ExecutionCompletion struct {
	ApprovalID uint64
	Status     string
	ResultJSON []byte
	// ManualCheck marks a result nobody can confirm without looking.
	ManualCheck bool
	// Change is a deployment change the action made, recorded with the result.
	Change     *ChangeEvent
	FinishedAt time.Time
}

// RemediationPolicy is the current trusted configuration a claim rechecks
// under locks, together with the persisted remediation state.
type RemediationPolicy struct {
	Binding incidentrule.ExecutionBinding
	Budgets map[string]RuleBudget
	// Maintenance is the reason of the maintenance window now in effect, if any.
	Maintenance string
}

type RuleBudget struct {
	Max    int
	Window time.Duration
}

func (db *DB) NextApprovedApproval(ctx context.Context, now time.Time) (Approval, bool, error) {
	var row Approval
	err := db.WithContext(ctx).Where("status = ? AND expires_at > ?", "approved", now).Order("id ASC").First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return row, false, nil
	}
	return row, err == nil, err
}

// lockApproval uses the same parent-first ordering as Run admission and verification.
// Transactions that check member scope use READ COMMITTED: the identity lookup
// below must not freeze a REPEATABLE READ view before waiting for the parent.
// Ingest holds that parent through its member writes and commit.
func lockApproval(ctx context.Context, tx *gorm.DB, id uint64) (Approval, Incident, error) {
	var row Approval
	if err := tx.WithContext(ctx).First(&row, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return row, Incident{}, ErrApprovalNotFound
		}
		return row, Incident{}, err
	}
	parent, err := lockIncident(ctx, tx, row.IncidentID)
	if err != nil {
		return row, parent, err
	}
	err = tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).First(&row, id).Error
	return row, parent, err
}

func (db *DB) ClaimApprovalExecution(ctx context.Context, id uint64, now time.Time, policy RemediationPolicy) (row Approval, claimed bool, err error) {
	if id == 0 || now.IsZero() {
		return row, false, errors.New("store: execution id and claim time required")
	}
	now = now.UTC().Truncate(time.Millisecond)
	err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var parent Incident
		var err error
		row, parent, err = lockApproval(ctx, tx, id)
		if errors.Is(err, ErrApprovalNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if row.Status != "approved" {
			return nil
		}
		reason, err := claimRefusal(ctx, tx, row, parent, now, policy)
		if err != nil {
			return err
		}
		if reason != "" {
			if err := tx.WithContext(ctx).Model(&Approval{}).Where("id = ? AND status = ?", id, "approved").Update("status", "expired").Error; err != nil {
				return err
			}
			row.Status = "expired"
			return appendApprovalEvent(ctx, tx, row, eventlog.EventApprovalExpired, "expired", truncateStoreText(reason, 512), now)
		}
		operation := fmt.Sprintf("op-%d", row.ID)
		result := tx.WithContext(ctx).Model(&Approval{}).Where("id = ? AND status = ?", id, "approved").
			Updates(map[string]any{"status": "executing", "operation_id": operation, "operation_started_at": now})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return nil
		}
		if err := appendApprovalEvent(ctx, tx, row, eventlog.EventExecutionStarted, "executing", "approval execution started as "+operation, now); err != nil {
			return err
		}
		row.Status, row.OperationID, row.OperationStartedAt = "executing", &operation, &now
		claimed = true
		return nil
	}, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	return row, claimed && err == nil, err
}

// claimRefusal rechecks, under the incident, approval and service locks, every
// condition the policy checked when the snapshot was frozen. A non-empty
// reason expires the approval: an old snapshot is never re-targeted.
func claimRefusal(ctx context.Context, tx *gorm.DB, row Approval, parent Incident, now time.Time, policy RemediationPolicy) (string, error) {
	if !row.ExpiresAt.After(now) {
		return "approval expired", nil
	}
	snapshot, parseErr := incidentrule.ParseExecutionContext(row.ExecutionContext)
	hash, hashErr := incidentrule.PlanHash(row.ToolName, row.ArgsJSON, row.ExecutionContext)
	if parseErr != nil || hashErr != nil || hash != row.PlanHash {
		return "approval execution snapshot is invalid", nil
	}
	if err := snapshot.ValidateBinding(row.ToolName, policy.Binding); err != nil {
		return err.Error(), nil
	}
	primary := snapshot.Kind == incidentrule.KindPrimary
	if primary && parent.Status != incidentrule.StatusFiring {
		return "incident is no longer firing", nil
	}
	members, err := listIncidentExecutionMembers(ctx, tx, parent.ID)
	if err != nil {
		return "", err
	}
	if err := snapshot.ValidateMembers(members, true); err != nil {
		return err.Error(), nil
	}
	if err := lockService(ctx, tx, snapshot.Service, now); err != nil {
		return "", err
	}
	family := row.ID
	if row.ParentApprovalID != nil {
		family = *row.ParentApprovalID
	}
	budget := policy.Budgets[snapshot.Rule.ID]
	state, err := remediationState(ctx, tx, RemediationQuery{Service: snapshot.Service, RuleID: snapshot.Rule.ID, IncidentID: parent.ID, Since: now.Add(-budget.Window), Family: family})
	if err != nil {
		return "", err
	}
	switch {
	case state.Stopped:
		return "emergency stop is active: " + state.StopReason, nil
	case state.BusyWith != 0:
		return fmt.Sprintf("service %s is busy with approval %d", snapshot.Service, state.BusyWith), nil
	case !primary:
		return "", nil
	case policy.Maintenance != "":
		return "maintenance window: " + policy.Maintenance, nil
	case budget.Max < 1 || state.Executions >= budget.Max:
		return fmt.Sprintf("rule %s budget exhausted (%d executions in %s)", snapshot.Rule.ID, state.Executions, budget.Window), nil
	case snapshot.Rule.Mode == incidentrule.ModeAuto && state.Blocked != "":
		return "rule " + snapshot.Rule.ID + " is blocked: " + state.Blocked, nil
	case snapshot.Rule.Mode == incidentrule.ModeAuto && state.IncidentActions > 0:
		return "a primary action already ran in this incident; a new action needs a new authorization", nil
	}
	return "", nil
}

// ListExecutingApprovals returns executions interrupted by a previous process.
func (db *DB) ListExecutingApprovals(ctx context.Context) ([]Approval, error) {
	var rows []Approval
	if err := db.WithContext(ctx).Where("status = ?", "executing").Order("id ASC").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("store: list executing approvals: %w", err)
	}
	return rows, nil
}

func (db *DB) FinishExecution(ctx context.Context, completion ExecutionCompletion) error {
	if completion.ApprovalID == 0 || completion.FinishedAt.IsZero() {
		return errors.New("store: execution id and completion time required")
	}
	if completion.Status != "executed" && completion.Status != "aborted" && completion.Status != "failed" {
		return errors.New("store: invalid execution final status")
	}
	if !json.Valid(completion.ResultJSON) {
		return errors.New("store: execution result must be JSON")
	}
	canonical, err := incidentrule.CanonicalJSON(completion.ResultJSON)
	if err != nil {
		return err
	}
	bounded := boundedExecutionResult(canonical)
	now := completion.FinishedAt.UTC().Truncate(time.Millisecond)
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		row, parent, err := lockApproval(ctx, tx, completion.ApprovalID)
		if err != nil {
			return err
		}
		if row.Status != "executing" {
			if row.Status != completion.Status || row.ResultJSON == nil {
				return ErrApprovalConflict
			}
			previous, err := incidentrule.CanonicalJSON(*row.ResultJSON)
			if err != nil || !bytes.Equal(previous, bounded) {
				return ErrApprovalConflict
			}
			return nil
		}
		// An old-format snapshot cannot be verified; its interrupted execution
		// is recorded as failed with a manual check and nothing else.
		snapshot, snapshotErr := incidentrule.ParseExecutionContext(row.ExecutionContext)
		if snapshotErr != nil && completion.Status != "failed" {
			return snapshotErr
		}
		if err := tx.WithContext(ctx).Model(&Approval{}).Where("id = ? AND status = ?", row.ID, "executing").Updates(map[string]any{"status": completion.Status, "result_json": bounded}).Error; err != nil {
			return err
		}
		eventType := map[string]eventlog.EventType{"executed": eventlog.EventExecutionCompleted, "failed": eventlog.EventExecutionFailed, "aborted": eventlog.EventExecutionAborted}[completion.Status]
		if err := appendApprovalEvent(ctx, tx, row, eventType, completion.Status, "approval execution "+completion.Status, now); err != nil {
			return err
		}
		switch completion.Status {
		case "executed":
			task := VerifyTask{ApprovalID: row.ID, Status: "pending", Phase: "verify", NextCheckAt: now, DeadlineAt: now.Add(time.Duration(snapshot.Verification.WindowSeconds) * time.Second), CreatedAt: now}
			if err := tx.WithContext(ctx).Create(&task).Error; err != nil {
				return err
			}
			if err := appendApprovalEvent(ctx, tx, row, eventlog.EventVerifyQueued, "pending", "recovery verification queued", now); err != nil {
				return err
			}
			if _, err := resolveIncidentProblem(ctx, tx, row.IncidentID, "execution_failed", &row.RunID, now); err != nil {
				return err
			}
		case "aborted":
			if _, err := openIncidentProblem(ctx, tx, IncidentProblem{IncidentID: row.IncidentID, RunID: &row.RunID, Code: "execution_aborted", Severity: "warning", Summary: "action refused before writing: the approved snapshot no longer matched the live object", FirstSeenAt: now, LastSeenAt: now}); err != nil {
				return err
			}
		case "failed":
			codes := []string{"execution_failed"}
			if completion.ManualCheck {
				codes = append(codes, "manual_check")
				// An unchanged read after timeout does not fence a late external write.
				// Stop every new action until an operator reconciles the target and resumes.
				if err := tx.WithContext(ctx).Create(&ControlEvent{Kind: ControlEmergencyStop, Actor: "system:execution",
					Reason: fmt.Sprintf("approval %d outcome is unknown; reconcile before resuming", row.ID), CreatedAt: now}).Error; err != nil {
					return err
				}
			}
			for _, code := range codes {
				if _, err := openIncidentProblem(ctx, tx, IncidentProblem{IncidentID: row.IncidentID, RunID: &row.RunID, Code: code, Severity: "error", Summary: "execution failed or result uncertain; manual verification required", FirstSeenAt: now, LastSeenAt: now}); err != nil {
					return err
				}
			}
		}
		if completion.Change != nil && completion.Status == "executed" {
			change := *completion.Change
			change.ApprovalID, change.Source, change.IdempotencyKey = &row.ID, "agent", fmt.Sprintf("approval-%d", row.ID)
			change.OccurredAt, change.CreatedAt = now, now
			// A rollback returns to a recorded release: it inherits that release's
			// declared migration and verification, which the next rollback reads.
			if change.ChangeType == "rollback" && change.ReleaseID != nil {
				var target ChangeEvent
				if err := tx.WithContext(ctx).Where("service = ? AND change_type = ? AND release_id = ?", change.Service, "release", *change.ReleaseID).Order("id DESC").Limit(1).Find(&target).Error; err != nil {
					return err
				}
				change.DBMigration, change.VerifiedAt = target.DBMigration, target.VerifiedAt
			}
			if err := insertChange(ctx, tx, change); err != nil {
				return err
			}
		}
		if snapshotErr == nil && snapshot.Kind == incidentrule.KindPrimary && completion.Status != "aborted" {
			var result struct {
				Detail string `json:"detail"`
				Error  string `json:"error"`
			}
			_ = json.Unmarshal(bounded, &result)
			brief := strings.TrimSpace(completion.Status + ": " + result.Detail + " " + result.Error)
			history := FaultCmdHistory{Fingerprint: incidentrule.FaultFingerprint(parent.GroupKey, snapshot.FaultAlert), ToolName: row.ToolName, ArgsJSON: row.ArgsJSON, ResultBrief: truncateStoreText(brief, 1024), ApprovalID: &row.ID, CreatedAt: now}
			if err := tx.WithContext(ctx).Create(&history).Error; err != nil {
				return err
			}
		}
		return nil
	}, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
}

const maxExecutionResultBytes = 8192

// result is canonical JSON. Retain content identity even when the body is omitted.
func boundedExecutionResult(result []byte) datatypes.JSON {
	if len(result) <= maxExecutionResultBytes {
		return datatypes.JSON(result)
	}
	digest := sha256.Sum256(result)
	bounded, _ := json.Marshal(map[string]any{"truncated": true, "reason": "execution result omitted", "result_sha256": fmt.Sprintf("%x", digest)})
	return datatypes.JSON(bounded)
}
