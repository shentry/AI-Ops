// Package config 负责进程 YAML 加载和启动前配置校验。
//
// 密钥用 ${ENV} 占位，不写默认值。校验覆盖数据库连接和已启用功能的数值边界。
package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"oncall-agent/internal/incident"
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
	// Service is the one trusted description of the monitored service: its
	// identity, targets, health, admin API and release entry.
	Service ServiceConfig `yaml:"service"`
	// Remediation holds the versioned action rules and their verification.
	Remediation RemediationConfig `yaml:"remediation"`
}

// ServiceConfig is the only place that names the monitored service. Evidence,
// actions and verification all read targets from here.
type ServiceConfig struct {
	Name      string `yaml:"name"`
	Env       string `yaml:"env"`
	Container string `yaml:"container"`
	// BaseURL is the service origin: health checks, admin API and probes.
	BaseURL string `yaml:"base_url"`
	// AdminAPIKey has full sub2api admin authority; callers are restricted to
	// their own endpoint allowlists. Empty disables upstream evidence and actions.
	AdminAPIKey   string        `yaml:"admin_api_key"`
	PostgresDSN   string        `yaml:"postgres_dsn"`
	RedisAddr     string        `yaml:"redis_addr"`
	RedisPassword string        `yaml:"redis_password"`
	Probe         ProbeConfig   `yaml:"probe"`
	Release       ReleaseConfig `yaml:"release"`
}

// ProbeConfig is the optional business probe: a dedicated low-cost test key
// and model. Each call reaches an upstream model and costs money.
type ProbeConfig struct {
	Path   string `yaml:"path"`
	Model  string `yaml:"model"`
	APIKey string `yaml:"api_key"`
}

// ReleaseConfig is the deployment entry used by deployment_rollback: a fixed
// executable run with an argument array (the approved image reference is
// appended), never a shell string. It must honor the same lock file as CI/CD.
type ReleaseConfig struct {
	Command        []string `yaml:"command"`
	WorkDir        string   `yaml:"work_dir"`
	LockFile       string   `yaml:"lock_file"`
	TimeoutSeconds int      `yaml:"timeout_seconds"`
}

// RemediationConfig is the pre-authorization of automatic handling. It is a
// version-managed configuration release; there is no second rule store.
type RemediationConfig struct {
	RulesVersion string              `yaml:"rules_version"`
	Rules        []RuleConfig        `yaml:"rules"`
	Maintenance  []MaintenanceWindow `yaml:"maintenance"`
	Verification VerificationConfig  `yaml:"verification"`
}

// RuleConfig authorizes one action for a fault condition within bounds.
// Mode observe records what would be done; manual needs a person to approve
// the frozen snapshot; auto executes when every fact, scope and budget holds.
type RuleConfig struct {
	ID     string   `yaml:"id"`
	Action string   `yaml:"action"`
	Mode   string   `yaml:"mode"`
	Alerts []string `yaml:"alerts"`
	// MaxExecutions real executions per WindowMinutes.
	MaxExecutions int `yaml:"max_executions"`
	WindowMinutes int `yaml:"window_minutes"`
	// MinAvailableAccounts is the floor of schedulable accounts left in each
	// affected group after upstream_quarantine.
	MinAvailableAccounts int `yaml:"min_available_accounts"`
	// MaxErrorRatio and MinRequests define business recovery for actions
	// verified on real traffic; too few requests is not a pass.
	MaxErrorRatio float64 `yaml:"max_error_ratio"`
	MinRequests   int     `yaml:"min_requests"`
	// Compensate pre-authorizes the action's frozen undo when verification fails.
	Compensate bool `yaml:"compensate"`
	// VerifyWindowSeconds overrides the verification window for this rule.
	// Traffic checks read a trailing five-minute window, so actions verified on
	// real requests need a longer window than a health probe.
	VerifyWindowSeconds int `yaml:"verify_window_seconds"`
}

