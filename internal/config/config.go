package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

var (
	envReferencePattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)
	unresolvedPattern   = regexp.MustCompile(`\$\{[^}]+\}`)
)

// Config is the process configuration shared by the service components.
type Config struct {
	Server    ServerConfig    `yaml:"server"`
	MySQL     MySQLConfig     `yaml:"mysql"`
	LLM       LLMConfig       `yaml:"llm"`
	Ingest    IngestConfig    `yaml:"ingest"`
	Correlate CorrelateConfig `yaml:"correlate"`
	Diagnose  DiagnoseConfig  `yaml:"diagnose"`
	Memory    MemoryConfig    `yaml:"memory"`
	Approval  ApprovalConfig  `yaml:"approval"`
	Tools     ToolsConfig     `yaml:"tools"`
	Notify    NotifyConfig    `yaml:"notify"`
}

type ServerConfig struct {
	Port      int    `yaml:"port"`
	AuthToken string `yaml:"auth_token"`
}

type MySQLConfig struct {
	DSN string `yaml:"dsn"`
}

type LLMConfig struct {
	Roles LLMRoles `yaml:"roles"`
}

type LLMRoles struct {
	Reasoner   RoleConfig `yaml:"reasoner"`
	Summarizer RoleConfig `yaml:"summarizer"`
}

type RoleConfig struct {
	BaseURL   string `yaml:"base_url"`
	APIKey    string `yaml:"api_key"`
	Model     string `yaml:"model"`
	MaxTokens int    `yaml:"max_tokens"`
}

type IngestConfig struct {
	FingerprintFields []string `yaml:"fingerprint_fields"`
	SeverityLabel     string   `yaml:"severity_label"`
	FullDedupIgnore   []string `yaml:"full_dedup_ignore"`
}

type CorrelateConfig struct {
	GroupBy       []string `yaml:"group_by"`
	WindowMinutes int      `yaml:"window_minutes"`
	MinAlerts     int      `yaml:"min_alerts"`
}

type DiagnoseConfig struct {
	Budget        DiagnoseBudget    `yaml:"budget"`
	SeverityRoute map[string]string `yaml:"severity_route"`
}

type DiagnoseBudget struct {
	FullSteps  int `yaml:"full_steps"`
	LightSteps int `yaml:"light_steps"`
}

type MemoryConfig struct {
	TTLSeconds         int  `yaml:"ttl_seconds"`
	OnlyHighConfidence bool `yaml:"only_high_confidence"`
	CmdHistoryInject   int  `yaml:"cmd_history_inject"`
}

type ApprovalConfig struct {
	TTLMinutes int `yaml:"ttl_minutes"`
}

type ToolsConfig struct {
	Prometheus PrometheusConfig `yaml:"prometheus"`
	Logs       LogsConfig       `yaml:"logs"`
	MySQLRead  MySQLReadConfig  `yaml:"mysql_select"`
}

type PrometheusConfig struct {
	BaseURL      string `yaml:"base_url"`
	RangeMinutes int    `yaml:"range_minutes"`
	MaxPoints    int    `yaml:"max_points"`
}

type LogsConfig struct {
	Enabled  bool   `yaml:"enabled"`
	Provider string `yaml:"provider"`
	Region   string `yaml:"region"`
	TopicID  string `yaml:"topic_id"`
}

type MySQLReadConfig struct {
	DSN string `yaml:"dsn"`
}

type NotifyConfig struct {
	IM IMConfig `yaml:"im"`
}

type IMConfig struct {
	Provider string `yaml:"provider"`
	Webhook  string `yaml:"webhook"`
}

// Load reads a YAML configuration file, expands ${ENV} references, applies
// non-sensitive defaults, and validates the required D01 settings.
func Load(path string) (Config, error) {
	if strings.TrimSpace(path) == "" {
		return Config{}, fmt.Errorf("config: file path is required")
	}

	contents, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("config: read %q: %w", path, err)
	}

	var document yaml.Node
	if err := yaml.Unmarshal(contents, &document); err != nil {
		return Config{}, fmt.Errorf("config: parse YAML: %w", err)
	}
	if err := expandEnvironment(&document); err != nil {
		return Config{}, err
	}

	cfg := defaultConfig()
	if err := document.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("config: decode YAML: %w", err)
	}
	if err := validate(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func defaultConfig() Config {
	return Config{
		Server: ServerConfig{Port: 8080},
		LLM: LLMConfig{Roles: LLMRoles{
			Reasoner:   RoleConfig{MaxTokens: 2048},
			Summarizer: RoleConfig{MaxTokens: 1024},
		}},
		Ingest: IngestConfig{
			SeverityLabel:   "severity",
			FullDedupIgnore: []string{"starts_at", "ends_at", "received_at"},
		},
		Correlate: CorrelateConfig{
			WindowMinutes: 15,
			MinAlerts:     1,
		},
		Diagnose: DiagnoseConfig{
			Budget: DiagnoseBudget{FullSteps: 8, LightSteps: 3},
			SeverityRoute: map[string]string{
				"critical": "full",
				"high":     "full",
				"warning":  "light",
				"info":     "skip",
				"low":      "skip",
			},
		},
		Memory: MemoryConfig{
			TTLSeconds:         3600,
			OnlyHighConfidence: true,
			CmdHistoryInject:   5,
		},
		Approval: ApprovalConfig{TTLMinutes: 30},
		Tools: ToolsConfig{
			Prometheus: PrometheusConfig{
				BaseURL:      "http://127.0.0.1:9090",
				RangeMinutes: 15,
				MaxPoints:    300,
			},
			Logs: LogsConfig{Provider: "cls"},
		},
	}
}

func expandEnvironment(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode && node.Tag == "!!str" {
		value := node.Value
		missing := ""
		node.Value = envReferencePattern.ReplaceAllStringFunc(value, func(reference string) string {
			name := envReferencePattern.FindStringSubmatch(reference)[1]
			resolved, ok := os.LookupEnv(name)
			if !ok {
				missing = name
				return reference
			}
			return resolved
		})
		if missing != "" {
			return fmt.Errorf("config: environment variable %s is not set", missing)
		}
		if unresolvedPattern.MatchString(node.Value) {
			return fmt.Errorf("config: unresolved environment variable reference")
		}
	}

	for _, child := range node.Content {
		if err := expandEnvironment(child); err != nil {
			return err
		}
	}
	return nil
}

func validate(cfg Config) error {
	if strings.TrimSpace(cfg.MySQL.DSN) == "" {
		return fmt.Errorf("config: mysql.dsn is required; set MYSQL_DSN or mysql.dsn")
	}
	return nil
}
