// Package config 负责进程 YAML 加载和启动前配置校验。
//
// 密钥用 ${ENV} 占位，不写默认值。校验覆盖数据库连接和已启用功能的数值边界。
package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

var (
	// 只展开 ${NAME}。不支持 $NAME，也不支持 ${NAME:-default}。
	envReferencePattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)
	unresolvedPattern   = regexp.MustCompile(`\$\{[^}]+\}`)
)

const maxCorrelateWindowMinutes = (1<<63 - 1) / int64(time.Minute)

// Config 对应 config.example.yaml。运行时校验数据库连接和功能参数边界。
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

// IngestConfig 给 D03 用。D02 的 ParseWebhook 不读配置，
// 仍按默认值：全部 labels、severity 标签名为 "severity"。
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
	Evidence      EvidenceConfig    `yaml:"evidence"`
}

// EvidenceConfig 是 D07 证据采集的数据源配置。地址类字段全部可空：
// 空表示该数据源缺席，对应 collector 记录"缺失"而不是让诊断失败。
type EvidenceConfig struct {
	Sub2APIBaseURL    string `yaml:"sub2api_base_url"`
	Sub2APIMetricsJob string `yaml:"sub2api_metrics_job"`
	PostgresDSN       string `yaml:"postgres_dsn"`
	RedisAddr         string `yaml:"redis_addr"`
	RedisPassword     string `yaml:"redis_password"`
	DockerContainer   string `yaml:"docker_container"`
	DockerSocket      string `yaml:"docker_socket"`
	TimeoutSeconds    int    `yaml:"timeout_seconds"`
	LogMaxLines       int    `yaml:"log_max_lines"`
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
	// AutoExecuteL2 是 L2 自动路径全局开关：默认关，L2 一律降级为审批。
	AutoExecuteL2 bool `yaml:"auto_execute_l2"`
	// DryRun 为真时 L2 只演练不真实执行（执行层检查）。
	DryRun bool `yaml:"dry_run"`
	// VerifyDelaySeconds 是执行后 Verify 的复查延迟。
	VerifyDelaySeconds int `yaml:"verify_delay_seconds"`
}

type ToolsConfig struct {
	Prometheus PrometheusConfig  `yaml:"prometheus"`
	Logs       LogsConfig        `yaml:"logs"`
	MySQLRead  MySQLReadConfig   `yaml:"mysql_select"`
	Docker     DockerToolsConfig `yaml:"docker"`
}

// DockerToolsConfig 是 Docker 变更动作的配置。
// AllowedContainers 是 docker_restart 的目标白名单（GC-11/GC-12）：
// 空列表 = 没有可重启目标，一切重启请求被拒。
type DockerToolsConfig struct {
	AllowedContainers []string `yaml:"allowed_containers"`
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

// Load 读 YAML、展开 ${ENV}、套上非敏感默认值，再校验必填项和数值边界。
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

// defaultConfig 在 YAML 覆盖前填入非敏感默认值。
// DSN、token、webhook 保持空，缺密钥时不能默默跑起来。
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
			Evidence: EvidenceConfig{
				Sub2APIMetricsJob: "sub2api",
				DockerContainer:   "sub2api",
				DockerSocket:      "/var/run/docker.sock",
				TimeoutSeconds:    5,
				LogMaxLines:       200,
			},
		},
		Memory: MemoryConfig{
			TTLSeconds:         3600,
			OnlyHighConfidence: true,
			CmdHistoryInject:   5,
		},
		Approval: ApprovalConfig{TTLMinutes: 30, DryRun: true, VerifyDelaySeconds: 30},
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

// expandEnvironment 只走 YAML 字符串标量，注释和类型值不动。
// 缺 ${ENV} 在这里失败，不会拖到 validate()。
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

// validate 是启动前的 fail-fast 关口。
func validate(cfg Config) error {
	if strings.TrimSpace(cfg.MySQL.DSN) == "" {
		return fmt.Errorf("config: mysql.dsn is required; set MYSQL_DSN or mysql.dsn")
	}
	if cfg.Correlate.WindowMinutes <= 0 {
		return fmt.Errorf("config: correlate.window_minutes must be greater than zero")
	}
	if int64(cfg.Correlate.WindowMinutes) > maxCorrelateWindowMinutes {
		return fmt.Errorf("config: correlate.window_minutes is too large")
	}
	if cfg.Correlate.MinAlerts < 1 {
		return fmt.Errorf("config: correlate.min_alerts must be at least 1")
	}
	return nil
}
