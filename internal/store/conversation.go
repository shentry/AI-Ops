package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
)

// conversation_message 提问队列：入队、CAS 领取、超时重排、终态回写。

// ErrConversationMessageNotFound 是对话消息不存在的哨兵错误。
var ErrConversationMessageNotFound = errors.New("store: conversation message not found")

// CreateConversationMessage 插入一条对话消息，供 Ask/Feishu worker 共享。
func (db *DB) CreateConversationMessage(ctx context.Context, message ConversationMessage) (ConversationMessage, error) {
	if message.IncidentID == 0 {
		return ConversationMessage{}, errors.New("store: conversation message incident is required")
	}
	if strings.TrimSpace(message.Channel) == "" || strings.TrimSpace(message.Role) == "" || strings.TrimSpace(message.Status) == "" {
		return ConversationMessage{}, errors.New("store: conversation message channel, role and status are required")
	}
	if message.Content == "" {
		return ConversationMessage{}, errors.New("store: conversation message content is required")
	}
	if message.CreatedAt.IsZero() {
		return ConversationMessage{}, errors.New("store: conversation message created_at is required")
	}
	if message.MetadataJSON != nil && len(*message.MetadataJSON) > 0 && !json.Valid(*message.MetadataJSON) {
		return ConversationMessage{}, errors.New("store: conversation message metadata must be valid JSON")
	}
	if err := db.WithContext(ctx).Create(&message).Error; err != nil {
		return ConversationMessage{}, fmt.Errorf("store: create conversation message: %w", err)
	}
	return message, nil
}

// ListConversationMessages 按严格 id > afterID、id ASC 回放对话消息。
func (db *DB) ListConversationMessages(ctx context.Context, incidentID, afterID uint64, limit int) ([]ConversationMessage, error) {
	if incidentID == 0 {
		return nil, errors.New("store: conversation message incident is required")
	}
	rows := make([]ConversationMessage, 0)
	err := db.WithContext(ctx).Where("incident_id = ? AND id > ?", incidentID, afterID).
		Order("id ASC").Limit(normalizePageLimit(limit)).Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("store: list conversation messages: %w", err)
	}
	return rows, nil
}

// ClaimConversationMessage CAS queued -> running and records the lease start.
func (db *DB) ClaimConversationMessage(ctx context.Context, id uint64, now time.Time) (bool, error) {
	if id == 0 {
		return false, errors.New("store: conversation message id is required")
	}
	if now.IsZero() {
		return false, errors.New("store: conversation message claim time is required")
	}
	result := db.WithContext(ctx).Model(&ConversationMessage{}).
		Where("id = ? AND status = ?", id, "queued").
		Updates(map[string]any{"status": "running", "claimed_at": now.UTC()})
	if result.Error != nil {
		return false, fmt.Errorf("store: claim conversation message: %w", result.Error)
	}
	return result.RowsAffected > 0, nil
}

// CompleteConversationMessage CAS running -> completed/failed and clears the lease.
func (db *DB) CompleteConversationMessage(ctx context.Context, id uint64, status string, finishedAt time.Time) error {
	if id == 0 {
		return errors.New("store: conversation message id is required")
	}
	if status != "completed" && status != "failed" {
		return errors.New("store: conversation message final status must be completed or failed")
	}
	if finishedAt.IsZero() {
		return errors.New("store: conversation message finished_at is required")
	}
	result := db.WithContext(ctx).Model(&ConversationMessage{}).
		Where("id = ? AND status = ?", id, "running").
		Updates(map[string]any{"status": status, "finished_at": finishedAt.UTC(), "claimed_at": nil})
	if result.Error != nil {
		return fmt.Errorf("store: complete conversation message: %w", result.Error)
	}
	if result.RowsAffected > 0 {
		return nil
	}
	var existing ConversationMessage
	findErr := db.WithContext(ctx).First(&existing, id).Error
	if errors.Is(findErr, gorm.ErrRecordNotFound) {
		return ErrConversationMessageNotFound
	}
	if findErr != nil {
		return fmt.Errorf("store: find conversation message: %w", findErr)
	}
	return fmt.Errorf("store: conversation message %d is not running", id)
}

// RequeueStaleConversationMessages recovers a question claimed by a process
// that died before writing its terminal state. A running worker has five
// minutes to complete the LLM call before another poller may reclaim it.
func (db *DB) RequeueStaleConversationMessages(ctx context.Context, staleBefore time.Time) (int64, error) {
	if staleBefore.IsZero() {
		return 0, errors.New("store: stale conversation threshold is required")
	}
	result := db.WithContext(ctx).Model(&ConversationMessage{}).
		Where("role = ? AND status = ? AND claimed_at IS NOT NULL AND claimed_at < ?", "user", "running", staleBefore.UTC()).
		Updates(map[string]any{"status": "queued", "claimed_at": nil})
	if result.Error != nil {
		return 0, fmt.Errorf("store: requeue stale conversation messages: %w", result.Error)
	}
	return result.RowsAffected, nil
}

// NextQueuedConversationMessage returns the oldest queued message for worker polling.
// ClaimConversationMessage performs the concurrent CAS after this best-effort read.
func (db *DB) NextQueuedConversationMessage(ctx context.Context) (ConversationMessage, bool, error) {
	var message ConversationMessage
	err := db.WithContext(ctx).Where("status = ?", "queued").Order("id ASC").First(&message).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ConversationMessage{}, false, nil
	}
	if err != nil {
		return ConversationMessage{}, false, fmt.Errorf("store: get next queued conversation message: %w", err)
	}
	return message, true, nil
}
