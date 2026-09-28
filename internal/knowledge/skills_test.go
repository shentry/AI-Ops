package knowledge

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"oncall-agent/internal/config"
	"oncall-agent/internal/tools"
)

func TestSkillsLoad(t *testing.T) {
	skills, err := LoadSkills()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, skill := range skills.List() {
		names = append(names, skill.Name)
		if len(skill.Alerts) == 0 || len(skill.Tools) == 0 || len(skill.SHA256) != 64 {
			t.Fatalf("%s: incomplete skill %+v", skill.Name, skill)
		}
	}
	want := []string{"business_errors_triage", "container_exit_oom", "host_resources", "postgres_connection", "redis_unreachable", "upstream_accounts"}
	if !slices.Equal(names, want) {
		t.Fatalf("skills = %v, want %v", names, want)
	}
}

// Skills are matched on alert names; a name missing from the only rule file
// would silently never match.
func TestSkillAlertsExistInRules(t *testing.T) {
	raw, err := os.ReadFile("../../alerts.yml")
	if err != nil {
		t.Fatal(err)
	}
	var rules struct {
		Groups []struct {
			Rules []struct {
				Alert string `yaml:"alert"`
			} `yaml:"rules"`
		} `yaml:"groups"`
	}
	if err := yaml.Unmarshal(raw, &rules); err != nil {
		t.Fatal(err)
	}
	defined := map[string]bool{}
	for _, group := range rules.Groups {
		for _, rule := range group.Rules {
			defined[rule.Alert] = true
		}
	}
	skills, _ := LoadSkills()
	for _, skill := range skills.List() {
		for _, alert := range skill.Alerts {
			if !defined[alert] {
				t.Errorf("%s: alert %s is not in alerts.yml", skill.Name, alert)
			}
		}
	}
}

// A skill may only name read-only tools. The registry here enables every
// optional tool, so a typo or an action name fails the build, not a diagnosis.
func TestSkillToolsAreReadOnlyTools(t *testing.T) {
	registry := tools.NewRegistry()
	prom, err := tools.NewPrometheusClient(config.PrometheusConfig{BaseURL: "http://127.0.0.1:9090", RangeMinutes: 15, MaxPoints: 300})
	if err != nil {
		t.Fatal(err)
	}
	loki, err := tools.NewLokiClient(config.LokiConfig{BaseURL: "http://127.0.0.1:3100", MaxLines: 200, MaxWindowMinutes: 360})
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(t.TempDir(), "docker.sock")
	if err := os.WriteFile(socket, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	docker, err := tools.NewDockerClient(socket)
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(prom.RegisterTools(registry), loki.RegisterTools(registry), docker.RegisterTools(registry, 200)); err != nil {
		t.Fatal(err)
	}
	registered := map[string]bool{}
	for _, spec := range registry.ForLLM() {
		registered[spec.Name] = true
	}
	skills, _ := LoadSkills()
	for _, skill := range skills.List() {
		for _, name := range skill.Tools {
			if !registered[name] {
				t.Errorf("%s: tool %s is not a registered read-only tool", skill.Name, name)
			}
		}
	}
}

func TestParseSkillRejectsMalformed(t *testing.T) {
	valid := "---\nname: a_skill\ndescription: d\nalerts: [A]\ntools: [t]\n---\nbody\n"
	if _, err := parseSkill([]byte(valid)); err != nil {
		t.Fatalf("valid skill rejected: %v", err)
	}
	for name, raw := range map[string]string{
		"no frontmatter":  "name: a\n",
		"unterminated":    "---\nname: a_skill\ndescription: d\n",
		"unknown field":   "---\nname: a_skill\ndescription: d\nimpl: run.sh\n---\nbody\n",
		"bad name":        "---\nname: A-Skill\ndescription: d\n---\nbody\n",
		"no description":  "---\nname: a_skill\n---\nbody\n",
		"empty body":      "---\nname: a_skill\ndescription: d\n---\n\n",
		"empty tool name": "---\nname: a_skill\ndescription: d\ntools: [\"\"]\n---\nbody\n",
		"oversized body":  "---\nname: a_skill\ndescription: d\n---\n" + strings.Repeat("x", maxSkillBody+1),
	} {
		if _, err := parseSkill([]byte(raw)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestMatchIsDeterministicAndBounded(t *testing.T) {
	skill := func(name string, size int, alerts ...string) Skill {
		return Skill{Name: name, Alerts: alerts, Body: strings.Repeat("x", size)}
	}
	catalog := &Skills{list: []Skill{skill("a", 10, "X"), skill("b", 10, "Y"), skill("c", 10, "X", "Y"), skill("d", 10, "Z")}}
	if got := names(catalog.Match([]string{"Y", "X"})); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("match = %v, want first two by name", got)
	}
	if got := catalog.Match([]string{"unrelated"}); len(got) != 0 {
		t.Fatalf("unrelated alert matched %v", names(got))
	}
	catalog = &Skills{list: []Skill{skill("a", maxInjected-5, "X"), skill("b", 10, "X"), skill("c", 5, "X")}}
	if got := names(catalog.Match([]string{"X"})); !slices.Equal(got, []string{"a", "c"}) {
		t.Fatalf("match = %v, want the size cap to skip b", got)
	}
	var none *Skills
	if none.Match([]string{"X"}) != nil || RenderSkills(nil) != "" {
		t.Fatal("no skills must render nothing")
	}
}

func TestRealAlertsMatchOneSkill(t *testing.T) {
	skills, _ := LoadSkills()
	for alert, want := range map[string]string{
		"Sub2APIDown":                  "container_exit_oom",
		"Sub2APIBusinessErrors":        "business_errors_triage",
		"Sub2APIPostgresUnreachable":   "postgres_connection",
		"Sub2APIRedisUnreachable":      "redis_unreachable",
		"Sub2APIUpstreamAccountErrors": "upstream_accounts",
		"HostDiskAlmostFull":           "host_resources",
	} {
		got := skills.Match([]string{alert})
		if len(got) != 1 || got[0].Name != want {
			t.Errorf("%s matched %v, want %s", alert, names(got), want)
		}
	}
	rendered := RenderSkills(skills.Match([]string{"Sub2APIDown"}))
	if !strings.HasPrefix(rendered, "# 排查技能\n") || !strings.Contains(rendered, "## 技能 container_exit_oom：") {
		t.Fatalf("rendered section:\n%s", rendered)
	}
}

func names(skills []Skill) []string {
	out := []string{}
	for _, skill := range skills {
		out = append(out, skill.Name)
	}
	return out
}
