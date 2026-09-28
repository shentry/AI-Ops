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
	"oncall-agent/internal/knowledge"
	"oncall-agent/internal/llm"
	"oncall-agent/internal/tools"
)

// Expectations are evaluator-only data; only Evidence.Render() reaches the model.
type scenario struct {
	ID string `json:"id"`
	// Alerts are the firing alert names in the snapshot; skills match on them.
	Alerts             []string                `json:"alerts"`
	ExpectedDiagnosis  string                  `json:"expected_diagnosis"`
	Evidence           []diagnose.EvidenceItem `json:"evidence"`
	RequiredRefs       []string                `json:"required_refs"`
	AllowedActions     []string                `json:"allowed_actions"`
	AllowedConfidences []string                `json:"allowed_confidences"`
	AllowedTargets     []string                `json:"allowed_targets"`
	// RestartGuard is the deterministic Guard decision for docker_restart on the
	// target container given this evidence, independent of what the model suggests.
	RestartGuard string `json:"restart_guard"`
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
	rules := alertRuleNames(t)
	if len(cases) == 0 {
		t.Fatal("empty evaluation set")
	}
	for _, c := range cases {
		if c.ID == "" || seen[c.ID] || c.ExpectedDiagnosis == "" || len(c.Evidence) == 0 || len(c.AllowedActions) == 0 || len(c.RequiredRefs) == 0 ||
			!slices.Contains([]string{diagnose.DecisionAllow, diagnose.DecisionDeny, diagnose.DecisionEscalate}, c.RestartGuard) {
			t.Fatalf("incomplete or duplicate scenario: %q", c.ID)
		}
		seen[c.ID] = true
		// Skills match real rule names; a case alert must be one and must be
		// the alert its snapshot shows.
		for _, alert := range c.Alerts {
			if !rules[alert] || !strings.Contains(c.Evidence[0].Body, "alert "+alert+":") {
				t.Fatalf("%s: alert %q is not a rule in alerts.yml shown by its alert_snapshot", c.ID, alert)
			}
		}
		if len(c.Alerts) == 0 || c.Evidence[0].Name != "alert_snapshot" {
			t.Fatalf("%s: needs its alerts and an alert_snapshot first", c.ID)
		}
		names := map[string]bool{}
		for _, item := range c.Evidence {
			if item.Name == "" || names[item.Name] || item.Source == "" || item.CollectedAt.IsZero() || !slices.Contains([]string{"ok", "partial", "error", "missing"}, item.Status) {
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
			if !slices.Contains([]string{"none", tools.ActionDockerRestart}, action) || (action == tools.ActionDockerRestart && len(c.AllowedTargets) == 0) {
				t.Fatalf("invalid action/target specification in %s", c.ID)
			}
		}
	}
}

// A restart is justified only by current structured facts. Whatever a model
// suggests, only the case whose evidence shows an identified, stopped, non
// self-healing container may pass; mis-association and anonymous OOM are denied.
func TestRestartGuardOnScenarioEvidence(t *testing.T) {
	for _, c := range scenarios(t) {
		evidence := diagnose.Evidence{IncidentID: 1, Items: c.Evidence}
		plan := llm.Plan{Action: tools.ActionDockerRestart, Target: llm.PlanTarget{Kind: "container", Name: "sub2api"}}
		if got := diagnose.Guard("", plan, evidence); got.Decision != c.RestartGuard {
			t.Errorf("%s: restart guard = %s (%s), want %s", c.ID, got.Decision, got.Reason, c.RestartGuard)
		}
		// An unidentified target never passes; a dependency outage escalates first.
		plan.Target.Name = "unrelated-victim"
		if got := diagnose.Guard("", plan, evidence); got.Decision == diagnose.DecisionAllow || got.Plan.Action != "none" {
			t.Errorf("%s: restart of an unidentified target = %s, want it stopped", c.ID, got.Decision)
		}
	}
}

func alertRuleNames(t *testing.T) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile("../../alerts.yml")
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Groups []struct {
			Rules []struct {
				Alert string `yaml:"alert"`
			} `yaml:"rules"`
		} `yaml:"groups"`
	}
	if err := yaml.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, group := range file.Groups {
		for _, rule := range group.Rules {
			names[rule.Alert] = true
		}
	}
	return names
}

