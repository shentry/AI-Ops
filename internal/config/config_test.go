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
	if cfg.Server.Port != 8080 || cfg.Correlate.WindowMinutes != 15 || cfg.Correlate.MinAlerts != 1 || cfg.Diagnose.Budget != DefaultDiagnoseBudget() {
		t.Fatalf("defaults not applied: server=%d window=%d min_alerts=%d full_steps=%d", cfg.Server.Port, cfg.Correlate.WindowMinutes, cfg.Correlate.MinAlerts, cfg.Diagnose.Budget.FullSteps)
	}
	if cfg.Server.ListenAddr != "127.0.0.1" {
		t.Fatalf("listen_addr default = %q, want loopback", cfg.Server.ListenAddr)
	}
	if cfg.Diagnose.Verification != (VerificationConfig{IntervalSeconds: 10, WindowSeconds: 120, TimeoutSeconds: 5}) {
		t.Fatalf("verification defaults = %+v", cfg.Diagnose.Verification)
	}
	if cfg.Ingest.SeverityLabel != "severity" {
		t.Fatalf("severity label default = %q", cfg.Ingest.SeverityLabel)
	}
}

func TestLoadAcceptsExplicitListenAddr(t *testing.T) {
	for _, addr := range []string{"0.0.0.0", "127.0.0.1", "192.0.2.10", "::1", "::"} {
		t.Run(addr, func(t *testing.T) {
			t.Setenv("LISTEN_ADDR", addr)
			cfg, err := Load(writeConfig(t, "mysql:\n  dsn: test\nserver:\n  listen_addr: ${LISTEN_ADDR}\n  port: 18080\n"))
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if cfg.Server.ListenAddr != addr || cfg.Server.Port != 18080 {
				t.Fatalf("server binding = %s:%d", cfg.Server.ListenAddr, cfg.Server.Port)
			}
		})
	}
}

func TestLoadVerificationTimeoutIndependentFromEvidence(t *testing.T) {
	for _, evidenceTimeout := range []string{"1", "60"} {
		for _, verificationTimeout := range []string{"", "7"} {
			t.Run(evidenceTimeout+"/"+verificationTimeout, func(t *testing.T) {
				contents := "mysql:\n  dsn: test\ndiagnose:\n  evidence:\n    timeout_seconds: " + evidenceTimeout + "\n"
				want := 5
				if verificationTimeout != "" {
					contents += "  verification:\n    timeout_seconds: " + verificationTimeout + "\n"
					want = 7
				}
				cfg, err := Load(writeConfig(t, contents))
				if err != nil {
					t.Fatalf("Load() error = %v", err)
				}
				if strconv.Itoa(cfg.Diagnose.Evidence.TimeoutSeconds) != evidenceTimeout {
					t.Fatalf("evidence timeout = %d", cfg.Diagnose.Evidence.TimeoutSeconds)
				}
				if cfg.Diagnose.Verification != (VerificationConfig{IntervalSeconds: 10, WindowSeconds: 120, TimeoutSeconds: want}) {
					t.Fatalf("verification = %+v", cfg.Diagnose.Verification)
				}
			})
		}
	}
}

func TestLoadAcceptsVerificationDurationBoundaries(t *testing.T) {
	for _, test := range []VerificationConfig{
		{TimeoutSeconds: 1, IntervalSeconds: 2, WindowSeconds: 3},
		{TimeoutSeconds: 29, IntervalSeconds: 30, WindowSeconds: 31},
		{TimeoutSeconds: 5, IntervalSeconds: int(maxConfigSeconds - 1), WindowSeconds: int(maxConfigSeconds)},
	} {
		contents := "mysql:\n  dsn: test\ndiagnose:\n  verification:\n    timeout_seconds: " + strconv.Itoa(test.TimeoutSeconds) +
			"\n    interval_seconds: " + strconv.Itoa(test.IntervalSeconds) + "\n    window_seconds: " + strconv.Itoa(test.WindowSeconds) + "\n"
		cfg, err := Load(writeConfig(t, contents))
		if err != nil {
			t.Fatalf("Load(%+v) error = %v", test, err)
		}
		if cfg.Diagnose.Verification != test {
			t.Fatalf("verification = %+v, want %+v", cfg.Diagnose.Verification, test)
		}
	}
}

