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

// 时长类配置的上限：换算成 time.Duration 不能溢出 int64 纳秒。
const (
	maxCorrelateWindowMinutes = (1<<63 - 1) / int64(time.Minute)
	maxConfigMinutes          = maxCorrelateWindowMinutes
	maxConfigSeconds          = (1<<63 - 1) / int64(time.Second)
)

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
	Web       WebConfig       `yaml:"web"`
}

type ServerConfig struct {
	Port      int    `yaml:"port"`
	AuthToken string `yaml:"auth_token"`
}

type MySQLConfig struct {
	DSN string `yaml:"dsn"`
}

type LLMConfig struct {
	Roles  LLMRoles       `yaml:"roles"`
	Models []ModelProfile `yaml:"models"`
}

// ModelProfile is one selectable model from the server-owned allowlist. The
// endpoint and API key remain role configuration, never browser input.
type ModelProfile struct {
	ID       string         `yaml:"id"`
	Thinking ThinkingConfig `yaml:"thinking"`
}

type LLMRoles struct {
	Reasoner RoleConfig `yaml:"reasoner"`
}

type RoleConfig struct {
	BaseURL   string         `yaml:"base_url"`
	APIKey    string         `yaml:"api_key"`
	Model     string         `yaml:"model"`
	MaxTokens int            `yaml:"max_tokens"`
	Thinking  ThinkingConfig `yaml:"thinking"`
}

// ThinkingConfig 控制该角色是否向 provider 声明思考模式。
// 未写 thinking 时 Enabled=false，Factory 会显式发送 thinking.type=disabled。
type ThinkingConfig struct {
	Enabled bool   `yaml:"enabled"`
	Effort  string `yaml:"effort"` // 空 | none | low | medium | high
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
	// L2RateWindowMinutes / L2MaxPerWindow 是 L2 自动路径的限频护栏：
	// 同一 target+action（同 plan_hash）在窗口内最多执行 L2MaxPerWindow 次，
	// 超限降级审批 —— 自动路径不许无限重复同一个动作。
	L2RateWindowMinutes int `yaml:"l2_rate_window_minutes"`
	L2MaxPerWindow      int `yaml:"l2_max_per_window"`
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
// 两个限频字段是工具层的兜底护栏：即使上游决策放行，同一容器也不能
// 被高频重启（对应设计"同一 target/action 在时间窗口内限制执行次数"）。
type DockerToolsConfig struct {
	AllowedContainers         []string `yaml:"allowed_containers"`
	RestartMinIntervalSeconds int      `yaml:"restart_min_interval_seconds"`
	RestartMaxPerHour         int      `yaml:"restart_max_per_hour"`
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
	Provider string       `yaml:"provider"`
	Webhook  string       `yaml:"webhook"`
	Feishu   FeishuConfig `yaml:"feishu"`
}

// FeishuConfig contains enterprise-app credentials and callback settings.
// Secret values are supplied through environment expansion, never defaults.
type FeishuConfig struct {
	AppID             string `yaml:"app_id"`
	AppSecret         string `yaml:"app_secret"`
	VerificationToken string `yaml:"verification_token"`
	EncryptKey        string `yaml:"encrypt_key"`
	ChatID            string `yaml:"chat_id"`
	WebBaseURL        string `yaml:"web_base_url"`
}

// WebConfig controls the browser control-room surface. A non-empty base_url
// (or a configured feishu_app provider) enables the public console; the console
// has no login, so there is no session or cookie configuration.
type WebConfig struct {
	BaseURL string `yaml:"base_url"`
	// OperatorAllowlist is the Feishu open_id allowlist for approval cards. It
	// governs the Feishu callback only; the public console is not gated by it.
	OperatorAllowlist []string `yaml:"operator_allowlist"`
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
	normalizeLLMModels(&cfg)
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
			Reasoner: RoleConfig{MaxTokens: 2048},
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
		Approval: ApprovalConfig{
			TTLMinutes: 30, DryRun: true, VerifyDelaySeconds: 30,
			L2RateWindowMinutes: 60, L2MaxPerWindow: 1,
		},
		Tools: ToolsConfig{
			Prometheus: PrometheusConfig{
				BaseURL: "http://127.0.0.1:9090",

				RangeMinutes: 15,
				MaxPoints:    300,
			},
			Logs:   LogsConfig{Provider: "cls"},
			Docker: DockerToolsConfig{RestartMinIntervalSeconds: 60, RestartMaxPerHour: 3},
		},
	}
}

