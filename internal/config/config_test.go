// D01 配置测试：${ENV} 展开、默认值生效、fail-fast，且错误里不泄漏密码。
package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoadExpandsEnvironmentAndAppliesDefaults(t *testing.T) {
	t.Setenv("MYSQL_DSN", "root:secret@tcp(127.0.0.1:3306)/oncall")
	path := writeConfig(t, "mysql:\n  dsn: ${MYSQL_DSN}\n")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.MySQL.DSN != "root:secret@tcp(127.0.0.1:3306)/oncall" {
		t.Fatalf("DSN = %q", cfg.MySQL.DSN)
	}
	if cfg.Server.Port != 8080 || cfg.Correlate.WindowMinutes != 15 || cfg.Correlate.MinAlerts != 1 || cfg.Diagnose.Budget.FullSteps != 8 {
		t.Fatalf("defaults not applied: server=%d window=%d min_alerts=%d full_steps=%d", cfg.Server.Port, cfg.Correlate.WindowMinutes, cfg.Correlate.MinAlerts, cfg.Diagnose.Budget.FullSteps)
	}
	if cfg.Ingest.SeverityLabel != "severity" {
		t.Fatalf("severity label default = %q", cfg.Ingest.SeverityLabel)
	}
}

func TestLoadRejectsMissingEnvironmentVariable(t *testing.T) {
	t.Setenv("MYSQL_DSN", "")
	path := writeConfig(t, "mysql:\n  dsn: ${MYSQL_DSN}\n")

	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "MYSQL_DSN") {
		t.Fatalf("Load() error = %v, want missing MYSQL_DSN", err)
	}
	if strings.Contains(err.Error(), "root:") {
		t.Fatalf("error leaked a credential: %v", err)
	}
}

func TestLoadRejectsEmptyDSN(t *testing.T) {
	path := writeConfig(t, "mysql:\n  dsn: \"\"\n")

	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "mysql.dsn") {
		t.Fatalf("Load() error = %v, want mysql.dsn validation", err)
	}
}

func TestLoadRejectsInvalidCorrelateWindow(t *testing.T) {
	for _, value := range []string{"0", "-1"} {
		t.Run(value, func(t *testing.T) {
			path := writeConfig(t, "mysql:\n  dsn: test\ncorrelate:\n  window_minutes: "+value+"\n")
			_, err := Load(path)
			if err == nil || !strings.Contains(err.Error(), "correlate.window_minutes") {
				t.Fatalf("Load() error = %v, want correlate.window_minutes validation", err)
			}
		})
	}
}

func TestLoadRejectsInvalidCorrelateMinAlerts(t *testing.T) {
	for _, value := range []string{"0", "-1"} {
		t.Run(value, func(t *testing.T) {
			path := writeConfig(t, "mysql:\n  dsn: test\ncorrelate:\n  min_alerts: "+value+"\n")
			_, err := Load(path)
			if err == nil || !strings.Contains(err.Error(), "correlate.min_alerts") {
				t.Fatalf("Load() error = %v, want correlate.min_alerts validation", err)
			}
		})
	}
}

func TestLoadRejectsCorrelateWindowOverflow(t *testing.T) {
	value := strconv.FormatInt(maxCorrelateWindowMinutes+1, 10)
	path := writeConfig(t, "mysql:\n  dsn: test\ncorrelate:\n  window_minutes: "+value+"\n")
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "correlate.window_minutes is too large") {
		t.Fatalf("Load() error = %v, want correlate window overflow validation", err)
	}
}

