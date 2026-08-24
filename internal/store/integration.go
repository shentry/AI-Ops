package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// 飞书侧持久化：integration_event_receipt 做回调幂等，
// im_binding 把消息/话题映射回 incident。

// ErrIMBindingNotFound 是飞书消息绑定未命中的哨兵错误。
var ErrIMBindingNotFound = errors.New("store: im binding not found")

const integrationReceiptLease = 5 * time.Minute

// ClaimIntegrationEventReceipt atomically claims a third-party event ID.
// Completed receipts are immutable; a processing receipt can be reclaimed
// only after its lease expires, recovering callbacks interrupted by a crash.
func (db *DB) ClaimIntegrationEventReceipt(ctx context.Context, receipt IntegrationEventReceipt) (bool, error) {
	if db == nil || db.DB == nil {
		return false, errors.New("store: database is required")
	}
	receipt.EventID = strings.TrimSpace(receipt.EventID)
	receipt.Provider = strings.TrimSpace(receipt.Provider)
	receipt.EventType = strings.TrimSpace(receipt.EventType)
	if receipt.EventID == "" {
		return false, errors.New("store: integration receipt event_id is required")
	}
	if receipt.Provider == "" || receipt.EventType == "" {
		return false, errors.New("store: integration receipt provider and event_type are required")
	}
	if len([]rune(receipt.EventID)) > 128 || len([]rune(receipt.Provider)) > 16 || len([]rune(receipt.EventType)) > 64 {
		return false, errors.New("store: integration receipt field is too long")
	}
	if receipt.ProcessedAt.IsZero() {
		receipt.ProcessedAt = time.Now().UTC()
	} else {
		receipt.ProcessedAt = receipt.ProcessedAt.UTC()
	}
	if strings.TrimSpace(receipt.Result) == "" {
		receipt.Result = "processing"
	}
	if receipt.Result != "processing" || len([]rune(receipt.Result)) > 32 {
		return false, errors.New("store: integration receipt claim must use processing result")
	}
	created := db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&receipt)
	if created.Error != nil {
		return false, fmt.Errorf("store: claim integration receipt: %w", created.Error)
	}
	if created.RowsAffected == 1 {
		return true, nil
	}
	staleBefore := receipt.ProcessedAt.Add(-integrationReceiptLease)
	reclaimed := db.WithContext(ctx).Model(&IntegrationEventReceipt{}).
		Where("event_id = ? AND result = ? AND processed_at < ?", receipt.EventID, "processing", staleBefore).
		Updates(map[string]any{"provider": receipt.Provider, "event_type": receipt.EventType, "processed_at": receipt.ProcessedAt})
	if reclaimed.Error != nil {
		return false, fmt.Errorf("store: reclaim integration receipt: %w", reclaimed.Error)
	}
	return reclaimed.RowsAffected == 1, nil
}

// CompleteIntegrationEventReceipt records the terminal handling result. It
// intentionally does not require the row to be in a particular intermediate
// state so recovery/replay tooling can safely finalize an older receipt.
func (db *DB) CompleteIntegrationEventReceipt(ctx context.Context, eventID, result string, processedAt time.Time) error {
	eventID = strings.TrimSpace(eventID)
	result = strings.TrimSpace(result)
	if eventID == "" {
		return errors.New("store: integration receipt event_id is required")
	}
	if result == "" {
		return errors.New("store: integration receipt result is required")
	}
	if len([]rune(result)) > 32 {
		return errors.New("store: integration receipt result is too long")
	}
	if processedAt.IsZero() {
		processedAt = time.Now().UTC()
	}
	updates := map[string]any{"result": result, "processed_at": processedAt.UTC()}
	query := db.WithContext(ctx).Model(&IntegrationEventReceipt{}).Where("event_id = ?", eventID).Updates(updates)
	if query.Error != nil {
		return fmt.Errorf("store: complete integration receipt: %w", query.Error)
	}
	if query.RowsAffected == 0 {
		var existing IntegrationEventReceipt
		lookupErr := db.WithContext(ctx).Where("event_id = ?", eventID).First(&existing).Error
		if errors.Is(lookupErr, gorm.ErrRecordNotFound) {
			return gorm.ErrRecordNotFound
		}
		if lookupErr != nil {
			return fmt.Errorf("store: find integration receipt: %w", lookupErr)
		}
	}
	return nil
}

// GetIntegrationEventReceipt reads one receipt by its globally unique event ID.
func (db *DB) GetIntegrationEventReceipt(ctx context.Context, eventID string) (IntegrationEventReceipt, error) {
	eventID = strings.TrimSpace(eventID)
	if eventID == "" {
		return IntegrationEventReceipt{}, errors.New("store: integration receipt event_id is required")
	}
	var receipt IntegrationEventReceipt
	err := db.WithContext(ctx).First(&receipt, "event_id = ?", eventID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return IntegrationEventReceipt{}, gorm.ErrRecordNotFound
	}
	if err != nil {
		return IntegrationEventReceipt{}, fmt.Errorf("store: get integration receipt: %w", err)
	}
	return receipt, nil
}

// DeleteIntegrationEventReceipt removes a receipt only when a caller is
// deliberately abandoning a failed claim. Normal handlers should finalize it
// instead, keeping duplicate callbacks idempotent.
func (db *DB) DeleteIntegrationEventReceipt(ctx context.Context, eventID string) error {
	eventID = strings.TrimSpace(eventID)
	if eventID == "" {
		return errors.New("store: integration receipt event_id is required")
	}
	if err := db.WithContext(ctx).Where("event_id = ?", eventID).Delete(&IntegrationEventReceipt{}).Error; err != nil {
		return fmt.Errorf("store: delete integration receipt: %w", err)
	}
	return nil
}