type MaintenanceWindow struct {
	Start  time.Time `yaml:"start"`
	End    time.Time `yaml:"end"`
	Reason string    `yaml:"reason"`
}

// Release binds permissions to the rules, service identity and verification.
// Credentials are hashed with the configuration and are never returned or audited.
func (r RemediationConfig) Release(service ServiceConfig) string {
	content, _ := json.Marshal(struct {
		Rules        []RuleConfig
		Service      ServiceConfig
		Verification VerificationConfig
	}{r.Rules, service, r.Verification})
	sum := sha256.Sum256(content)
	return r.RulesVersion + "@" + hex.EncodeToString(sum[:])[:12]
}

// InMaintenance returns the reason of the window covering now, if any.
func (r RemediationConfig) InMaintenance(now time.Time) (string, bool) {
	for _, window := range r.Maintenance {
		if !now.Before(window.Start) && now.Before(window.End) {
			return window.Reason, true
		}
	}
	return "", false
}

type ServerConfig struct {
	ListenAddr string `yaml:"listen_addr"`
	Port       int    `yaml:"port"`
	AuthToken  string `yaml:"auth_token"`
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
	ID                  string         `yaml:"id"`
	Thinking            ThinkingConfig `yaml:"thinking"`
	ContextWindowTokens int            `yaml:"context_window_tokens"`
}

// DefaultContextWindowTokens is an application fallback for unspecified models,
// not a claim about any provider's actual capacity. Set each profile explicitly.
const DefaultContextWindowTokens = 131072

