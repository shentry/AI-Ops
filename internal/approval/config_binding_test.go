package approval

import (
	"context"
	"testing"

	"oncall-agent/internal/config"
	"oncall-agent/internal/incident"
	"oncall-agent/internal/llm"
)

func TestAuthorityRevokesChangedExecutionConfiguration(t *testing.T) {
	remediation := testRemediation(incident.ModeAuto)
	decision := testPolicy(t, newRestartAction(), remediation, &fakeState{}).Decide(context.Background(), llm.Plan{Action: "docker_restart"}, testInput())
	snapshot, err := incident.ParseExecutionContext(decision.ExecutionContext)
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*config.ServiceConfig, *config.RemediationConfig){
		"container":   func(s *config.ServiceConfig, _ *config.RemediationConfig) { s.Container = "replacement-service" },
		"environment": func(s *config.ServiceConfig, _ *config.RemediationConfig) { s.Env = "staging" },
		"origin":      func(s *config.ServiceConfig, _ *config.RemediationConfig) { s.BaseURL = "http://replacement:8080" },
		"probe":       func(s *config.ServiceConfig, _ *config.RemediationConfig) { s.Probe.Model = "different-model" },
		"release entry": func(s *config.ServiceConfig, _ *config.RemediationConfig) {
			s.Release.Command = []string{"/new/deploy"}
		},
		"verification": func(_ *config.ServiceConfig, r *config.RemediationConfig) { r.Verification.RequiredPasses++ },
	} {
		t.Run(name, func(t *testing.T) {
			service, rules := testService, testRemediation(incident.ModeAuto)
			mutate(&service, &rules)
			current, err := NewAuthority(service, rules, testRegistry(t, newRestartAction()))
			if err != nil {
				t.Fatal(err)
			}
			if err := snapshot.ValidateBinding("docker_restart", current.Binding()); err == nil {
				t.Fatal("changed execution configuration retained approval authority")
			}
		})
	}
}
