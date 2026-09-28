package store

import (
	"time"

	"gorm.io/datatypes"
)

// 模型对应 migrations/001_init.sql。TableName() 写死，避免 GORM 复数化
// 和 SQL 表名对不上。可空列用指针，NULL 不会被读成 Go 零值。

// RawEvent 保存 webhook 原文，供 D03 处理。
// HTTP 接收和归一化/去重解耦，崩溃后可以重放 pending 行。
type RawEvent struct {
	ID          uint64         `gorm:"column:id;primaryKey;autoIncrement"`
	Source      string         `gorm:"column:source;size:64;not null"`
	Payload     datatypes.JSON `gorm:"column:payload;type:json;not null"`
	Status      string         `gorm:"column:status;size:16;not null"` // pending / processed / failed
	Error       *string        `gorm:"column:error;type:text"`
	CreatedAt   time.Time      `gorm:"column:created_at;not null"`
	ProcessedAt *time.Time     `gorm:"column:processed_at"`
}

func (RawEvent) TableName() string { return "raw_event" }

// Alert 是 NormalizedAlert 的追加历史。D03 只 INSERT，不 UPDATE。
type Alert struct {
	ID           uint64         `gorm:"column:id;primaryKey;autoIncrement"`
	Fingerprint  string         `gorm:"column:fingerprint;size:64;not null"` // D02 的 SHA-256
	AlertHash    string         `gorm:"column:alert_hash;size:32;not null"`  // D02 的 MD5 FullHash
	Source       string         `gorm:"column:source;size:64;not null"`
	Name         string         `gorm:"column:name;size:255;not null"`
	Severity     uint8          `gorm:"column:severity;not null"`
	Status       string         `gorm:"column:status;size:8;not null"`
	Labels       datatypes.JSON `gorm:"column:labels;type:json;not null"`
	Annotations  datatypes.JSON `gorm:"column:annotations;type:json;not null"`
	GeneratorURL string         `gorm:"column:generator_url;type:text;not null"`
	StartsAt     time.Time      `gorm:"column:starts_at;not null"`
	ReceivedAt   time.Time      `gorm:"column:received_at;not null"`
}

func (Alert) TableName() string { return "alert" }

// LastAlert 是按 fingerprint 索引的当前快照。
// D03 全量去重只跟上一条 AlertHash 比，不扫历史。
type LastAlert struct {
	Fingerprint string    `gorm:"column:fingerprint;primaryKey;size:64"`
	AlertID     uint64    `gorm:"column:alert_id;not null"`
	AlertHash   string    `gorm:"column:alert_hash;size:32;not null"`
	Status      string    `gorm:"column:status;size:8;not null"`
	Severity    uint8     `gorm:"column:severity;not null"`
	FirstSeen   time.Time `gorm:"column:first_seen;not null"`
	LastSeen    time.Time `gorm:"column:last_seen;not null"`
	FiringCount int       `gorm:"column:firing_count;not null;default:1"`
	IncidentID  *uint64   `gorm:"column:incident_id"`
}

func (LastAlert) TableName() string { return "last_alert" }

// IncidentAlert 是 D04 成员表。联合主键保证同一 fingerprint 不会进同一个 incident 两次。
type IncidentAlert struct {
	IncidentID  uint64    `gorm:"column:incident_id;primaryKey;not null"`
	Fingerprint string    `gorm:"column:fingerprint;primaryKey;size:64;not null"`
	LinkedAt    time.Time `gorm:"column:linked_at;not null"`
}

func (IncidentAlert) TableName() string { return "incident_alert" }

// Incident 是 D04/D05 聚合后的事件。状态：candidate → firing → acknowledged → resolved。
type Incident struct {
	ID          uint64     `gorm:"column:id;primaryKey;autoIncrement"`
	GroupKey    string     `gorm:"column:group_key;size:255;not null"`
	Status      string     `gorm:"column:status;size:16;not null"`
	Severity    uint8      `gorm:"column:severity;not null"`
	AlertsCount int        `gorm:"column:alerts_count;not null;default:0"`
	Title       string     `gorm:"column:title;size:512;not null"`
	StartedAt   time.Time  `gorm:"column:started_at;not null"`
	LastSeenAt  time.Time  `gorm:"column:last_seen_at;not null"`
	ResolvedAt  *time.Time `gorm:"column:resolved_at"`
}