func TestLoadExampleConfig(t *testing.T) {
	for _, name := range []string{"AUTH_TOKEN", "MYSQL_DSN", "ARK_KEY", "SUB2API_BASE_URL", "PG_RO_DSN", "REDIS_ADDR", "REDIS_PASSWORD", "CLS_TOPIC", "MYSQL_RO_DSN"} {
		t.Setenv(name, "test")
	}
	contents, err := os.ReadFile("../../config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(writeConfig(t, string(contents)))
	if err != nil {
		t.Fatalf("Load(example) error = %v", err)
	}
	if cfg.Server.ListenAddr != "0.0.0.0" || cfg.Server.Port != 18080 || cfg.Web.BaseURL != "http://127.0.0.1:18080" {
		t.Fatal("example must use explicit Compose binding on port 18080")
	}
	if cfg.Diagnose.Verification != (VerificationConfig{IntervalSeconds: 10, WindowSeconds: 120, TimeoutSeconds: 5}) {
		t.Fatalf("example verification = %+v", cfg.Diagnose.Verification)
	}
}

func TestLoadRejectsUnknownAndDeletedKeys(t *testing.T) {
	for _, test := range []struct {
		name string
		yaml string
		key  string
	}{
		{"unknown root", "unexpected: true\n", "unexpected"},
		{"unknown server key", "server:\n  listen_address: 127.0.0.1\n", "listen_address"},
		{"unknown nested key", "diagnose:\n  evidence:\n    timeuot_seconds: 5\n", "timeuot_seconds"},
		{"unknown verification key", "diagnose:\n  verification:\n    delay_seconds: 5\n", "delay_seconds"},
		{"deleted delay", "approval:\n  verify_delay_seconds: 30\n", "verify_delay_seconds"},
		{"deleted zero delay", "approval:\n  verify_delay_seconds: 0\n", "verify_delay_seconds"},
		{"deleted null delay", "approval:\n  verify_delay_seconds: null\n", "verify_delay_seconds"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, "mysql:\n  dsn: test\n"+test.yaml))
			if err == nil || !strings.Contains(err.Error(), "field "+test.key+" not found") {
				t.Fatalf("Load() error = %v, want unknown field %s", err, test.key)
			}
		})
	}
}

func TestLoadRejectsExtraYAMLDocuments(t *testing.T) {
	for _, extra := range []string{"---\nserver:\n  port: 18080\n", "---\n", "---\nnull\n", "---\n[invalid\n"} {
		t.Run(extra, func(t *testing.T) {
			if _, err := Load(writeConfig(t, "mysql:\n  dsn: test\n"+extra)); err == nil {
				t.Fatal("Load() accepted an extra YAML document")
			}
		})
	}
}

func TestLoadRejectsInvalidListenAddr(t *testing.T) {
	t.Setenv("EMPTY_LISTEN_ADDR", "")
	for _, value := range []string{
		`""`, `" "`, "", "null", "~", "localhost", "127.0.0.1:18080",
		"999.0.0.1", "127.0.0.1/8", `"[::1]"`, `" 127.0.0.1 "`, `"${EMPTY_LISTEN_ADDR}"`,
	} {
		t.Run(value, func(t *testing.T) {
			_, err := Load(writeConfig(t, "mysql:\n  dsn: test\nserver:\n  listen_addr: "+value+"\n"))
			if err == nil || !strings.Contains(err.Error(), "server.listen_addr") {
				t.Fatalf("Load() error = %v, want invalid server.listen_addr", err)
			}
		})
	}
}

func TestLoadRejectsInvalidVerification(t *testing.T) {
	overflow := strconv.FormatInt(maxConfigSeconds+1, 10)
	for _, test := range []struct {
		name string
		yaml string
		want string
	}{
		{"zero timeout", "timeout_seconds: 0", "timeout_seconds"},
		{"negative timeout", "timeout_seconds: -1", "timeout_seconds"},
		{"zero interval", "interval_seconds: 0", "interval_seconds"},
		{"negative interval", "interval_seconds: -1", "interval_seconds"},
		{"zero window", "window_seconds: 0", "window_seconds"},
		{"negative window", "window_seconds: -1", "window_seconds"},
		{"timeout equals interval", "timeout_seconds: 10", "timeout_seconds must be less than"},
		{"timeout exceeds interval", "timeout_seconds: 11", "timeout_seconds must be less than"},
		{"interval equals window", "interval_seconds: 120", "interval_seconds must be less than"},
		{"interval exceeds window", "interval_seconds: 121", "interval_seconds must be less than"},
		{"timeout equals claim limit", "timeout_seconds: 30\n    interval_seconds: 31", "timeout_seconds must be less than 30"},
		{"timeout exceeds claim limit", "timeout_seconds: 31\n    interval_seconds: 32", "timeout_seconds must be less than 30"},
		{"timeout overflow", "timeout_seconds: " + overflow, "timeout_seconds is too large"},
		{"interval overflow", "interval_seconds: " + overflow, "interval_seconds is too large"},
		{"window overflow", "window_seconds: " + overflow, "window_seconds is too large"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, "mysql:\n  dsn: test\ndiagnose:\n  verification:\n    "+test.yaml+"\n"))
			if err == nil || !strings.Contains(err.Error(), "diagnose.verification."+test.want) {
				t.Fatalf("Load() error = %v, want diagnose.verification.%s", err, test.want)
			}
		})
	}
}

