package store

import (
	"time"

	"oncall-agent/internal/incident"
)

// VerificationCompletion is one observation and its conditional business effects.
// The store validates the claim and current scope before committing any effect.
type VerificationCompletion struct {
	ApprovalID        uint64
	ClaimedAt         time.Time
	CheckedAt         time.Time
	Status            string
	NextCheckAt       time.Time
	Observation       string
	Detail            string
	Binding           incident.ExecutionBinding
	Memory            *FaultMemory
	DemoteFingerprint string
	Retry             bool
}

type VerificationFinalization struct {
	Applied    bool
	Status     string
	RetryRunID uint64
	Escalated  bool
}