func (Incident) TableName() string { return "incident" }

// FaultMemory 是 D13 可复用根因。CHAR(12) 是记忆键，不是 D02 的 SHA-256 告警指纹。
type FaultMemory struct {
	Fingerprint string         `gorm:"column:fingerprint;primaryKey;size:12"`
	GroupKey    string         `gorm:"column:group_key;size:255;not null"`
	AlertName   string         `gorm:"column:alert_name;size:255;not null"`
	RCAText     string         `gorm:"column:rca_text;type:text;not null"`
	PlanJSON    datatypes.JSON `gorm:"column:plan_json;type:json;not null"`
	Confidence  string         `gorm:"column:confidence;size:6;not null"`
	Hits        int            `gorm:"column:hits;not null;default:0"`
	FirstSeen   time.Time      `gorm:"column:first_seen;not null"`
	LastUsed    *time.Time     `gorm:"column:last_used"`
	LastSuccess time.Time      `gorm:"column:last_success;not null"`
	TTLSeconds  int            `gorm:"column:ttl_sec;not null;default:3600"`
}

func (FaultMemory) TableName() string { return "fault_memory" }

// FaultCmdHistory 记录已审批命令结果，供后续诊断注入。
type FaultCmdHistory struct {
	ID          uint64         `gorm:"column:id;primaryKey;autoIncrement"`
	Fingerprint string         `gorm:"column:fingerprint;size:12;not null"`
	ToolName    string         `gorm:"column:tool_name;size:128;not null"`
	ArgsJSON    datatypes.JSON `gorm:"column:args_json;type:json;not null"`
	ResultBrief string         `gorm:"column:result_brief;size:1024;not null"`
	ApprovalID  *uint64        `gorm:"column:approval_id"`
	CreatedAt   time.Time      `gorm:"column:created_at;not null"`
}

func (FaultCmdHistory) TableName() string { return "fault_cmd_history" }

// Approval is one disposition: a frozen action snapshot and its lifecycle
// pending → approved → executing → executed/failed/aborted (or denied/expired).
// Service and RuleID mirror the snapshot for queries; ParentApprovalID links a
// compensation to the action it undoes.
type Approval struct {
	ID               uint64          `gorm:"column:id;primaryKey;autoIncrement"`
	IncidentID       uint64          `gorm:"column:incident_id;not null"`
	RunID            uint64          `gorm:"column:run_id;not null"`
	Service          *string         `gorm:"column:service;size:64"`
	RuleID           *string         `gorm:"column:rule_id;size:64"`
	ParentApprovalID *uint64         `gorm:"column:parent_approval_id"`
	ToolName         string          `gorm:"column:tool_name;size:128;not null"`
	ArgsJSON         datatypes.JSON  `gorm:"column:args_json;type:json;not null"`
	Reason           string          `gorm:"column:reason;type:text;not null"`
	PlanHash         string          `gorm:"column:plan_hash;size:64;not null"`
	ExecutionContext datatypes.JSON  `gorm:"column:execution_context;type:json"`
	Status           string          `gorm:"column:status;size:9;not null"`
	ExpiresAt        time.Time       `gorm:"column:expires_at;not null"`
	DecidedBy        *string         `gorm:"column:decided_by;size:64"`
	DecidedAt        *time.Time      `gorm:"column:decided_at"`
	DecisionReason   *string         `gorm:"column:decision_reason;type:text"`
	DecisionSource   *string         `gorm:"column:decision_source;size:16"`
	ResultJSON       *datatypes.JSON `gorm:"column:result_json;type:json"`
	// OperationID is persisted at claim, before the external call, and reused
	// as the idempotency key where the target supports one.
	OperationID        *string     `gorm:"column:operation_id;size:64"`
	OperationStartedAt *time.Time  `gorm:"column:operation_started_at"`
	CreatedAt          time.Time   `gorm:"column:created_at;not null"`
	Verification       *VerifyTask `gorm:"foreignKey:ApprovalID;references:ID" json:"-"`
}

