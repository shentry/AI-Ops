package store

import (
	"context"
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
)

// Control event kinds.
const (
	ControlEmergencyStop   = "emergency_stop"
	ControlEmergencyResume = "emergency_resume"
	ControlRuleReset       = "rule_reset"
	ControlRulesLoaded     = "rules_loaded"
)

// blockingEvents are the persisted facts that block a rule's new automatic
// primary actions until an operator resets it: an unknown or failed execution,
// a failed or inconclusive recovery, a recurrence, or an action reviewed wrong.
var blockingEvents = []string{
	string(eventlog.EventExecutionFailed), string(eventlog.EventVerifyFailed),
	string(eventlog.EventVerifyInconclusive), string(eventlog.EventVerifyRecurred),
}

// RemediationQuery names what a policy decision or claim must know.
type RemediationQuery struct {
	Service    string
	RuleID     string
	IncidentID uint64
	// Since is the start of the rule's budget window.
	Since time.Time
	// Family is the approval whose own chain (itself and its compensation)
	// does not count as another disposition of the service.
	Family uint64
}

// RemediationState is computed from persisted facts only: there is no second
// counter or lock table that could drift from the approval and event history.
type RemediationState struct {
	Stopped    bool
	StopReason string
	// Blocked explains why the rule is blocked; empty when it is not.
	Blocked string
	// Executions are real executions under the rule since Query.Since.
	Executions int
	// BusyWith is another approval of the service still executing, queued as a
	// compensation, or verifying; zero when the service is free.
	BusyWith uint64
	// IncidentActions are primary actions already attempted in the incident.
	IncidentActions int
}

func (db *DB) RemediationState(ctx context.Context, q RemediationQuery) (RemediationState, error) {
	return remediationState(ctx, db.DB, q)
}

func remediationState(ctx context.Context, tx *gorm.DB, q RemediationQuery) (RemediationState, error) {
	var state RemediationState
	var stop ControlEvent
	err := tx.WithContext(ctx).Where("kind IN ?", []string{ControlEmergencyStop, ControlEmergencyResume}).Order("id DESC").Limit(1).Find(&stop).Error
	if err != nil {
		return state, fmt.Errorf("store: read emergency stop: %w", err)
	}
	state.Stopped, state.StopReason = stop.Kind == ControlEmergencyStop, stop.Reason

	if q.RuleID != "" {
		var reset ControlEvent
		if err := tx.WithContext(ctx).Where("kind = ? AND rule_id = ?", ControlRuleReset, q.RuleID).Order("id DESC").Limit(1).Find(&reset).Error; err != nil {
			return state, fmt.Errorf("store: read rule reset: %w", err)
		}
		var blocking IncidentEvent
		query := tx.WithContext(ctx).Table("incident_event").Select("incident_event.*").
			Joins("JOIN approval ON approval.id = incident_event.approval_id").
			Where("approval.rule_id = ?", q.RuleID).
			Where("incident_event.event_type IN ? OR (incident_event.event_type = ? AND incident_event.phase = 'action' AND incident_event.status = 'wrong')",
				blockingEvents, string(eventlog.EventReviewRecorded))
		if reset.ID != 0 {
			query = query.Where("incident_event.created_at > ?", reset.CreatedAt)
		}
		if err := query.Order("incident_event.id DESC").Limit(1).Find(&blocking).Error; err != nil {
			return state, fmt.Errorf("store: read rule blocks: %w", err)
		}
		if blocking.ID != 0 {
			state.Blocked = fmt.Sprintf("%s on approval %d at %s", blocking.EventType, derefID(blocking.ApprovalID), blocking.CreatedAt.UTC().Format(time.RFC3339))
		}
		var executions int64
		if err := tx.WithContext(ctx).Model(&Approval{}).
			Where("rule_id = ? AND parent_approval_id IS NULL AND operation_started_at >= ? AND status IN ?", q.RuleID, q.Since, []string{"executing", "executed", "failed"}).
			Count(&executions).Error; err != nil {
			return state, fmt.Errorf("store: count rule executions: %w", err)
		}
		state.Executions = int(executions)
	}

	if q.Service != "" {
		var busy Approval
		err := tx.WithContext(ctx).Table("approval").Select("approval.id").
			Joins("LEFT JOIN verify_task ON verify_task.approval_id = approval.id").
			Where("approval.service = ? AND approval.id <> ? AND COALESCE(approval.parent_approval_id, approval.id) <> ?", q.Service, q.Family, q.Family).
			Where("approval.status = 'executing' OR (approval.status = 'approved' AND approval.parent_approval_id IS NOT NULL) OR (approval.status = 'executed' AND verify_task.phase = 'verify' AND verify_task.status IN ('pending','running'))").
			Order("approval.id ASC").Limit(1).Find(&busy).Error
		if err != nil {
			return state, fmt.Errorf("store: read service dispositions: %w", err)
		}
		state.BusyWith = busy.ID
	}

	if q.IncidentID != 0 {
		var actions int64
		if err := tx.WithContext(ctx).Model(&Approval{}).
			Where("incident_id = ? AND parent_approval_id IS NULL AND status IN ?", q.IncidentID, []string{"executing", "executed", "failed"}).
			Count(&actions).Error; err != nil {
			return state, fmt.Errorf("store: count incident actions: %w", err)
		}
		state.IncidentActions = int(actions)
	}
	return state, nil
}

