package llm

import (
	"context"
	"errors"
	"github.com/cloudwego/eino/schema"
	"slices"
	"testing"
	"time"

	"oncall-agent/internal/config"
	"oncall-agent/internal/store"
)

type memoryModelSelectionStore struct {
	selection   store.LLMModelSelection
	initialized int
	sets        []string
	err         error
}

func (s *memoryModelSelectionStore) GetLLMModelSelection(context.Context) (store.LLMModelSelection, error) {
	if s.err != nil {
		return store.LLMModelSelection{}, s.err
	}
	if s.selection.SingletonID == 0 {
		return store.LLMModelSelection{}, store.ErrLLMModelSelectionNotFound
	}
	return s.selection, nil
}

func (s *memoryModelSelectionStore) GetOrInitializeLLMModelSelection(_ context.Context, defaultModel string, now time.Time) (store.LLMModelSelection, error) {
	s.initialized++
	if s.err != nil {
		return store.LLMModelSelection{}, s.err
	}
	if s.selection.SingletonID == 0 {
		s.selection = store.LLMModelSelection{SingletonID: 1, CurrentModel: defaultModel, UpdatedAt: now.UTC()}
	}
	return s.selection, nil
}

func (s *memoryModelSelectionStore) SetLLMModelSelection(_ context.Context, model string, now time.Time) (store.LLMModelSelection, error) {
	if s.err != nil {
		return store.LLMModelSelection{}, s.err
	}
	s.sets = append(s.sets, model)
	s.selection = store.LLMModelSelection{SingletonID: 1, CurrentModel: model, UpdatedAt: now.UTC()}
	return s.selection, nil
}

func testModelSwitchConfig() config.LLMConfig {
	return config.LLMConfig{
		Roles: config.LLMRoles{
			Reasoner:   config.RoleConfig{BaseURL: "http://127.0.0.1:1", APIKey: "key", Model: "glm-5", MaxTokens: 128},
			Summarizer: config.RoleConfig{BaseURL: "http://127.0.0.1:1", APIKey: "key", Model: "glm-5", MaxTokens: 64},
		},
		Models: []config.ModelProfile{
			{ID: "glm-5"},
			{ID: "deepseek-v4-pro", Thinking: config.ThinkingConfig{Enabled: true, Effort: "medium"}},
		},
	}
}

func TestModelSwitcherInitializesAndPersistsSelection(t *testing.T) {
	factory := NewFactory(testModelSwitchConfig())
	store := &memoryModelSelectionStore{}
	switcher, err := NewModelSwitcher(store, factory, testModelSwitchConfig())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 22, 21, 0, 0, 0, time.UTC)
	switcher.now = func() time.Time { return now }
	selection, err := switcher.Initialize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if selection.Model != "glm-5" || factory.CurrentModel() != "glm-5" || store.initialized != 1 {
		t.Fatalf("initial selection=%+v factory=%q store=%+v", selection, factory.CurrentModel(), store)
	}

	now = now.Add(time.Minute)
	selection, err = switcher.Select(context.Background(), "deepseek-v4-pro")
	if err != nil {
		t.Fatal(err)
	}
	if selection.Model != "deepseek-v4-pro" || factory.CurrentModel() != "deepseek-v4-pro" {
		t.Fatalf("selected=%+v factory=%q", selection, factory.CurrentModel())
	}
	if len(store.sets) != 1 || store.sets[0] != "deepseek-v4-pro" {
		t.Fatalf("persisted models=%v", store.sets)
	}
}

func TestModelSwitcherResetsRemovedPersistedModel(t *testing.T) {
	factory := NewFactory(testModelSwitchConfig())
	state := &memoryModelSelectionStore{selection: store.LLMModelSelection{SingletonID: 1, CurrentModel: "retired-model", UpdatedAt: time.Now().UTC()}}
	switcher, err := NewModelSwitcher(state, factory, testModelSwitchConfig())
	if err != nil {
		t.Fatal(err)
	}
	selection, err := switcher.Initialize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if selection.Model != "glm-5" || factory.CurrentModel() != "glm-5" || len(state.sets) != 1 || state.sets[0] != "glm-5" {
		t.Fatalf("reset result=%+v factory=%q store=%+v", selection, factory.CurrentModel(), state)
	}
}

func TestModelSwitcherRejectsUnconfiguredModelBeforeWrite(t *testing.T) {
	factory := NewFactory(testModelSwitchConfig())
	state := &memoryModelSelectionStore{selection: store.LLMModelSelection{SingletonID: 1, CurrentModel: "glm-5", UpdatedAt: time.Now().UTC()}}
	switcher, err := NewModelSwitcher(state, factory, testModelSwitchConfig())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := switcher.Select(context.Background(), "unapproved"); !errors.Is(err, ErrModelNotAllowed) {
		t.Fatalf("Select(unapproved) error=%v", err)
	}
	if len(state.sets) != 0 {
		t.Fatalf("unapproved model persisted: %v", state.sets)
	}
}

func TestModelSwitcherRebuildsFutureRoleClients(t *testing.T) {
	var models []string
	fake := newFakeOpenAIServer(t, func(_ int, body map[string]any) map[string]any {
		models = append(models, body["model"].(string))
		return chatResponse("ok", 1, 1)
	})
	cfg := testModelSwitchConfig()
	cfg.Roles.Reasoner.BaseURL = fake.server.URL
	cfg.Roles.Summarizer.BaseURL = fake.server.URL
	factory := NewFactory(cfg)
	state := &memoryModelSelectionStore{}
	switcher, err := NewModelSwitcher(state, factory, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := switcher.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	first, err := factory.Build(RoleReasoner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Generate(context.Background(), []*schema.Message{schema.UserMessage("before")}); err != nil {
		t.Fatal(err)
	}
	if _, err := switcher.Select(context.Background(), "deepseek-v4-pro"); err != nil {
		t.Fatal(err)
	}
	next, err := factory.Build(RoleReasoner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := next.Generate(context.Background(), []*schema.Message{schema.UserMessage("after")}); err != nil {
		t.Fatal(err)
	}
	summarizer, err := factory.Build(RoleSummarizer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := summarizer.Generate(context.Background(), []*schema.Message{schema.UserMessage("summary")}); err != nil {
		t.Fatal(err)
	}
	if got, want := models, []string{"glm-5", "deepseek-v4-pro", "deepseek-v4-pro"}; !slices.Equal(got, want) {
		t.Fatalf("models = %v, want %v", got, want)
	}
}
