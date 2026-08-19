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