func (Approval) TableName() string { return "approval" }

// VerifyTask is the durable read-only recovery queue. Execution remains on Approval.
// Phase verify decides recovery; phase watch looks for a recurrence afterwards.
type VerifyTask struct {
	ApprovalID     uint64         `gorm:"column:approval_id;primaryKey;autoIncrement:false"`
	Status         string         `gorm:"column:status;size:12;not null"`
	Phase          string         `gorm:"column:phase;size:8;not null;default:verify"`
	NextCheckAt    time.Time      `gorm:"column:next_check_at;not null"`
	DeadlineAt     time.Time      `gorm:"column:deadline_at;not null"`
	ClaimedAt      *time.Time     `gorm:"column:claimed_at"`
	LastCheckedAt  *time.Time     `gorm:"column:last_checked_at"`
	LastResultJSON datatypes.JSON `gorm:"column:last_result_json;type:json"`
	// ConsecutivePasses counts healthy observations since the last non-healthy
	// one; ConsecutiveFailures counts unhealthy ones during the watch.
	ConsecutivePasses   int        `gorm:"column:consecutive_passes;not null;default:0"`
	ConsecutiveFailures int        `gorm:"column:consecutive_failures;not null;default:0"`
	CreatedAt           time.Time  `gorm:"column:created_at;not null"`
	FinishedAt          *time.Time `gorm:"column:finished_at"`
}

func (VerifyTask) TableName() string { return "verify_task" }

// AgentRun 是一次诊断尝试，也是 D05 诊断 worker 的队列行。
type AgentRun struct {
	ID         uint64          `gorm:"column:id;primaryKey;autoIncrement"`
	IncidentID uint64          `gorm:"column:incident_id;not null"`
	Mode       string          `gorm:"column:mode;size:10;not null"`
	Status     string          `gorm:"column:status;size:9;not null"`
	RetryOf    *uint64         `gorm:"column:retry_of"`
	RCAText    *string         `gorm:"column:rca_text;type:text"`
	PlanJSON   *datatypes.JSON `gorm:"column:plan_json;type:json"`
	TokensIn   int             `gorm:"column:tokens_in;not null;default:0"`
	TokensOut  int             `gorm:"column:tokens_out;not null;default:0"`
	StartedAt  time.Time       `gorm:"column:started_at;not null"`
	FinishedAt *time.Time      `gorm:"column:finished_at"`
}

func (AgentRun) TableName() string { return "agent_run" }

// AgentRunStep 是可回放的流水线一步（evidence/llm/tool/guard/approval/verify）。
type AgentRunStep struct {
	ID         uint64          `gorm:"column:id;primaryKey;autoIncrement"`
	RunID      uint64          `gorm:"column:run_id;not null"`
	Seq        int             `gorm:"column:seq;not null"`
	Kind       string          `gorm:"column:kind;size:9;not null"`
	Name       string          `gorm:"column:name;size:128;not null"`
	InputJSON  *datatypes.JSON `gorm:"column:input_json;type:json"`
	OutputJSON *datatypes.JSON `gorm:"column:output_json;type:json"`
	Error      *string         `gorm:"column:error;type:text"`
	StartedAt  time.Time       `gorm:"column:started_at;not null"`
	FinishedAt *time.Time      `gorm:"column:finished_at"`
}

func (AgentRunStep) TableName() string { return "agent_run_step" }

// IncidentEvent 是 Incident 的持久化事实事件，只保存脱敏摘要和有限 payload。
type IncidentEvent struct {
	ID          uint64          `gorm:"column:id;primaryKey;autoIncrement"`
	IncidentID  uint64          `gorm:"column:incident_id;not null"`
	RunID       *uint64         `gorm:"column:run_id"`
	ApprovalID  *uint64         `gorm:"column:approval_id"`
	EventType   string          `gorm:"column:event_type;size:64;not null"`
	Phase       string          `gorm:"column:phase;size:32;not null"`
	Status      string          `gorm:"column:status;size:32;not null"`
	Summary     string          `gorm:"column:summary;size:512;not null"`
	PayloadJSON *datatypes.JSON `gorm:"column:payload_json;type:json"`
	CreatedAt   time.Time       `gorm:"column:created_at;not null"`
}