func TestLoadPreservesEnvironmentScalarValues(t *testing.T) {
	value := "root:secret # still a value\nserver:\n  port: 1"
	t.Setenv("MYSQL_DSN", value)
	cfg, err := Load(writeConfig(t, "# ${UNSET_COMMENT_IS_IGNORED}\nmysql:\n  dsn: ${MYSQL_DSN}\n"))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.MySQL.DSN != value || cfg.Server.Port != 8080 {
		t.Fatal("environment expansion changed the YAML structure or scalar value")
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
// "立即超时 / 审批立即过期 / 验证窗口失效 / 诊断没有预算"，全部是静默失效。
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
		{"l2 rate window zero", "approval:\n  l2_rate_window_minutes: 0\n", "approval.l2_rate_window_minutes"},
		{"l2 max per window zero", "approval:\n  l2_max_per_window: 0\n", "approval.l2_max_per_window"},
		{"memory ttl zero", "memory:\n  ttl_seconds: 0\n", "memory.ttl_seconds"},
		{"cmd history negative", "memory:\n  cmd_history_inject: -2\n", "memory.cmd_history_inject"},
		{"prom range zero", "tools:\n  prometheus:\n    range_minutes: 0\n", "tools.prometheus.range_minutes"},
		{"prom max points zero", "tools:\n  prometheus:\n    max_points: 0\n", "tools.prometheus.max_points"},
		{"restart interval negative", "tools:\n  docker:\n    restart_min_interval_seconds: -1\n", "restart_min_interval_seconds"},
		{"restart cap zero", "tools:\n  docker:\n    restart_max_per_hour: 0\n", "restart_max_per_hour"},
		{"reasoner tokens zero", "llm:\n  roles:\n    reasoner:\n      max_tokens: 0\n", "llm.roles.reasoner.max_tokens"},
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
	path = writeConfig(t, "mysql:\n  dsn: test\ndiagnose:\n  evidence:\n    timeout_seconds: "+seconds+"\n")
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "diagnose.evidence.timeout_seconds is too large") {
		t.Fatalf("Load() error = %v, want evidence timeout overflow validation", err)
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

func TestLoadEnablesAnonymousConsoleFromBaseURLAlone(t *testing.T) {
	path := writeConfig(t, "mysql:\n  dsn: test\nweb:\n  base_url: http://127.0.0.1:8080\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Web.BaseURL != "http://127.0.0.1:8080" {
		t.Fatalf("base_url = %q", cfg.Web.BaseURL)
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

func TestModelContextWindowConfiguration(t *testing.T) {
	for _, tc := range []struct {
		value     int
		wantError bool
	}{{0, false}, {-1, true}, {2048, true}, {2500, true}, {1000000, false}} {
		path := writeConfig(t, "mysql:\n  dsn: test\nllm:\n  roles:\n    reasoner:\n      model: deepseek-v4-flash\n  models:\n    - id: deepseek-v4-flash\n      context_window_tokens: "+strconv.Itoa(tc.value)+"\n")
		cfg, err := Load(path)
		if tc.wantError {
			if err == nil || !strings.Contains(err.Error(), "context_window_tokens") {
				t.Fatalf("value=%d err=%v", tc.value, err)
			}
		} else {
			want := tc.value
			if want == 0 {
				want = DefaultContextWindowTokens
			}
			if err != nil || cfg.LLM.Models[0].ContextWindow() != want {
				t.Fatalf("value=%d cfg=%+v err=%v", tc.value, cfg.LLM.Models, err)
			}
		}
	}
}

func TestRejectsRetiredGlobalContextBudget(t *testing.T) {
	_, err := Load(writeConfig(t, "mysql:\n  dsn: test\ndiagnose:\n  budget:\n    context_tokens: 32768\n"))
	if err == nil || !strings.Contains(err.Error(), "context_tokens") {
		t.Fatalf("retired setting accepted: %v", err)
	}
}
