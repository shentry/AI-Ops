package store

import (
	"context"
	"time"

	"gorm.io/gorm"

	"oncall-agent/internal/eventlog"
)

// NotificationTask retries delivery, never the action that produced the event.
type NotificationTask struct {
	EventID       uint64        `gorm:"column:event_id;primaryKey;autoIncrement:false"`
	Attempts      int           `gorm:"column:attempts"`
	NextAttemptAt time.Time     `gorm:"column:next_attempt_at"`
	DeliveredAt   *time.Time    `gorm:"column:delivered_at"`
	LastError     *string       `gorm:"column:last_error"`
	Event         IncidentEvent `gorm:"foreignKey:EventID;references:ID"`
}

func (NotificationTask) TableName() string { return "notification_task" }

func requiresHumanAttention(event IncidentEvent) bool {
	// Offline upgrades are explicitly supervised and can run before this queue exists.
	if event.Phase == "upgrade" {
		return false
	}
	switch eventlog.EventType(event.EventType) {
	case eventlog.EventRunFailed, eventlog.EventExecutionFailed, eventlog.EventExecutionAborted,
		eventlog.EventVerifyFailed, eventlog.EventVerifyInconclusive, eventlog.EventVerifyRecurred:
		return true
	case eventlog.EventApprovalExpired, eventlog.EventNotificationFailed:
		return true
	case eventlog.EventEscalationRequired:
		return event.Phase == "diagnosis"
	}
	return false
}

func (db *DB) NextNotification(ctx context.Context, now time.Time) (NotificationTask, bool, error) {
	var task NotificationTask
	result := db.WithContext(ctx).Preload("Event").Where("delivered_at IS NULL AND next_attempt_at <= ?", now).
		Order("next_attempt_at ASC, event_id ASC").Limit(1).Find(&task)
	return task, result.Error == nil && result.RowsAffected == 1, result.Error
}

func (db *DB) FinishNotification(ctx context.Context, task NotificationTask, at time.Time, failure string) error {
	updates := map[string]any{"attempts": gorm.Expr("attempts + 1"), "last_error": nil}
	if failure == "" {
		updates["delivered_at"] = at
	} else {
		updates["last_error"] = truncateStoreText(failure, 512)
		updates["next_attempt_at"] = at.Add(time.Duration(1<<min(task.Attempts, 6)) * 5 * time.Second)
	}
	return db.WithContext(ctx).Model(&NotificationTask{}).Where("event_id = ? AND delivered_at IS NULL AND attempts = ?", task.EventID, task.Attempts).Updates(updates).Error
}
