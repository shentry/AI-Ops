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

// llm_model_selection 单行表：全局模型选择的读写。

// ErrLLMModelSelectionNotFound means the singleton model-selection row has
// not been initialized. Startup creates it before any model is built.
var ErrLLMModelSelectionNotFound = errors.New("store: llm model selection not found")

// GetLLMModelSelection reads the singleton current-model row.
func (db *DB) GetLLMModelSelection(ctx context.Context) (LLMModelSelection, error) {
	var selection LLMModelSelection
	query := db.WithContext(ctx).Where("singleton_id = ?", 1).First(&selection)
	if errors.Is(query.Error, gorm.ErrRecordNotFound) {
		return LLMModelSelection{}, ErrLLMModelSelectionNotFound
	}
	if query.Error != nil {
		return LLMModelSelection{}, fmt.Errorf("store: get llm model selection: %w", query.Error)
	}
	return selection, nil
}

// GetOrInitializeLLMModelSelection creates the singleton only when absent.
// The configured default never overwrites a previously selected model.
func (db *DB) GetOrInitializeLLMModelSelection(ctx context.Context, defaultModel string, now time.Time) (LLMModelSelection, error) {
	defaultModel, err := normalizeLLMModelName(defaultModel)
	if err != nil {
		return LLMModelSelection{}, err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	candidate := LLMModelSelection{SingletonID: 1, CurrentModel: defaultModel, UpdatedAt: now.UTC()}
	if err := db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&candidate).Error; err != nil {
		return LLMModelSelection{}, fmt.Errorf("store: initialize llm model selection: %w", err)
	}
	return db.GetLLMModelSelection(ctx)
}

// SetLLMModelSelection serializes global model changes with a row lock. The
// caller must validate the model against its configuration allowlist first.
func (db *DB) SetLLMModelSelection(ctx context.Context, model string, now time.Time) (LLMModelSelection, error) {
	model, err := normalizeLLMModelName(model)
	if err != nil {
		return LLMModelSelection{}, err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	var selection LLMModelSelection
	err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		query := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("singleton_id = ?", 1).First(&selection)
		if errors.Is(query.Error, gorm.ErrRecordNotFound) {
			return ErrLLMModelSelectionNotFound
		}
		if query.Error != nil {
			return fmt.Errorf("store: lock llm model selection: %w", query.Error)
		}
		selection.CurrentModel = model
		selection.UpdatedAt = now.UTC()
		if err := tx.Model(&selection).Updates(map[string]any{
			"current_model": selection.CurrentModel,
			"updated_at":    selection.UpdatedAt,
		}).Error; err != nil {
			return fmt.Errorf("store: set llm model selection: %w", err)
		}
		return nil
	})
	if err != nil {
		return LLMModelSelection{}, err
	}
	return selection, nil
}

func normalizeLLMModelName(model string) (string, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return "", errors.New("store: llm model is required")
	}
	if len([]rune(model)) > 128 {
		return "", errors.New("store: llm model is too long")
	}
	return model, nil
}
