package config

import (
	"os"
	"path/filepath"
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
	if cfg.Server.Port != 8080 || cfg.Correlate.WindowMinutes != 15 || cfg.Diagnose.Budget.FullSteps != 8 {
		t.Fatalf("defaults not applied: server=%d window=%d full_steps=%d", cfg.Server.Port, cfg.Correlate.WindowMinutes, cfg.Diagnose.Budget.FullSteps)
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