// 关键数值字段写 0 或负数必须启动失败。这些字段一旦漏校验，运行时表现是
// "立即超时 / 审批立即过期 / 跳过 Verify / 诊断没有预算"，全部是静默失效。
func TestLoadRejectsInvalidNumericFields(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{"evidence timeout zero", "diagnose:\n  evidence:\n    timeout_seconds: 0\n", "diagnose.evidence.timeout_seconds"},
		{"evidence timeout negative", "diagnose:\n  evidence:\n    timeout_seconds: -3\n", "diagnose.evidence.timeout_seconds"},
		{"log max lines zero", "diagnose:\n  evidence:\n    log_max_lines: 0\n", "diagnose.evidence.log_max_lines"},
		{"full steps zero", "diagnose:\n  budget:\n    full_steps: 0\n", "diagnose.budget.full_steps"},
		{"light steps negative", "diagnose:\n  budget:\n    light_steps: -1\n", "diagnose.budget.light_steps"},
		{"approval ttl zero", "approval:\n  ttl_minutes: 0\n", "approval.ttl_minutes"},
		{"verify delay negative", "approval:\n  verify_delay_seconds: -1\n", "approval.verify_delay_seconds"},
		{"l2 rate window zero", "approval:\n  l2_rate_window_minutes: 0\n", "approval.l2_rate_window_minutes"},
		{"l2 max per window zero", "approval:\n  l2_max_per_window: 0\n", "approval.l2_max_per_window"},
		{"memory ttl zero", "memory:\n  ttl_seconds: 0\n", "memory.ttl_seconds"},
		{"cmd history negative", "memory:\n  cmd_history_inject: -2\n", "memory.cmd_history_inject"},
		{"prom range zero", "tools:\n  prometheus:\n    range_minutes: 0\n", "tools.prometheus.range_minutes"},
		{"prom max points zero", "tools:\n  prometheus:\n    max_points: 0\n", "tools.prometheus.max_points"},
		{"restart interval negative", "tools:\n  docker:\n    restart_min_interval_seconds: -1\n", "restart_min_interval_seconds"},
		{"restart cap zero", "tools:\n  docker:\n    restart_max_per_hour: 0\n", "restart_max_per_hour"},
		{"reasoner tokens zero", "llm:\n  roles:\n    reasoner:\n      max_tokens: 0\n", "llm.roles.reasoner.max_tokens"},
		{"summarizer tokens zero", "llm:\n  roles:\n    summarizer:\n      max_tokens: 0\n", "llm.roles.summarizer.max_tokens"},
		{"thinking effort crazy", "llm:\n  roles:\n    reasoner:\n      thinking:\n        effort: crazy\n", "thinking.effort"},
		{"thinking none when enabled", "llm:\n  roles:\n    reasoner:\n      thinking:\n        enabled: true\n        effort: none\n", "cannot be none"},
		{"port out of range", "server:\n  port: 70000\n", "server.port"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := writeConfig(t, "mysql:\n  dsn: test\n"+test.yaml)
			_, err := Load(path)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Load() error = %v, want %s validation", err, test.want)
			}
		})
	}
}

// 时长字段的溢出上限：换算成 Duration 会溢出的巨值同样拒绝。
func TestLoadRejectsDurationOverflow(t *testing.T) {
	minutes := strconv.FormatInt(maxConfigMinutes+1, 10)
	path := writeConfig(t, "mysql:\n  dsn: test\napproval:\n  ttl_minutes: "+minutes+"\n")
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "approval.ttl_minutes is too large") {
		t.Fatalf("Load() error = %v, want ttl overflow validation", err)
	}
	seconds := strconv.FormatInt(maxConfigSeconds+1, 10)
	path = writeConfig(t, "mysql:\n  dsn: test\napproval:\n  verify_delay_seconds: "+seconds+"\n")
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "approval.verify_delay_seconds is too large") {
		t.Fatalf("Load() error = %v, want verify delay overflow validation", err)
	}
}