func derefID(id *uint64) uint64 {
	if id == nil {
		return 0
	}
	return *id
}

// lockService serializes claims of one service; the row carries no state.
func lockService(ctx context.Context, tx *gorm.DB, service string, now time.Time) error {
	if err := tx.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&ServiceLock{Service: service, CreatedAt: now}).Error; err != nil {
		return err
	}
	var row ServiceLock
	return tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).First(&row, "service = ?", service).Error
}

// AppendControlEvent records an operator or system decision.
func (db *DB) AppendControlEvent(ctx context.Context, event ControlEvent) (ControlEvent, error) {
	switch event.Kind {
	case ControlEmergencyStop, ControlEmergencyResume, ControlRulesLoaded:
	case ControlRuleReset:
		if event.RuleID == nil || strings.TrimSpace(*event.RuleID) == "" {
			return ControlEvent{}, errors.New("store: rule reset names a rule")
		}
	default:
		return ControlEvent{}, fmt.Errorf("store: unknown control event %q", event.Kind)
	}
	if strings.TrimSpace(event.Actor) == "" || strings.TrimSpace(event.Reason) == "" || event.CreatedAt.IsZero() {
		return ControlEvent{}, errors.New("store: control event needs actor, reason and time")
	}
	event.ID = 0
	event.Reason = truncateStoreText(event.Reason, 512)
	event.CreatedAt = event.CreatedAt.UTC().Truncate(time.Millisecond)
	if err := db.WithContext(ctx).Create(&event).Error; err != nil {
		return ControlEvent{}, fmt.Errorf("store: append control event: %w", err)
	}
	return event, nil
}

func (db *DB) ListControlEvents(ctx context.Context, limit int) ([]ControlEvent, error) {
	rows := make([]ControlEvent, 0)
	if err := db.WithContext(ctx).Order("id DESC").Limit(normalizePageLimit(limit)).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("store: list control events: %w", err)
	}
	return rows, nil
}

// RecordRulesRelease audits the rules release this process runs with, once per
// distinct release. The rule content itself lives in version control.
func (db *DB) RecordRulesRelease(ctx context.Context, release string, rules any, now time.Time) error {
	var latest ControlEvent
	if err := db.WithContext(ctx).Where("kind = ?", ControlRulesLoaded).Order("id DESC").Limit(1).Find(&latest).Error; err != nil {
		return fmt.Errorf("store: read rules release: %w", err)
	}
	if latest.ID != 0 && latest.Reason == release {
		return nil
	}
	detail, err := json.Marshal(rules)
	if err != nil {
		return err
	}
	raw := datatypes.JSON(detail)
	_, err = db.AppendControlEvent(ctx, ControlEvent{Kind: ControlRulesLoaded, Actor: "config", Reason: release, DetailJSON: &raw, CreatedAt: now})
	return err
}

// AcquireExecutorLock holds a MySQL named lock on a dedicated connection for
// the life of the process. Startup recovery and claims assume one active
// executor; a second instance fails instead of racing the first.
func (db *DB) AcquireExecutorLock(ctx context.Context, name string) (release func(), err error) {
	sqlDB, err := db.DB.DB()
	if err != nil {
		return nil, err
	}
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: executor lock connection: %w", err)
	}
	var got sql.NullInt64
	if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, 0)", name).Scan(&got); err != nil {
		conn.Close()
		return nil, fmt.Errorf("store: acquire executor lock: %w", err)
	}
	if !got.Valid || got.Int64 != 1 {
		conn.Close()
		return nil, errors.New("store: another executor instance holds the execution lock")
	}
	db.executionLease = &executionLease{conn: conn, name: name}
	return func() {
		_, _ = conn.ExecContext(context.Background(), "SELECT RELEASE_LOCK(?)", name)
		conn.Close()
	}, nil
}
