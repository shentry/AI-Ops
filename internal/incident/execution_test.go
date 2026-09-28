package incident_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"oncall-agent/internal/incident"
)

func testSnapshot() incident.ExecutionContext {
	return incident.ExecutionContext{
		Version: incident.ExecutionContextVersion, Kind: incident.KindPrimary, Service: "sub2api",
		Rule:          incident.RuleRef{ID: "restart", Version: "r1@abc", Mode: incident.ModeManual, Alerts: []string{"Sub2APIDown"}},
		ActionVersion: 1, Target: incident.Object{Kind: "container", Name: "sub2api", ID: "c0ffee"},
		Revision: "started=2026-09-24T01:00:00Z", PreState: json.RawMessage(`{"status":"exited"}`),
		Members: []string{"fp-1"}, FaultAlert: "Sub2APIDown",
		Verification: incident.VerificationSpec{
			Checks:          []incident.Check{{Kind: incident.CheckHealth, Params: json.RawMessage(`{"base_url":"http://127.0.0.1:8080"}`)}},
			IntervalSeconds: 10, WindowSeconds: 120, TimeoutSeconds: 5, RequiredPasses: 1, WatchSeconds: 1800,
		},
		ExpiresAt: time.Date(2026, 9, 24, 2, 0, 0, 0, time.UTC),
	}
}

