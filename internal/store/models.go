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

// Approval 是 D10 的 L3 变更动作状态机。
type Approval struct {
	ID         uint64          `gorm:"column:id;primaryKey;autoIncrement"`
	IncidentID uint64          `gorm:"column:incident_id;not null"`
	RunID      uint64          `gorm:"column:run_id;not null"`
	ToolName   string          `gorm:"column:tool_name;size:128;not null"`
	ArgsJSON   datatypes.JSON  `gorm:"column:args_json;type:json;not null"`
	Reason     string          `gorm:"column:reason;type:text;not null"`
	PlanHash   string          `gorm:"column:plan_hash;size:64;not null"`
	Status     string          `gorm:"column:status;size:9;not null"`
	ExpiresAt  time.Time       `gorm:"column:expires_at;not null"`
	DecidedBy  *string         `gorm:"column:decided_by;size:64"`
	ResultJSON *datatypes.JSON `gorm:"column:result_json;type:json"`
	CreatedAt  time.Time       `gorm:"column:created_at;not null"`
}

func (Approval) TableName() string { return "approval" }

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
