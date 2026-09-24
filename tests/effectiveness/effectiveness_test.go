package effectiveness

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"oncall-agent/internal/config"
	"oncall-agent/internal/diagnose"
	"oncall-agent/internal/llm"
	"oncall-agent/internal/tools"
)

// Expectations are evaluator-only data; only Evidence.Render() reaches the model.
type scenario struct {
	ID                 string                  `json:"id"`
	ExpectedDiagnosis  string                  `json:"expected_diagnosis"`
	Evidence           []diagnose.EvidenceItem `json:"evidence"`
	RequiredRefs       []string                `json:"required_refs"`
	AllowedActions     []string                `json:"allowed_actions"`
	AllowedConfidences []string                `json:"allowed_confidences"`
	AllowedTargets     []string                `json:"allowed_targets"`
}

func scenarios(t *testing.T) []scenario {
	t.Helper()
	data, err := os.ReadFile("cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []scenario
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	return cases
}

func TestScenarioSpecifications(t *testing.T) {
	seen := map[string]bool{}
	cases := scenarios(t)
	if len(cases) == 0 {
		t.Fatal("empty evaluation set")
	}
	for _, c := range cases {
		if c.ID == "" || seen[c.ID] || c.ExpectedDiagnosis == "" || len(c.Evidence) == 0 || len(c.AllowedActions) == 0 || len(c.RequiredRefs) == 0 {
			t.Fatalf("incomplete or duplicate scenario: %q", c.ID)
		}
		seen[c.ID] = true
		names := map[string]bool{}
		for _, item := range c.Evidence {
			if item.Name == "" || names[item.Name] || item.Source == "" || item.CollectedAt.IsZero() || !slices.Contains([]string{"ok", "error", "missing"}, item.Status) {
				t.Fatalf("invalid evidence in %s: %s", c.ID, item.Name)
			}
			names[item.Name] = true
		}
		for _, ref := range c.RequiredRefs {
			if !names[ref] {
				t.Fatalf("%s requires nonexistent evidence %s", c.ID, ref)
			}
		}
		for _, action := range c.AllowedActions {
			if !slices.Contains([]string{"none", tools.ToolDockerRestart}, action) || (action == tools.ToolDockerRestart && len(c.AllowedTargets) == 0) {
				t.Fatalf("invalid action/target specification in %s", c.ID)
			}
		}
	}
}

type observation struct {
	CaseID            string                `json:"case_id"`
	Trial             int                   `json:"trial"`
	Model             string                `json:"model"`
	StartedAt         time.Time             `json:"started_at"`
	ElapsedSeconds    float64               `json:"elapsed_seconds"`
	ExpectedDiagnosis string                `json:"expected_diagnosis"`
	Result            *llm.DiagnoseResult   `json:"result"`
	Guard             *diagnose.GuardResult `json:"guard,omitempty"`
	CheckFailures     []string              `json:"check_failures"`
	Error             string                `json:"error,omitempty"`
	DiagnosisReview   string                `json:"diagnosis_review"`
}

// Explicit opt-in prevents ordinary go test from spending model credits.
// Missing credentials after opt-in FAIL, rather than producing a green skip.
func TestReasonerEffectiveness(t *testing.T) {
	if os.Getenv("EFFECT_EVAL") != "1" {
		t.Skip("set EFFECT_EVAL=1 and configure a real model; fixture checks alone are not an effectiveness result")
	}
	cfg := modelConfig(t)
	factory := llm.NewFactory(cfg)
	if err := factory.Validate(); err != nil {
		t.Fatal(err)
	}
	repeats := 1
	if value := os.Getenv("EFFECT_EVAL_REPEATS"); value != "" {
		var err error
		repeats, err = strconv.Atoi(value)
		if err != nil || repeats < 1 || repeats > 5 {
			t.Fatal("EFFECT_EVAL_REPEATS must be between 1 and 5")
		}
	}
	var report *os.File
	if path := os.Getenv("EFFECT_EVAL_OUTPUT"); path != "" {
		var err error
		report, err = os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			t.Fatal(err)
		}
		defer report.Close()
	}
	// No live data sources and no mutation tools. Additional metric discovery
	// truthfully returns no recorded series; all available facts are in the snapshot.
	registry := tools.NewRegistry()
	if err := registry.Register(tools.ToolSpec{
		Name: tools.ToolPromSeriesMeta, Description: "List available metric series in the frozen incident snapshot",
		Level: tools.L1ReadOnly, Timeout: time.Second, MaxOutput: 1024,
		Params: []tools.ParamSpec{{Name: "match", Description: "series selector", Required: true}},
		Handler: func(context.Context, json.RawMessage) (string, error) {
			return `{"series":[],"returned":0,"truncated":false}`, nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	reasoner := llm.NewReasoner(factory, registry, config.DefaultDiagnoseBudget())
	consecutiveErrors := 0
	for _, c := range scenarios(t) {
		for trial := 1; trial <= repeats; trial++ {
			if consecutiveErrors >= 3 {
				t.Fatal("stopped after three consecutive model execution errors; inspect connectivity/provider before retrying")
			}
			t.Run(fmt.Sprintf("%s/trial_%d", c.ID, trial), func(t *testing.T) {
				o := observation{CaseID: c.ID, Trial: trial, Model: cfg.Roles.Reasoner.Model,
					ExpectedDiagnosis: c.ExpectedDiagnosis, DiagnosisReview: "pending_human_review",
					CheckFailures: []string{}, StartedAt: time.Now().UTC()}
				ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
				defer cancel()
				result, err := reasoner.Diagnose(ctx, (diagnose.Evidence{IncidentID: 1, Items: c.Evidence}).Render(), "light")
				o.ElapsedSeconds = time.Since(o.StartedAt).Seconds()
				o.Result = result
				if err != nil {
					consecutiveErrors++
					o.Error = strings.ReplaceAll(diagnose.Sanitize(err.Error()), cfg.Roles.Reasoner.APIKey, "[REDACTED]")
				} else {
					consecutiveErrors = 0
					o.CheckFailures = check(c, result)
					guard := diagnose.Guard(result.RCA, result.Plan)
					o.Guard = &guard
				}
				if report != nil {
					if err := json.NewEncoder(report).Encode(o); err != nil {
						t.Fatal(err)
					}
					if err := report.Sync(); err != nil {
						t.Fatal(err)
					}
				}
				raw, _ := json.Marshal(o)
				t.Log(string(raw))
				if o.Error != "" || len(o.CheckFailures) != 0 {
					t.Errorf("model_error=%q check_failures=%v", o.Error, o.CheckFailures)
				}
			})
		}
	}
}

// These checks verify bounded contract/evidence-reference/action properties.
// Passing them is NOT a semantic root-cause accuracy score.
func check(c scenario, r *llm.DiagnoseResult) []string {
	failures := []string{}
	if !slices.Contains(c.AllowedActions, r.Plan.Action) {
		failures = append(failures, "unexpected_action:"+r.Plan.Action)
	}
	if r.Plan.Action != "none" && (r.Plan.Target.Kind != "container" || !slices.Contains(c.AllowedTargets, r.Plan.Target.Name)) {
		failures = append(failures, "unsupported_action_target")
	}
	if len(c.AllowedConfidences) > 0 && !slices.Contains(c.AllowedConfidences, r.Confidence) {
		failures = append(failures, "unexpected_confidence:"+r.Confidence)
	}
	for _, ref := range c.RequiredRefs {
		if !slices.Contains(r.EvidenceRefs, ref) {
			failures = append(failures, "missing_required_reference:"+ref)
		}
	}
	for _, ref := range r.EvidenceRefs {
		if !slices.ContainsFunc(c.Evidence, func(item diagnose.EvidenceItem) bool { return item.Name == ref }) {
			failures = append(failures, "nonexistent_reference:"+ref)
		}
	}
	return failures
}

func TestChecksRejectUnsupportedFindings(t *testing.T) {
	c := scenario{Evidence: []diagnose.EvidenceItem{{Name: "docker"}}, RequiredRefs: []string{"docker"},
		AllowedActions: []string{"none"}, AllowedConfidences: []string{"low"}}
	r := &llm.DiagnoseResult{Confidence: "high", EvidenceRefs: []string{"approved_admin"},
		Plan: llm.Plan{Action: tools.ToolDockerRestart, Target: llm.PlanTarget{Kind: "container", Name: "unrelated-victim"}}}
	want := []string{"unexpected_action:docker_restart", "unsupported_action_target", "unexpected_confidence:high",
		"missing_required_reference:docker", "nonexistent_reference:approved_admin"}
	if got := check(c, r); !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	r.Confidence, r.EvidenceRefs, r.Plan = "low", []string{"docker"}, llm.Plan{Action: "none"}
	if got := check(c, r); len(got) != 0 {
		t.Fatalf("valid abstention rejected: %v", got)
	}
}

func modelConfig(t *testing.T) config.LLMConfig {
	t.Helper()
	var cfg config.LLMConfig
	if path := os.Getenv("EFFECT_EVAL_CONFIG"); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		// Read only llm, without requiring database/notification credentials or
		// initializing any production integration from the server configuration.
		var document struct {
			LLM config.LLMConfig `yaml:"llm"`
		}
		if err := yaml.Unmarshal(data, &document); err != nil {
			t.Fatal("cannot parse evaluation model configuration")
		}
		cfg = document.LLM
		for _, field := range []*string{&cfg.Roles.Reasoner.BaseURL, &cfg.Roles.Reasoner.APIKey, &cfg.Roles.Reasoner.Model} {
			*field = os.Expand(*field, func(name string) string {
				value := os.Getenv(name)
				if value == "" {
					t.Fatalf("model configuration requires environment variable %s", name)
				}
				return value
			})
		}
	} else {
		cfg.Roles.Reasoner = config.RoleConfig{BaseURL: os.Getenv("TEST_LLM_BASE_URL"), APIKey: os.Getenv("TEST_LLM_API_KEY"), Model: os.Getenv("TEST_LLM_MODEL")}
	}
	if cfg.Roles.Reasoner.MaxTokens == 0 {
		cfg.Roles.Reasoner.MaxTokens = 2048
	}
	return cfg
}
