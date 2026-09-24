package approval

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"oncall-agent/internal/incident"
	"oncall-agent/internal/store"
	"oncall-agent/internal/tools"
)

func TestExecutorTreatsCommittedClaimAsTTLAuthority(t *testing.T) {
	calls := 0
	db := newFakeExecStore(approvedApproval(t, 1, false))
	db.claim = func(row *store.Approval, _ incident.ExecutionBinding) bool {
		// The store accepted the TTL before commit; the returned executing row is
		// now past it, as can happen while the claim response is in transit.
		row.ExpiresAt = time.Now().Add(-time.Millisecond)
		return true
	}
	registry := executorTestRegistry(t, tools.L2LowRisk, func(context.Context, json.RawMessage) (string, error) {
		calls++
		return "restarted", nil
	})
	if err := quietExecutor(db, registry, executorBinding()).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || db.rows[0].Status != "executed" || len(db.committed) != 1 {
		t.Fatalf("committed claim was stranded: calls=%d status=%s completions=%d", calls, db.rows[0].Status, len(db.committed))
	}
}
