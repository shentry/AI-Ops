package store

import (
	"time"
)

// VerificationCompletion is one observation and its conditional business effects.
// The store validates the claim and current scope before committing any effect.
// In phase verify Status is pending, passed, failed or inconclusive; in phase
// watch it is pending, stable or recurred.
type VerificationCompletion struct {
	ApprovalID  uint64
	ClaimedAt   time.Time
	CheckedAt   time.Time
	Status      string
	NextCheckAt time.Time
	Observation string
	Detail      string
	// Memory is written only when recovery is confirmed stable (or passed
	// without a watch window): a brief improvement is not a reusable fix.
	Memory            *FaultMemory
	DemoteFingerprint string
	Retry             bool
}

type VerificationFinalization struct {
	Applied bool
	Status  string
	Phase   string
	// CompensationID is the frozen undo queued because verification failed.
	CompensationID uint64
	RetryRunID     uint64
	Escalated      bool
}