func (IncidentEvent) TableName() string { return "incident_event" }

// IncidentProblem 是 Incident 当前问题读模型。incident_id/code 唯一，重现问题只更新同一行。
type IncidentProblem struct {
	ID          uint64          `gorm:"column:id;primaryKey;autoIncrement"`
	IncidentID  uint64          `gorm:"column:incident_id;not null"`
	RunID       *uint64         `gorm:"column:run_id"`
	Code        string          `gorm:"column:code;size:64;not null"`
	Severity    string          `gorm:"column:severity;size:16;not null"`
	Status      string          `gorm:"column:status;size:16;not null"`
	Summary     string          `gorm:"column:summary;size:512;not null"`
	DetailJSON  *datatypes.JSON `gorm:"column:detail_json;type:json"`
	FirstSeenAt time.Time       `gorm:"column:first_seen_at;not null"`
	LastSeenAt  time.Time       `gorm:"column:last_seen_at;not null"`
	ResolvedAt  *time.Time      `gorm:"column:resolved_at"`
}

func (IncidentProblem) TableName() string { return "incident_problem" }

// ConversationMessage 是 Incident 绑定的 Web/Feishu 对话消息。
type ConversationMessage struct {
	ID           uint64          `gorm:"column:id;primaryKey;autoIncrement"`
	IncidentID   uint64          `gorm:"column:incident_id;not null"`
	RunID        *uint64         `gorm:"column:run_id"`
	ReplyToID    *uint64         `gorm:"column:reply_to_id"`
	Channel      string          `gorm:"column:channel;size:16;not null"`
	Role         string          `gorm:"column:role;size:16;not null"`
	ActorID      *string         `gorm:"column:actor_id;size:128"`
	ActorName    *string         `gorm:"column:actor_name;size:128"`
	Content      string          `gorm:"column:content;type:text;not null"`
	ToolName     *string         `gorm:"column:tool_name;size:128"`
	ToolCallID   *string         `gorm:"column:tool_call_id;size:128"`
	Status       string          `gorm:"column:status;size:16;not null"`
	MetadataJSON *datatypes.JSON `gorm:"column:metadata_json;type:json"`
	CreatedAt    time.Time       `gorm:"column:created_at;not null"`
	ClaimedAt    *time.Time      `gorm:"column:claimed_at"`
	FinishedAt   *time.Time      `gorm:"column:finished_at"`
}

func (ConversationMessage) TableName() string { return "conversation_message" }

// IMBinding 将飞书消息/线程绑定到 Incident、Run 或 Approval。
type IMBinding struct {
	ID            uint64    `gorm:"column:id;primaryKey;autoIncrement"`
	Provider      string    `gorm:"column:provider;size:16;not null"`
	ChatID        string    `gorm:"column:chat_id;size:128;not null"`
	MessageID     string    `gorm:"column:message_id;size:128;not null"`
	RootMessageID *string   `gorm:"column:root_message_id;size:128"`
	ThreadID      *string   `gorm:"column:thread_id;size:128"`
	IncidentID    uint64    `gorm:"column:incident_id;not null"`
	RunID         *uint64   `gorm:"column:run_id"`
	ApprovalID    *uint64   `gorm:"column:approval_id"`
	MessageKind   string    `gorm:"column:message_kind;size:32;not null"`
	CreatedAt     time.Time `gorm:"column:created_at;not null"`
}

func (IMBinding) TableName() string { return "im_binding" }

// IntegrationEventReceipt 用于第三方回调 event_id 去重。
type IntegrationEventReceipt struct {
	EventID     string    `gorm:"column:event_id;primaryKey;size:128"`
	Provider    string    `gorm:"column:provider;size:16;not null"`
	EventType   string    `gorm:"column:event_type;size:64;not null"`
	ProcessedAt time.Time `gorm:"column:processed_at;not null"`
	Result      string    `gorm:"column:result;size:32;not null"`
}