type observation struct {
	CaseID string `json:"case_id"`
	// Skills are the skills placed before the evidence (EFFECT_EVAL_SKILLS=1).
	Skills            []string              `json:"skills"`
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
		Timeout: time.Second, MaxOutput: 1024,
		Params: []tools.ParamSpec{{Name: "match", Description: "series selector", Required: true}},
		Handler: func(context.Context, json.RawMessage) (string, error) {
			return `{"series":[],"returned":0,"truncated":false}`, nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	// The model may name the restart action in its plan; nothing executes it.
	if err := registry.RegisterAction(sentinelRestart{t}); err != nil {
		t.Fatal(err)
	}
	reasoner := llm.NewReasoner(factory, registry, config.DefaultDiagnoseBudget())
	// The skills arm uses the pipeline's own matching and rendering.
	var skills *knowledge.Skills
	if os.Getenv("EFFECT_EVAL_SKILLS") == "1" {
		var err error
		if skills, err = knowledge.LoadSkills(); err != nil {
			t.Fatal(err)
		}
	}
	consecutiveErrors := 0
	for _, c := range scenarios(t) {
		for trial := 1; trial <= repeats; trial++ {
			if consecutiveErrors >= 3 {
				t.Fatal("stopped after three consecutive model execution errors; inspect connectivity/provider before retrying")
			}
			t.Run(fmt.Sprintf("%s/trial_%d", c.ID, trial), func(t *testing.T) {
				matched := skills.Match(c.Alerts)
				o := observation{CaseID: c.ID, Skills: []string{}, Trial: trial, Model: cfg.Roles.Reasoner.Model,
					ExpectedDiagnosis: c.ExpectedDiagnosis, DiagnosisReview: "pending_human_review",
					CheckFailures: []string{}, StartedAt: time.Now().UTC()}
				for _, skill := range matched {
					o.Skills = append(o.Skills, skill.Name)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
				defer cancel()
				input := knowledge.RenderSkills(matched) + (diagnose.Evidence{IncidentID: 1, Items: c.Evidence}).Render()
				result, err := reasoner.Diagnose(ctx, input, "light", nil)
				o.ElapsedSeconds = time.Since(o.StartedAt).Seconds()
				o.Result = result
				if err != nil {
					consecutiveErrors++
					o.Error = strings.ReplaceAll(tools.Sanitize(err.Error()), cfg.Roles.Reasoner.APIKey, "[REDACTED]")
				} else {
					consecutiveErrors = 0
					o.CheckFailures = check(c, result)
					guard := diagnose.Guard(result.RCA, result.Plan, diagnose.Evidence{IncidentID: 1, Items: c.Evidence})
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
		Plan: llm.Plan{Action: tools.ActionDockerRestart, Target: llm.PlanTarget{Kind: "container", Name: "unrelated-victim"}}}
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

// sentinelRestart is the enabled restart action of offline evaluations: the
// model may plan it, but any attempt to prepare or execute it fails the test.
type sentinelRestart struct{ t *testing.T }

func (sentinelRestart) Definition() tools.ActionDefinition {
	return tools.ActionDefinition{Name: tools.ActionDockerRestart, Version: 2, TargetKind: "container", Description: "Restart the identified container", Timeout: time.Second}
}

func (s sentinelRestart) Prepare(context.Context, tools.PrepareRequest) (tools.Prepared, error) {
	s.t.Fatal("offline evaluation must not prepare a mutation")
	return tools.Prepared{}, nil
}

func (s sentinelRestart) Execute(context.Context, tools.Operation) (tools.Receipt, error) {
	s.t.Fatal("offline evaluation must never execute a mutation")
	return tools.Receipt{}, nil
}

func (s sentinelRestart) Reconcile(context.Context, tools.Operation) (tools.Reconciliation, error) {
	s.t.Fatal("offline evaluation must never reconcile a mutation")
	return tools.Reconciliation{Outcome: tools.OutcomeUnknown}, nil
}