func normalizeLLMModels(cfg *Config) {
	if cfg == nil {
		return
	}
	for index := range cfg.LLM.Models {
		cfg.LLM.Models[index].ID = strings.TrimSpace(cfg.LLM.Models[index].ID)
	}
	if len(cfg.LLM.Models) != 0 {
		return
	}
	defaultModel := strings.TrimSpace(cfg.LLM.Roles.Reasoner.Model)
	if defaultModel == "" {
		return
	}
	cfg.LLM.Models = []ModelProfile{{
		ID:       defaultModel,
		Thinking: cfg.LLM.Roles.Reasoner.Thinking,
	}}
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
// 数值边界一律在这里挡住：0 或负数会在运行时变成"立即超时"、"审批立即过期"、
// "跳过 Verify"这类静默失效行为 —— 那时候没人看得出是配置写错了。
func validate(cfg Config) error {
	if strings.TrimSpace(cfg.MySQL.DSN) == "" {
		return fmt.Errorf("config: mysql.dsn is required; set MYSQL_DSN or mysql.dsn")
	}
	if cfg.Server.Port < 1 || cfg.Server.Port > 65535 {
		return fmt.Errorf("config: server.port must be between 1 and 65535")
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
	if err := validatePositiveMinutes("approval.ttl_minutes", cfg.Approval.TTLMinutes); err != nil {
		return err
	}
	// Verify 延迟允许 0（立即复查，单测和演示用），负数不允许 —— 负延迟会
	// 静默跳过"给系统留出自愈时间"这一步。
	if err := validateNonNegativeSeconds("approval.verify_delay_seconds", cfg.Approval.VerifyDelaySeconds); err != nil {
		return err
	}
	if err := validatePositiveMinutes("approval.l2_rate_window_minutes", cfg.Approval.L2RateWindowMinutes); err != nil {
		return err
	}
	if cfg.Approval.L2MaxPerWindow < 1 {
		return fmt.Errorf("config: approval.l2_max_per_window must be at least 1")
	}
	if err := validatePositiveSeconds("diagnose.evidence.timeout_seconds", cfg.Diagnose.Evidence.TimeoutSeconds); err != nil {
		return err
	}
	if cfg.Diagnose.Evidence.LogMaxLines < 1 {
		return fmt.Errorf("config: diagnose.evidence.log_max_lines must be at least 1")
	}
	if cfg.Diagnose.Budget.FullSteps < 1 {
		return fmt.Errorf("config: diagnose.budget.full_steps must be at least 1")
	}
	if cfg.Diagnose.Budget.LightSteps < 1 {
		return fmt.Errorf("config: diagnose.budget.light_steps must be at least 1")
	}
	if err := validatePositiveSeconds("memory.ttl_seconds", cfg.Memory.TTLSeconds); err != nil {
		return err
	}
	if cfg.Memory.CmdHistoryInject < 0 {
		return fmt.Errorf("config: memory.cmd_history_inject must not be negative")
	}
	if cfg.Tools.Prometheus.RangeMinutes < 1 {
		return fmt.Errorf("config: tools.prometheus.range_minutes must be at least 1")
	}
	if cfg.Tools.Prometheus.MaxPoints < 1 {
		return fmt.Errorf("config: tools.prometheus.max_points must be at least 1")
	}
	if err := validateNonNegativeSeconds("tools.docker.restart_min_interval_seconds", cfg.Tools.Docker.RestartMinIntervalSeconds); err != nil {
		return err
	}
	if cfg.Tools.Docker.RestartMaxPerHour < 1 {
		return fmt.Errorf("config: tools.docker.restart_max_per_hour must be at least 1")
	}
	if cfg.LLM.Roles.Reasoner.MaxTokens < 1 {
		return fmt.Errorf("config: llm.roles.reasoner.max_tokens must be at least 1")
	}
	if err := validateThinking("reasoner", cfg.LLM.Roles.Reasoner.Thinking); err != nil {
		return err
	}
	if err := validateModelProfiles(cfg.LLM); err != nil {
		return err
	}
	provider := strings.ToLower(strings.TrimSpace(cfg.Notify.IM.Provider))
	switch provider {
	case "", "wecom", "feishu":
		// Empty provider and legacy webhook providers remain compatible.
	case "feishu_app":
		f := cfg.Notify.IM.Feishu
		missing := make([]string, 0, 6)
		if strings.TrimSpace(f.AppID) == "" {
			missing = append(missing, "app_id")
		}
		if strings.TrimSpace(f.AppSecret) == "" {
			missing = append(missing, "app_secret")
		}
		if strings.TrimSpace(f.VerificationToken) == "" {
			missing = append(missing, "verification_token")
		}
		if strings.TrimSpace(f.EncryptKey) == "" {
			missing = append(missing, "encrypt_key")
		}
		if strings.TrimSpace(f.ChatID) == "" {
			missing = append(missing, "chat_id")
		}
		if strings.TrimSpace(f.WebBaseURL) == "" {
			missing = append(missing, "web_base_url")
		}
		if len(missing) != 0 {
			return fmt.Errorf("config: notify.im.feishu_app requires %s", strings.Join(missing, ", "))
		}
	default:
		return fmt.Errorf("config: notify.im.provider %q is unsupported", cfg.Notify.IM.Provider)
	}
	return nil
}

func validateThinking(role string, cfg ThinkingConfig) error {
	return validateThinkingField("llm.roles."+role+".thinking", cfg)
}

func validateModelProfiles(cfg LLMConfig) error {
	selected := strings.TrimSpace(cfg.Roles.Reasoner.Model)
	seen := make(map[string]struct{}, len(cfg.Models))
	selectedAllowed := selected == ""
	for index, profile := range cfg.Models {
		id := strings.TrimSpace(profile.ID)
		if id == "" {
			return fmt.Errorf("config: llm.models[%d].id is required", index)
		}
		if len([]rune(id)) > 128 {
			return fmt.Errorf("config: llm.models[%d].id is too long", index)
		}
		if _, exists := seen[id]; exists {
			return fmt.Errorf("config: llm.models contains duplicate id %q", id)
		}
		seen[id] = struct{}{}
		if id == selected {
			selectedAllowed = true
		}
		if err := validateThinkingField(fmt.Sprintf("llm.models[%d].thinking", index), profile.Thinking); err != nil {
			return err
		}
	}
	if !selectedAllowed {
		return fmt.Errorf("config: llm.roles.reasoner.model %q is not in llm.models", selected)
	}
	return nil
}

func validateThinkingField(field string, cfg ThinkingConfig) error {
	effort := strings.ToLower(strings.TrimSpace(cfg.Effort))
	switch effort {
	case "", "none", "low", "medium", "high":
	default:
		return fmt.Errorf("config: %s.effort must be none, low, medium, or high", field)
	}
	if cfg.Enabled && effort == "none" {
		return fmt.Errorf("config: %s.effort cannot be none when thinking is enabled", field)
	}
	if !cfg.Enabled && effort != "" && effort != "none" {
		return fmt.Errorf("config: %s.effort must be none, low, medium, or high", field)
	}
	return nil
}

// validatePositiveMinutes / validatePositiveSeconds / validateNonNegativeSeconds
// 是时长类字段的统一边界：既挡 0 和负数，也挡换算成 Duration 会溢出的巨值。
func validatePositiveMinutes(field string, minutes int) error {
	if minutes < 1 {
		return fmt.Errorf("config: %s must be greater than zero", field)
	}
	if int64(minutes) > maxConfigMinutes {
		return fmt.Errorf("config: %s is too large", field)
	}
	return nil
}

func validatePositiveSeconds(field string, seconds int) error {
	if seconds < 1 {
		return fmt.Errorf("config: %s must be greater than zero", field)
	}
	if int64(seconds) > maxConfigSeconds {
		return fmt.Errorf("config: %s is too large", field)
	}
	return nil
}

func validateNonNegativeSeconds(field string, seconds int) error {
	if seconds < 0 {
		return fmt.Errorf("config: %s must not be negative", field)
	}
	if int64(seconds) > maxConfigSeconds {
		return fmt.Errorf("config: %s is too large", field)
	}
	return nil
}