// dry_run 的默认值必须是 true：安全约束要求"默认演练"。配置遗漏该字段
// 又开了 auto_execute_l2 时，绝不能因为 Go 零值就直接执行真实变更。
func TestDryRunDefaultsToTrue(t *testing.T) {
	path := writeConfig(t, "mysql:\n  dsn: test\napproval:\n  auto_execute_l2: true\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !cfg.Approval.DryRun {
		t.Fatal("approval.dry_run default = false, want true")
	}
	// 显式关掉仍然生效 —— 默认值不是硬编码。
	path = writeConfig(t, "mysql:\n  dsn: test\napproval:\n  dry_run: false\n")
	cfg, err = Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Approval.DryRun {
		t.Fatal("approval.dry_run = true, want the explicit false to win")
	}
}

// L2 限频与工具层限频的默认值必须存在：配置不写这些字段时，
// 护栏得有保守的兜底值，而不是 0（= 无限制或立即拒绝）。
func TestGuardrailDefaults(t *testing.T) {
	path := writeConfig(t, "mysql:\n  dsn: test\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Approval.L2RateWindowMinutes != 60 || cfg.Approval.L2MaxPerWindow != 1 {
		t.Fatalf("L2 rate defaults = %d/%d, want 60/1", cfg.Approval.L2RateWindowMinutes, cfg.Approval.L2MaxPerWindow)
	}
	if cfg.Tools.Docker.RestartMinIntervalSeconds != 60 || cfg.Tools.Docker.RestartMaxPerHour != 3 {
		t.Fatalf("restart limit defaults = %ds/%d per hour, want 60s/3",
			cfg.Tools.Docker.RestartMinIntervalSeconds, cfg.Tools.Docker.RestartMaxPerHour)
	}
}

func TestLoadAcceptsThinkingEnabledWithoutEffort(t *testing.T) {
	path := writeConfig(t, "mysql:\n  dsn: test\nllm:\n  roles:\n    reasoner:\n      thinking:\n        enabled: true\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !cfg.LLM.Roles.Reasoner.Thinking.Enabled {
		t.Fatal("reasoner thinking.enabled = false, want true")
	}
	if cfg.LLM.Roles.Reasoner.Thinking.Effort != "" {
		t.Fatalf("reasoner thinking.effort = %q, want empty", cfg.LLM.Roles.Reasoner.Thinking.Effort)
	}
}

func TestLoadAllowsAnonymousWebWithoutSessionSecret(t *testing.T) {
	path := writeConfig(t, "mysql:\n  dsn: test\nweb:\n  trusted_operator: oncall\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Web.TrustedOperator != "oncall" {
		t.Fatalf("trusted_operator = %q", cfg.Web.TrustedOperator)
	}
}

func TestLoadDerivesModelAllowlistFromReasoner(t *testing.T) {
	path := writeConfig(t, "mysql:\n  dsn: test\nllm:\n  roles:\n    reasoner:\n      model: glm-5\n      thinking:\n        enabled: false\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(cfg.LLM.Models) != 1 || cfg.LLM.Models[0].ID != "glm-5" || cfg.LLM.Models[0].Thinking.Enabled {
		t.Fatalf("derived models = %+v", cfg.LLM.Models)
	}
}

func TestLoadRejectsInvalidModelAllowlist(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "default missing from allowlist",
			yaml: "llm:\n  roles:\n    reasoner:\n      model: glm-5\n  models:\n    - id: other\n",
			want: "not in llm.models",
		},
		{
			name: "duplicate model",
			yaml: "llm:\n  roles:\n    reasoner:\n      model: glm-5\n  models:\n    - id: glm-5\n    - id: glm-5\n",
			want: "duplicate id",
		},
		{
			name: "invalid profile thinking",
			yaml: "llm:\n  roles:\n    reasoner:\n      model: glm-5\n  models:\n    - id: glm-5\n      thinking:\n        enabled: true\n        effort: none\n",
			want: "cannot be none",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := writeConfig(t, "mysql:\n  dsn: test\n"+test.yaml)
			if _, err := Load(path); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Load() error = %v, want %q", err, test.want)
			}
		})
	}
}
