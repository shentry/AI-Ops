package llm

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"oncall-agent/internal/config"
	"oncall-agent/internal/store"
)

var (
	// ErrModelNotAllowed means a caller requested an ID outside the server-owned
	// configuration allowlist.
	ErrModelNotAllowed = errors.New("llm: model is not allowed")
	// ErrModelSwitcherUnavailable means the runtime selector was not assembled.
	ErrModelSwitcherUnavailable = errors.New("llm: model switcher is unavailable")
)

// ModelSelection is the browser-safe current-model projection. It never
// contains provider URLs or credentials.
type ModelSelection struct {
	Model     string
	UpdatedAt time.Time
}

// ModelOption describes one server-configured selectable model.
type ModelOption struct {
	ID              string
	ThinkingEnabled bool
}

// ModelSelectionStore persists only the selected ID. *store.DB satisfies this
// interface; profiles remain exclusively in YAML configuration.
type ModelSelectionStore interface {
	GetLLMModelSelection(context.Context) (store.LLMModelSelection, error)
	GetOrInitializeLLMModelSelection(context.Context, string, time.Time) (store.LLMModelSelection, error)
	SetLLMModelSelection(context.Context, string, time.Time) (store.LLMModelSelection, error)
}

// ModelSwitcher coordinates the durable global selection and Factory cache.
// Its mutex serializes management changes within this process; the store's row
// lock serializes changes across processes.
type ModelSwitcher struct {
	store        ModelSelectionStore
	factory      *Factory
	profiles     map[string]config.ModelProfile
	options      []ModelOption
	defaultModel string
	now          func() time.Time
	mu           sync.Mutex
}

func NewModelSwitcher(db ModelSelectionStore, factory *Factory, cfg config.LLMConfig) (*ModelSwitcher, error) {
	if db == nil || factory == nil {
		return nil, ErrModelSwitcherUnavailable
	}
	defaultModel := strings.TrimSpace(cfg.Roles.Reasoner.Model)
	if defaultModel == "" {
		return nil, fmt.Errorf("llm: default reasoner model is required")
	}
	profiles := cfg.Models
	if len(profiles) == 0 {
		profiles = []config.ModelProfile{{ID: defaultModel, Thinking: cfg.Roles.Reasoner.Thinking}}
	}
	byID := make(map[string]config.ModelProfile, len(profiles))
	for _, profile := range profiles {
		profile.ID = strings.TrimSpace(profile.ID)
		if profile.ID == "" {
			return nil, fmt.Errorf("llm: selectable model id is required")
		}
		if _, exists := byID[profile.ID]; exists {
			return nil, fmt.Errorf("llm: duplicate selectable model %q", profile.ID)
		}
		byID[profile.ID] = profile
	}
	if _, exists := byID[defaultModel]; !exists {
		return nil, fmt.Errorf("llm: default model %q is not selectable", defaultModel)
	}
	options := make([]ModelOption, 0, len(byID))
	for _, profile := range byID {
		options = append(options, ModelOption{ID: profile.ID, ThinkingEnabled: profile.Thinking.Enabled})
	}
	sort.Slice(options, func(i, j int) bool { return options[i].ID < options[j].ID })
	return &ModelSwitcher{
		store:        db,
		factory:      factory,
		profiles:     byID,
		options:      options,
		defaultModel: defaultModel,
		now:          time.Now,
	}, nil
}

// Initialize restores the persisted selection, falling back to the configured
// default if a removed profile is no longer allowed.
func (s *ModelSwitcher) Initialize(ctx context.Context) (ModelSelection, error) {
	if s == nil {
		return ModelSelection{}, ErrModelSwitcherUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	selection, err := s.store.GetOrInitializeLLMModelSelection(ctx, s.defaultModel, s.now().UTC())
	if err != nil {
		return ModelSelection{}, fmt.Errorf("llm: initialize selected model: %w", err)
	}
	profile, allowed := s.profiles[selection.CurrentModel]
	if !allowed {
		selection, err = s.store.SetLLMModelSelection(ctx, s.defaultModel, s.now().UTC())
		if err != nil {
			return ModelSelection{}, fmt.Errorf("llm: reset unavailable selected model: %w", err)
		}
		profile = s.profiles[s.defaultModel]
	}
	if err := s.factory.SelectModel(profile); err != nil {
		return ModelSelection{}, err
	}
	return selectionDTO(selection), nil
}

func (s *ModelSwitcher) Current(ctx context.Context) (ModelSelection, error) {
	if s == nil {
		return ModelSelection{}, ErrModelSwitcherUnavailable
	}
	selection, err := s.store.GetLLMModelSelection(ctx)
	if err != nil {
		return ModelSelection{}, fmt.Errorf("llm: get selected model: %w", err)
	}
	return selectionDTO(selection), nil
}

func (s *ModelSwitcher) Options() []ModelOption {
	if s == nil {
		return nil
	}
	return append([]ModelOption(nil), s.options...)
}

// Select validates against the immutable configured allowlist, durably records
// the selection, then clears the Factory cache for future requests.
func (s *ModelSwitcher) Select(ctx context.Context, model string) (ModelSelection, error) {
	if s == nil {
		return ModelSelection{}, ErrModelSwitcherUnavailable
	}
	model = strings.TrimSpace(model)
	s.mu.Lock()
	defer s.mu.Unlock()
	profile, allowed := s.profiles[model]
	if !allowed {
		return ModelSelection{}, fmt.Errorf("%w: %s", ErrModelNotAllowed, model)
	}
	selection, err := s.store.SetLLMModelSelection(ctx, model, s.now().UTC())
	if err != nil {
		return ModelSelection{}, fmt.Errorf("llm: persist selected model: %w", err)
	}
	if err := s.factory.SelectModel(profile); err != nil {
		return ModelSelection{}, err
	}
	return selectionDTO(selection), nil
}

func selectionDTO(selection store.LLMModelSelection) ModelSelection {
	return ModelSelection{Model: selection.CurrentModel, UpdatedAt: selection.UpdatedAt.UTC()}
}