func encode(t *testing.T, c incident.ExecutionContext) []byte {
	t.Helper()
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestPlanHashCoversEverythingThatDecidedTheAction(t *testing.T) {
	args := []byte(`{"container":"sub2api"}`)
	raw := encode(t, testSnapshot())
	hash, err := incident.PlanHash("docker_restart", args, raw)
	if err != nil {
		t.Fatal(err)
	}
	var reordered map[string]any
	_ = json.Unmarshal(raw, &reordered)
	again, _ := json.Marshal(reordered)
	if other, err := incident.PlanHash("docker_restart", []byte(`{ "container": "sub2api" }`), again); err != nil || other != hash {
		t.Fatalf("equivalent JSON changed hash: %q %v", other, err)
	}
	for name, mutate := range map[string]func(*incident.ExecutionContext){
		"rule mode": func(c *incident.ExecutionContext) { c.Rule.Mode = incident.ModeAuto },
		"rules":     func(c *incident.ExecutionContext) { c.Rule.Version = "r2@def" },
		"identity":  func(c *incident.ExecutionContext) { c.Target.ID = "beef" },
		"revision":  func(c *incident.ExecutionContext) { c.Revision = "started=later" },
		"members":   func(c *incident.ExecutionContext) { c.Members = []string{"fp-2"} },
		"expiry":    func(c *incident.ExecutionContext) { c.ExpiresAt = c.ExpiresAt.Add(time.Minute) },
		"checks":    func(c *incident.ExecutionContext) { c.Verification.RequiredPasses = 2 },
		"evidence":  func(c *incident.ExecutionContext) { c.EvidenceRefs = []string{"docker_inspect"} },
		"compensate": func(c *incident.ExecutionContext) {
			c.Compensation = &incident.Compensation{Action: "undo", ActionVersion: 1, Args: json.RawMessage(`{}`), Revision: "x", Checks: c.Verification.Checks}
		},
	} {
		changed := testSnapshot()
		mutate(&changed)
		other, err := incident.PlanHash("docker_restart", args, encode(t, changed))
		if err != nil || other == hash {
			t.Fatalf("%s: changed snapshot kept hash %q (%v)", name, other, err)
		}
	}
}

func TestSnapshotRejectsIncompleteOrOldContent(t *testing.T) {
	for name, mutate := range map[string]func(*incident.ExecutionContext){
		"old version":        func(c *incident.ExecutionContext) { c.Version = 2 },
		"observe mode":       func(c *incident.ExecutionContext) { c.Rule.Mode = incident.ModeObserve },
		"no rule alerts":     func(c *incident.ExecutionContext) { c.Rule.Alerts = nil },
		"anonymous target":   func(c *incident.ExecutionContext) { c.Target.ID = "" },
		"no revision":        func(c *incident.ExecutionContext) { c.Revision = "" },
		"no checks":          func(c *incident.ExecutionContext) { c.Verification.Checks = nil },
		"unknown check":      func(c *incident.ExecutionContext) { c.Verification.Checks[0].Kind = "shell" },
		"passes beyond":      func(c *incident.ExecutionContext) { c.Verification.RequiredPasses = 13 },
		"no expiry":          func(c *incident.ExecutionContext) { c.ExpiresAt = time.Time{} },
		"fault outside rule": func(c *incident.ExecutionContext) { c.FaultAlert = "Other" },
		"duplicate members":  func(c *incident.ExecutionContext) { c.Members = []string{"fp", "fp"} },
		"compensation nested": func(c *incident.ExecutionContext) {
			c.Kind = incident.KindCompensation
			c.Compensation = &incident.Compensation{}
		},
	} {
		c := testSnapshot()
		mutate(&c)
		if _, err := incident.ParseExecutionContext(encode(t, c)); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	raw := encode(t, testSnapshot())
	for _, bad := range []string{`null`, `{}`, string(raw) + `{}`, strings.Replace(string(raw), `"version":3`, `"version":3,"dry_run":false`, 1)} {
		if _, err := incident.ParseExecutionContext([]byte(bad)); err == nil {
			t.Fatalf("invalid snapshot accepted: %.80s", bad)
		}
	}
	if _, err := incident.PlanHash("docker_restart", []byte(`[]`), raw); err == nil {
		t.Fatal("non-object args accepted")
	}
}

func TestSnapshotIsRevokedByRuleActionOrServiceChanges(t *testing.T) {
	snapshot := testSnapshot()
	binding := incident.ExecutionBinding{Service: "sub2api", RulesVersion: "r1@abc",
		Rules: map[string]incident.RuleRef{"restart": {ID: "restart", Mode: incident.ModeManual}}, Actions: map[string]int{"docker_restart": 1}}
	if err := snapshot.ValidateBinding("docker_restart", binding); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*incident.ExecutionBinding){
		"rules release": func(b *incident.ExecutionBinding) { b.RulesVersion = "r2@def" },
		"rule removed":  func(b *incident.ExecutionBinding) { b.Rules = map[string]incident.RuleRef{} },
		"observe":       func(b *incident.ExecutionBinding) { b.Rules["restart"] = incident.RuleRef{Mode: incident.ModeObserve} },
		"action gone":   func(b *incident.ExecutionBinding) { b.Actions = map[string]int{} },
		"action newer":  func(b *incident.ExecutionBinding) { b.Actions = map[string]int{"docker_restart": 2} },
		"service":       func(b *incident.ExecutionBinding) { b.Service = "other" },
	} {
		changed := binding
		changed.Rules = map[string]incident.RuleRef{"restart": binding.Rules["restart"]}
		mutate(&changed)
		if err := snapshot.ValidateBinding("docker_restart", changed); err == nil {
			t.Fatalf("%s did not revoke the snapshot", name)
		}
	}
	auto := snapshot
	auto.Rule.Mode = incident.ModeAuto
	if err := auto.ValidateBinding("docker_restart", binding); err == nil {
		t.Fatal("auto authority survived demotion to manual")
	}
}

func TestSnapshotRejectsFaultScopeDrift(t *testing.T) {
	snapshot := testSnapshot()
	members := []incident.ExecutionMember{{Fingerprint: "fp-1", Name: "Sub2APIDown", Status: "firing", Service: "sub2api"}}
	if err := snapshot.ValidateMembers(members, true); err != nil {
		t.Fatal(err)
	}
	members[0].Status = "resolved"
	if err := snapshot.ValidateMembers(members, true); err == nil {
		t.Fatal("resolved fault allowed to execute")
	}
	if err := snapshot.ValidateMembers(members, false); err != nil {
		t.Fatal("resolution must not prevent read-only verification")
	}
	members = append(members, incident.ExecutionMember{Fingerprint: "fp-2", Name: "Sub2APISlow", Status: "firing", Service: "sub2api"})
	if err := snapshot.ValidateMembers(members, false); err == nil {
		t.Fatal("alert outside the rule accepted")
	}
	compensation := snapshot
	compensation.Kind = incident.KindCompensation
	if err := compensation.ValidateMembers(members, true); err != nil {
		t.Fatal("compensation undoes our own write regardless of alert scope")
	}
}