func (IntegrationEventReceipt) TableName() string { return "integration_event_receipt" }

// LLMModelSelection 是唯一的全局当前模型记录。模型 allowlist、端点和
// 凭据始终留在配置文件；此表只保存已选择的安全模型 ID。
type LLMModelSelection struct {
	SingletonID  uint8     `gorm:"column:singleton_id;primaryKey;not null"`
	CurrentModel string    `gorm:"column:current_model;size:128;not null"`
	UpdatedAt    time.Time `gorm:"column:updated_at;not null"`
}

func (LLMModelSelection) TableName() string { return "llm_model_selection" }

// ServiceLock is the row a claim locks to serialize dispositions of a service.
type ServiceLock struct {
	Service   string    `gorm:"column:service;primaryKey;size:64"`
	CreatedAt time.Time `gorm:"column:created_at;not null"`
}

func (ServiceLock) TableName() string { return "service_lock" }

// ControlEvent is an append-only global decision: emergency stop/resume, a rule
// block reset, or a rules release loaded at startup.
type ControlEvent struct {
	ID         uint64          `gorm:"column:id;primaryKey;autoIncrement"`
	Kind       string          `gorm:"column:kind;size:32;not null"`
	RuleID     *string         `gorm:"column:rule_id;size:64"`
	Actor      string          `gorm:"column:actor;size:64;not null"`
	Reason     string          `gorm:"column:reason;size:512;not null"`
	DetailJSON *datatypes.JSON `gorm:"column:detail_json;type:json"`
	CreatedAt  time.Time       `gorm:"column:created_at;not null"`
}

func (ControlEvent) TableName() string { return "control_event" }

// ChangeEvent is one deployment change of a service. Only references and
// digests are stored; configuration and secret bodies stay in the deploy system.
type ChangeEvent struct {
	ID             uint64     `gorm:"column:id;primaryKey;autoIncrement"`
	Env            string     `gorm:"column:env;size:32;not null"`
	Service        string     `gorm:"column:service;size:64;not null"`
	ChangeType     string     `gorm:"column:change_type;size:16;not null"`
	ReleaseID      *string    `gorm:"column:release_id;size:128"`
	ImageRef       *string    `gorm:"column:image_ref;size:512"`
	BeforeRef      *string    `gorm:"column:before_ref;size:512"`
	ConfigVersion  *string    `gorm:"column:config_version;size:128"`
	DBMigration    string     `gorm:"column:db_migration;size:16;not null"`
	VerifiedAt     *time.Time `gorm:"column:verified_at"`
	OccurredAt     time.Time  `gorm:"column:occurred_at;not null"`
	Source         string     `gorm:"column:source;size:32;not null"`
	Actor          string     `gorm:"column:actor;size:64;not null"`
	IdempotencyKey string     `gorm:"column:idempotency_key;size:128;not null"`
	ApprovalID     *uint64    `gorm:"column:approval_id"`
	CreatedAt      time.Time  `gorm:"column:created_at;not null"`
}

func (ChangeEvent) TableName() string { return "change_event" }

// Review is a person's verdict on a diagnosis (run) or an action (approval),
// with the real root cause and fix. unknown is never counted as correct.
type Review struct {
	ID            uint64    `gorm:"column:id;primaryKey;autoIncrement"`
	IncidentID    uint64    `gorm:"column:incident_id;not null"`
	RunID         *uint64   `gorm:"column:run_id"`
	ApprovalID    *uint64   `gorm:"column:approval_id"`
	Subject       string    `gorm:"column:subject;size:16;not null"`
	Verdict       string    `gorm:"column:verdict;size:16;not null"`
	RootCause     string    `gorm:"column:root_cause;size:1024;not null"`
	ActualFix     string    `gorm:"column:actual_fix;size:1024;not null"`
	ManualMinutes int       `gorm:"column:manual_minutes;not null;default:0"`
	Reviewer      string    `gorm:"column:reviewer;size:64;not null"`
	CreatedAt     time.Time `gorm:"column:created_at;not null"`
}

func (Review) TableName() string { return "review" }
