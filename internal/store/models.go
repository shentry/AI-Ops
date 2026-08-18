package store

import (
	"time"

	"gorm.io/datatypes"
)

// RawEvent is the append-only Alertmanager webhook envelope.
type RawEvent struct {
	ID          uint64         `gorm:"column:id;primaryKey;autoIncrement"`
	Source      string         `gorm:"column:source;size:64;not null"`
	Payload     datatypes.JSON `gorm:"column:payload;type:json;not null"`
	Status      string         `gorm:"column:status;size:16;not null"`
	Error       *string        `gorm:"column:error;type:text"`
	CreatedAt   time.Time      `gorm:"column:created_at;not null"`
	ProcessedAt *time.Time     `gorm:"column:processed_at"`
}

func (RawEvent) TableName() string { return "raw_event" }

// Alert stores one normalized alert delivery and is never updated.
type Alert struct {
	ID           uint64         `gorm:"column:id;primaryKey;autoIncrement"`
	Fingerprint  string         `gorm:"column:fingerprint;size:64;not null"`
	AlertHash    string         `gorm:"column:alert_hash;size:32;not null"`
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

// LastAlert is the current fingerprint snapshot used by deduplication.
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

// IncidentAlert links an incident to a fingerprint.
type IncidentAlert struct {
	IncidentID  uint64    `gorm:"column:incident_id;primaryKey;not null"`
	Fingerprint string    `gorm:"column:fingerprint;primaryKey;size:64;not null"`
	LinkedAt    time.Time `gorm:"column:linked_at;not null"`
}

func (IncidentAlert) TableName() string { return "incident_alert" }

// Incident is the correlated alert lifecycle record.
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

// FaultMemory is a validated reusable diagnosis case.
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

// FaultCmdHistory records an approved command result for later diagnosis.
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

// Approval is the durable L2 action state machine.
type Approval struct {
	ID         uint64          `gorm:"column:id;primaryKey;autoIncrement"`
	IncidentID uint64          `gorm:"column:incident_id;not null"`
	RunID      uint64          `gorm:"column:run_id;not null"`
	ToolName   string          `gorm:"column:tool_name;size:128;not null"`
	ArgsJSON   datatypes.JSON  `gorm:"column:args_json;type:json;not null"`
	Reason     string          `gorm:"column:reason;type:text;not null"`
	Status     string          `gorm:"column:status;size:9;not null"`
	ExpiresAt  time.Time       `gorm:"column:expires_at;not null"`
	DecidedBy  *string         `gorm:"column:decided_by;size:64"`
	ResultJSON *datatypes.JSON `gorm:"column:result_json;type:json"`
	CreatedAt  time.Time       `gorm:"column:created_at;not null"`
}

func (Approval) TableName() string { return "approval" }

// AgentRun is one diagnostic attempt and queue item.
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

// AgentRunStep is one replayable diagnostic pipeline step.
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