// CreateIMBinding records a Feishu message/thread binding. Insert is
// idempotent on (provider,message_id), allowing a notification retry to reuse
// the existing binding rather than creating ambiguous Incident associations.
func (db *DB) CreateIMBinding(ctx context.Context, binding IMBinding) (IMBinding, error) {
	binding.Provider = strings.TrimSpace(binding.Provider)
	binding.ChatID = strings.TrimSpace(binding.ChatID)
	binding.MessageID = strings.TrimSpace(binding.MessageID)
	binding.MessageKind = strings.TrimSpace(binding.MessageKind)
	if binding.Provider == "" || binding.ChatID == "" || binding.MessageID == "" || binding.MessageKind == "" {
		return IMBinding{}, errors.New("store: im binding provider, chat_id, message_id and message_kind are required")
	}
	if binding.IncidentID == 0 {
		return IMBinding{}, errors.New("store: im binding incident is required")
	}
	if len([]rune(binding.Provider)) > 16 || len([]rune(binding.ChatID)) > 128 || len([]rune(binding.MessageID)) > 128 || len([]rune(binding.MessageKind)) > 32 {
		return IMBinding{}, errors.New("store: im binding field is too long")
	}
	if binding.CreatedAt.IsZero() {
		binding.CreatedAt = time.Now().UTC()
	} else {
		binding.CreatedAt = binding.CreatedAt.UTC()
	}
	result := db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&binding)
	if result.Error != nil {
		return IMBinding{}, fmt.Errorf("store: create im binding: %w", result.Error)
	}
	if result.RowsAffected == 1 {
		return binding, nil
	}
	var existing IMBinding
	if err := db.WithContext(ctx).Where("provider = ? AND message_id = ?", binding.Provider, binding.MessageID).First(&existing).Error; err != nil {
		return IMBinding{}, fmt.Errorf("store: find existing im binding: %w", err)
	}
	if existing.ChatID != binding.ChatID || existing.IncidentID != binding.IncidentID || existing.MessageKind != binding.MessageKind {
		return IMBinding{}, errors.New("store: im binding conflicts with existing message")
	}
	return existing, nil
}

// GetIMBinding reads the unique provider/message binding.
func (db *DB) GetIMBinding(ctx context.Context, provider, messageID string) (IMBinding, error) {
	provider = strings.TrimSpace(provider)
	messageID = strings.TrimSpace(messageID)
	if provider == "" || messageID == "" {
		return IMBinding{}, errors.New("store: im binding provider and message_id are required")
	}
	var binding IMBinding
	err := db.WithContext(ctx).Where("provider = ? AND message_id = ?", provider, messageID).First(&binding).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return IMBinding{}, gorm.ErrRecordNotFound
	}
	if err != nil {
		return IMBinding{}, fmt.Errorf("store: get im binding: %w", err)
	}
	return binding, nil
}

// FindIMBinding resolves a message to its Incident by exact chat and any
// message/thread identity. Message ID is preferred over root/thread matches,
// and newest binding wins when several messages share a thread.
func (db *DB) FindIMBinding(ctx context.Context, provider, chatID, messageID, rootMessageID, threadID string) (IMBinding, error) {
	provider = strings.TrimSpace(provider)
	chatID = strings.TrimSpace(chatID)
	messageID = strings.TrimSpace(messageID)
	rootMessageID = strings.TrimSpace(rootMessageID)
	threadID = strings.TrimSpace(threadID)
	if provider == "" || chatID == "" {
		return IMBinding{}, errors.New("store: im binding provider and chat_id are required")
	}
	if messageID == "" && rootMessageID == "" && threadID == "" {
		return IMBinding{}, errors.New("store: im binding message identity is required")
	}
	query := db.WithContext(ctx).Where("provider = ? AND chat_id = ?", provider, chatID)
	identities := make([]string, 0, 3)
	args := make([]any, 0, 3)
	if messageID != "" {
		identities = append(identities, "message_id = ?")
		args = append(args, messageID)
	}
	if rootMessageID != "" {
		identities = append(identities, "root_message_id = ?")
		args = append(args, rootMessageID)
	}
	if threadID != "" {
		identities = append(identities, "thread_id = ?")
		args = append(args, threadID)
	}
	query = query.Where("("+strings.Join(identities, " OR ")+")", args...)
	var binding IMBinding
	err := query.Order(gorm.Expr("CASE WHEN message_id = ? THEN 0 WHEN root_message_id = ? THEN 1 ELSE 2 END", messageID, rootMessageID)).Order("created_at DESC, id DESC").First(&binding).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return IMBinding{}, gorm.ErrRecordNotFound
	}
	if err != nil {
		return IMBinding{}, fmt.Errorf("store: find im binding: %w", err)
	}
	return binding, nil
}

// LatestIMBinding 返回该 Incident 在指定 provider 下最新一条绑定。
func (db *DB) LatestIMBinding(ctx context.Context, provider string, incidentID uint64, messageKind string) (IMBinding, error) {
	provider = strings.TrimSpace(provider)
	messageKind = strings.TrimSpace(messageKind)
	if provider == "" || incidentID == 0 {
		return IMBinding{}, errors.New("store: im binding lookup is incomplete")
	}
	query := db.WithContext(ctx).Where("provider = ? AND incident_id = ?", provider, incidentID)
	if messageKind != "" {
		query = query.Where("message_kind = ?", messageKind)
	}
	var binding IMBinding
	err := query.Order("id DESC").First(&binding).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return IMBinding{}, ErrIMBindingNotFound
	}
	if err != nil {
		return IMBinding{}, fmt.Errorf("store: latest im binding: %w", err)
	}
	return binding, nil
}
