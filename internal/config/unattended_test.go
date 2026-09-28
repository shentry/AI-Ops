package config

import (
	"strings"
	"testing"

	"oncall-agent/internal/incident"
)

func TestAutomaticRemediationRequiresRecoveryAndEscalation(t *testing.T) {
	ready := func() Config {
		c := defaultConfig()
		c.Service.Probe = ProbeConfig{APIKey: "test", Path: "/v1/messages", Model: "test"}
		c.Web.Operators = []OperatorConfig{{ID: "admin", Role: "admin"}}
		c.Notify.IM.Webhook = "http://notifier.invalid"
		c.Remediation.Rules = []RuleConfig{{ID: "restart", Action: "docker_restart", Mode: incident.ModeAuto}}
		return c
	}
	if err := ready().validateUnattended(); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		mutate func(*Config)
		want   string
	}{
		"no admin":                          {func(c *Config) { c.Web.Operators = nil }, "admin"},
		"no notification":                   {func(c *Config) { c.Notify.IM.Webhook = "" }, "notification"},
		"no business probe":                 {func(c *Config) { c.Service.Probe.APIKey = "" }, "business probe"},
		"single observation":                {func(c *Config) { c.Remediation.Verification.RequiredPasses = 1 }, "three passes"},
		"no watch":                          {func(c *Config) { c.Remediation.Verification.WatchSeconds = 0 }, "watch window"},
		"rollback without sample threshold": {func(c *Config) { c.Remediation.Rules[0].Action = "deployment_rollback" }, "real traffic"},
	} {
		t.Run(name, func(t *testing.T) {
			c := ready()
			tc.mutate(&c)
			if err := c.validateUnattended(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v want %s", err, tc.want)
			}
		})
	}
}