func (p ModelProfile) ContextWindow() int {
	if p.ContextWindowTokens == 0 {
		return DefaultContextWindowTokens
	}
	return p.ContextWindowTokens
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

// VerificationConfig 控制独立恢复验证的调度，不复用证据采集超时。
// RequiredPasses 是判定恢复所需的连续健康观测次数；WatchSeconds 是恢复后
// 识别复发的观察窗口。写入审批快照后对该次处置不可改。
type VerificationConfig struct {
	IntervalSeconds int `yaml:"interval_seconds"`
	WindowSeconds   int `yaml:"window_seconds"`
	TimeoutSeconds  int `yaml:"timeout_seconds"`
	RequiredPasses  int `yaml:"required_passes"`
	WatchSeconds    int `yaml:"watch_seconds"`
}

// EvidenceConfig 是证据采集自身的参数；采集目标来自 service。
type EvidenceConfig struct {
	DockerSocket   string `yaml:"docker_socket"`
	TimeoutSeconds int    `yaml:"timeout_seconds"`
	LogMaxLines    int    `yaml:"log_max_lines"`
}

type DiagnoseBudget struct {
	FullSteps  int `yaml:"full_steps"`
	LightSteps int `yaml:"light_steps"`
}

func DefaultDiagnoseBudget() DiagnoseBudget {
	return DiagnoseBudget{FullSteps: 32, LightSteps: 16}
}

type MemoryConfig struct {
	TTLSeconds         int  `yaml:"ttl_seconds"`
	OnlyHighConfidence bool `yaml:"only_high_confidence"`
	CmdHistoryInject   int  `yaml:"cmd_history_inject"`
}

// ApprovalConfig 只管人工审批单的有效期；是否需要审批由 remediation 规则决定。
type ApprovalConfig struct {
	TTLMinutes int `yaml:"ttl_minutes"`
}

type ToolsConfig struct {
	Prometheus PrometheusConfig `yaml:"prometheus"`
	Loki       LokiConfig       `yaml:"loki"`
}

type PrometheusConfig struct {
	BaseURL      string `yaml:"base_url"`
	RangeMinutes int    `yaml:"range_minutes"`
	MaxPoints    int    `yaml:"max_points"`
}

// LokiConfig is the optional historical log search. An empty base_url leaves
// loki_query unregistered; diagnosis then relies on docker_logs alone.
type LokiConfig struct {
	BaseURL          string `yaml:"base_url"`
	MaxLines         int    `yaml:"max_lines"`
	MaxWindowMinutes int    `yaml:"max_window_minutes"`
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
// (or a configured feishu_app provider) enables the console, which then
// requires at least one configured operator: there is no anonymous console.
type WebConfig struct {
	BaseURL string `yaml:"base_url"`
	// OperatorAllowlist is the Feishu open_id allowlist for approval cards. It
	// governs the Feishu callback only; console access is governed by Operators.
	OperatorAllowlist []string `yaml:"operator_allowlist"`
	// Operators are the people (or service identities) allowed to use the
	// console and operator APIs. Only the SHA-256 of each personal token is stored.
	Operators []OperatorConfig `yaml:"operators"`
}

// OperatorConfig is one server-side identity. Role is viewer, operator or admin;
// token_sha256 is the lowercase hex SHA-256 of a high-entropy personal token.
type OperatorConfig struct {
	ID          string `yaml:"id"`
	Name        string `yaml:"name"`
	Role        string `yaml:"role"`
	TokenSHA256 string `yaml:"token_sha256"`
}

// ConsoleEnabled reports whether the browser console is served.
func (c Config) ConsoleEnabled() bool {
	return strings.TrimSpace(c.Web.BaseURL) != "" || strings.EqualFold(strings.TrimSpace(c.Notify.IM.Provider), "feishu_app")
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
	parser := yaml.NewDecoder(bytes.NewReader(contents))
	if err := parser.Decode(&document); err != nil {
		return Config{}, fmt.Errorf("config: parse YAML: %w", err)
	}
	var extra yaml.Node
	if err := parser.Decode(&extra); err != io.EOF {
		if err != nil {
			return Config{}, fmt.Errorf("config: parse YAML: %w", err)
		}
		return Config{}, fmt.Errorf("config: only one YAML document is allowed")
	}
	if err := expandEnvironment(&document); err != nil {
		return Config{}, err
	}
	if err := rejectNullListenAddr(&document); err != nil {
		return Config{}, err
	}
	encoded, err := yaml.Marshal(&document)
	if err != nil {
		return Config{}, fmt.Errorf("config: encode expanded YAML: %w", err)
	}
	cfg := defaultConfig()
	decoder := yaml.NewDecoder(bytes.NewReader(encoded))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
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
		Server: ServerConfig{ListenAddr: "127.0.0.1", Port: 8080},
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
			Budget: DefaultDiagnoseBudget(),
			SeverityRoute: map[string]string{
				"critical": "full",
				"high":     "full",
				"warning":  "light",
				"info":     "skip",
				"low":      "skip",
			},
			Evidence: EvidenceConfig{
				DockerSocket:   "/var/run/docker.sock",
				TimeoutSeconds: 5,
				LogMaxLines:    200,
			},
		},
		Memory: MemoryConfig{
			TTLSeconds:         3600,
			OnlyHighConfidence: true,
			CmdHistoryInject:   5,
		},
		Approval: ApprovalConfig{TTLMinutes: 30},
		Service:  ServiceConfig{Name: "sub2api", Env: "prod", Container: "sub2api", Release: ReleaseConfig{TimeoutSeconds: 300}},
		Remediation: RemediationConfig{
			Verification: VerificationConfig{IntervalSeconds: 10, WindowSeconds: 300, TimeoutSeconds: 5, RequiredPasses: 3, WatchSeconds: 1800},
		},
		Tools: ToolsConfig{
			Prometheus: PrometheusConfig{
				BaseURL: "http://127.0.0.1:9090",

				RangeMinutes: 15,
				MaxPoints:    300,
			},
			Loki: LokiConfig{MaxLines: 200, MaxWindowMinutes: 360},
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

// YAML null 不会覆盖 string 默认值，必须在类型解码前拒绝显式空监听地址。
func rejectNullListenAddr(node *yaml.Node) error {
	if node.Kind == yaml.MappingNode {
		for i := 0; i < len(node.Content); i += 2 {
			if node.Content[i].Value == "listen_addr" && node.Content[i+1].ShortTag() == "!!null" {
				return fmt.Errorf("config: server.listen_addr must be a valid IP address")
			}
		}
	}
	for _, child := range node.Content {
		if err := rejectNullListenAddr(child); err != nil {
			return err
		}
	}
	return nil
}

// validate 是启动前的 fail-fast 关口。
// 数值边界一律在这里挡住：0 或负数会在运行时变成"立即超时"、"审批立即过期"、
// "验证窗口失效"这类静默失效行为 —— 那时候没人看得出是配置写错了。
func validate(cfg Config) error {
	if strings.TrimSpace(cfg.MySQL.DSN) == "" {
		return fmt.Errorf("config: mysql.dsn is required; set MYSQL_DSN or mysql.dsn")
	}
	if net.ParseIP(cfg.Server.ListenAddr) == nil {
		return fmt.Errorf("config: server.listen_addr must be a valid IP address")
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
	if err := validatePositiveSeconds("diagnose.evidence.timeout_seconds", cfg.Diagnose.Evidence.TimeoutSeconds); err != nil {
		return err
	}
	if err := validateVerification(cfg.Remediation.Verification); err != nil {
		return err
	}
	if err := validateService(cfg.Service); err != nil {
		return err
	}
	if err := validateRemediation(cfg.Remediation, cfg.Service); err != nil {
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
	if cfg.Tools.Loki.MaxLines < 1 {
		return fmt.Errorf("config: tools.loki.max_lines must be at least 1")
	}
	if err := validatePositiveMinutes("tools.loki.max_window_minutes", cfg.Tools.Loki.MaxWindowMinutes); err != nil {
		return err
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
	if err := validateOperators(cfg); err != nil {
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
	if cfg.AutomaticRemediation() {
		if err := cfg.validateUnattended(); err != nil {
			return err
		}
	}
	return nil
}

// AutomaticRemediation reports whether this configuration authorizes unattended writes.
func (c Config) AutomaticRemediation() bool {
	for _, rule := range c.Remediation.Rules {
		if rule.Mode == incident.ModeAuto {
			return true
		}
	}
	return false
}

func (c Config) validateUnattended() error {
	admin := false
	for _, operator := range c.Web.Operators {
		admin = admin || operator.Role == "admin"
	}
	if !admin {
		return fmt.Errorf("config: auto remediation requires an admin identity for stop and recovery")
	}
	if !strings.EqualFold(c.Notify.IM.Provider, "feishu_app") && strings.TrimSpace(c.Notify.IM.Webhook) == "" {
		return fmt.Errorf("config: auto remediation requires a notification provider")
	}
	for _, rule := range c.Remediation.Rules {
		if rule.Mode != incident.ModeAuto {
			continue
		}
		v := c.Remediation.VerificationFor(rule)
		if v.RequiredPasses < 3 || v.WatchSeconds <= v.IntervalSeconds*v.RequiredPasses {
			return fmt.Errorf("config: auto rule %s requires at least three passes and a watch window", rule.ID)
		}
		if rule.Action == "docker_restart" && strings.TrimSpace(c.Service.Probe.APIKey) == "" {
			return fmt.Errorf("config: auto restart requires a business probe; health alone cannot prove recovery")
		}
		if rule.Action == "deployment_rollback" || rule.Action == "upstream_quarantine" {
			if rule.MinRequests < 1 || v.WindowSeconds <= 300 {
				return fmt.Errorf("config: auto rule %s requires real traffic samples and a window longer than five minutes", rule.ID)
			}
		}
	}
	return nil
}

func validateVerification(v VerificationConfig) error {
	const field = "remediation.verification"
	for name, seconds := range map[string]int{"timeout_seconds": v.TimeoutSeconds, "interval_seconds": v.IntervalSeconds, "window_seconds": v.WindowSeconds} {
		if err := validatePositiveSeconds(field+"."+name, seconds); err != nil {
			return err
		}
	}
	if v.TimeoutSeconds >= 30 {
		return fmt.Errorf("config: %s.timeout_seconds must be less than 30 (verification claim limit)", field)
	}
	if v.TimeoutSeconds >= v.IntervalSeconds {
		return fmt.Errorf("config: %s.timeout_seconds must be less than interval_seconds", field)
	}
	if v.IntervalSeconds >= v.WindowSeconds {
		return fmt.Errorf("config: %s.interval_seconds must be less than window_seconds", field)
	}
	if err := incident.ValidatePassWindow(v.RequiredPasses, v.IntervalSeconds, v.WindowSeconds); err != nil {
		return fmt.Errorf("config: %s.required_passes must be 1..100 and (required_passes-1)*interval_seconds must be less than window_seconds", field)
	}
	if err := validateNonNegativeSeconds(field+".watch_seconds", v.WatchSeconds); err != nil {
		return err
	}
	if v.WatchSeconds > 0 && v.WatchSeconds <= v.IntervalSeconds {
		return fmt.Errorf("config: %s.watch_seconds must be 0 or longer than interval_seconds", field)
	}
	return nil
}

var serviceNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,63}$`)

func validateService(s ServiceConfig) error {
	if !serviceNamePattern.MatchString(s.Name) || !serviceNamePattern.MatchString(s.Container) || !serviceNamePattern.MatchString(s.Env) {
		return fmt.Errorf("config: service.name, service.env and service.container must be 1-64 lowercase letters, digits and ._-")
	}
	if strings.TrimSpace(s.BaseURL) != "" {
		if _, err := incident.NormalizeHealthBaseURL(s.BaseURL); err != nil {
			return fmt.Errorf("config: service.base_url must be an HTTP(S) origin without credentials, path or query")
		}
	}
	if p := s.Probe; strings.TrimSpace(p.APIKey) != "" && (!strings.HasPrefix(p.Path, "/") || strings.TrimSpace(p.Model) == "") {
		return fmt.Errorf("config: service.probe requires path (starting with /) and model when api_key is set")
	}
	if r := s.Release; len(r.Command) > 0 {
		if !strings.HasPrefix(r.Command[0], "/") || !strings.HasPrefix(r.WorkDir, "/") || !strings.HasPrefix(r.LockFile, "/") {
			return fmt.Errorf("config: service.release requires an absolute command, work_dir and lock_file")
		}
		if err := validatePositiveSeconds("service.release.timeout_seconds", r.TimeoutSeconds); err != nil {
			return err
		}
	}
	return nil
}

var (
	ruleIDPattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	validRuleModes = map[string]bool{incident.ModeObserve: true, incident.ModeManual: true, incident.ModeAuto: true}
)

// VerificationFor returns the verification timing a rule freezes into snapshots.
func (r RemediationConfig) VerificationFor(rule RuleConfig) VerificationConfig {
	v := r.Verification
	if rule.VerifyWindowSeconds > 0 {
		v.WindowSeconds = rule.VerifyWindowSeconds
	}
	return v
}

// validateRemediation checks rule shape only; whether each action is available
// (for example release needs service.release) is checked against the action
// catalog at startup.
func validateRemediation(r RemediationConfig, service ServiceConfig) error {
	if len(r.Rules) > 0 && strings.TrimSpace(r.RulesVersion) == "" {
		return fmt.Errorf("config: remediation.rules_version is required when rules are configured")
	}
	if len(r.Rules) > 0 && strings.TrimSpace(service.BaseURL) == "" {
		return fmt.Errorf("config: service.base_url is required when remediation rules are configured")
	}
	ids := map[string]bool{}
	for index, rule := range r.Rules {
		field := fmt.Sprintf("remediation.rules[%d]", index)
		if !ruleIDPattern.MatchString(rule.ID) || ids[rule.ID] {
			return fmt.Errorf("config: %s.id must be unique, 1-64 lowercase letters, digits, _ or -", field)
		}
		ids[rule.ID] = true
		if strings.TrimSpace(rule.Action) == "" || !validRuleModes[rule.Mode] {
			return fmt.Errorf("config: %s needs an action and mode observe, manual or auto", field)
		}
		if len(rule.Alerts) == 0 {
			return fmt.Errorf("config: %s.alerts must name the alerts this rule covers", field)
		}
		for _, alert := range rule.Alerts {
			if strings.TrimSpace(alert) == "" {
				return fmt.Errorf("config: %s.alerts must not contain empty names", field)
			}
		}
		if rule.MaxExecutions < 1 {
			return fmt.Errorf("config: %s.max_executions must be at least 1", field)
		}
		if err := validatePositiveMinutes(field+".window_minutes", rule.WindowMinutes); err != nil {
			return err
		}
		if rule.MinAvailableAccounts < 0 || rule.MinRequests < 0 || rule.MaxErrorRatio < 0 || rule.MaxErrorRatio >= 1 {
			return fmt.Errorf("config: %s bounds must be non-negative and max_error_ratio below 1", field)
		}
		if rule.VerifyWindowSeconds != 0 {
			window := r.Verification
			window.WindowSeconds = rule.VerifyWindowSeconds
			if err := validateVerification(window); err != nil {
				return fmt.Errorf("config: %s.verify_window_seconds: %w", field, err)
			}
		}
	}
	for index, window := range r.Maintenance {
		if window.Start.IsZero() || !window.End.After(window.Start) {
			return fmt.Errorf("config: remediation.maintenance[%d] needs start before end", index)
		}
	}
	return nil
}

var (
	operatorIDPattern  = regexp.MustCompile(`^[A-Za-z0-9._@-]{1,64}$`)
	sha256HexPattern   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	validOperatorRoles = map[string]bool{"viewer": true, "operator": true, "admin": true}
)

// validateOperators rejects an anonymous console and any identity that could be
// confused with another or with the shared machine token.
func validateOperators(cfg Config) error {
	if cfg.ConsoleEnabled() && len(cfg.Web.Operators) == 0 {
		return fmt.Errorf("config: web.operators is required when the console is enabled; the console has no anonymous access")
	}
	machine := sha256.Sum256([]byte(cfg.Server.AuthToken))
	ids, hashes := map[string]bool{}, map[string]bool{}
	for index, operator := range cfg.Web.Operators {
		field := fmt.Sprintf("web.operators[%d]", index)
		if !operatorIDPattern.MatchString(operator.ID) || ids[operator.ID] {
			return fmt.Errorf("config: %s.id must be unique, 1-64 characters of letters, digits and ._@-", field)
		}
		if !validOperatorRoles[operator.Role] {
			return fmt.Errorf("config: %s.role must be viewer, operator or admin", field)
		}
		if !sha256HexPattern.MatchString(operator.TokenSHA256) || hashes[operator.TokenSHA256] {
			return fmt.Errorf("config: %s.token_sha256 must be a unique lowercase hex SHA-256", field)
		}
		if operator.TokenSHA256 == hex.EncodeToString(machine[:]) {
			return fmt.Errorf("config: %s must not reuse server.auth_token; the machine token is not a human identity", field)
		}
		ids[operator.ID], hashes[operator.TokenSHA256] = true, true
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
		if profile.ContextWindow() <= cfg.Roles.Reasoner.MaxTokens || profile.ContextWindow()-cfg.Roles.Reasoner.MaxTokens < 1024 {
			return fmt.Errorf("config: llm.models[%d].context_window_tokens must reserve max_tokens plus at least 1024 input tokens", index)
		}
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
