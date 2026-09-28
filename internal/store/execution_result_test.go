package store

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestBoundedExecutionResultPreservesReplayIdentity(t *testing.T) {
	db := openIntegrationDB(t)
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	approval, policy := executionFixture(t, db, now, "approved")
	if _, claimed, err := db.ClaimApprovalExecution(ctx, approval.ID, now, policy); err != nil || !claimed {
		t.Fatalf("claim=%v %v", claimed, err)
	}
	result, _ := json.Marshal(map[string]string{"output": strings.Repeat("x", maxExecutionResultBytes), "detail": "first"})
	completion := ExecutionCompletion{ApprovalID: approval.ID, Status: "executed", ResultJSON: result, FinishedAt: now}
	if err := db.FinishExecution(ctx, completion); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetApproval(ctx, approval.ID)
	if err != nil || got.ResultJSON == nil || len(*got.ResultJSON) > maxExecutionResultBytes {
		t.Fatalf("unbounded result: %+v %v", got, err)
	}
	if err := db.FinishExecution(ctx, completion); err != nil {
		t.Fatalf("same replay: %v", err)
	}
	// Formatting and key order are not new content, even when the result is omitted.
	completion.ResultJSON = []byte("{\"output\":" + strconv.Quote(strings.Repeat("x", maxExecutionResultBytes)) + ", \"detail\": \"first\"}")
	if err := db.FinishExecution(ctx, completion); err != nil {
		t.Fatalf("equivalent replay: %v", err)
	}
	completion.ResultJSON, _ = json.Marshal(map[string]string{"output": strings.Repeat("x", maxExecutionResultBytes), "detail": "different"})
	if err := db.FinishExecution(ctx, completion); !errors.Is(err, ErrApprovalConflict) {
		t.Fatalf("different omitted result must conflict: %v", err)
	}
	var count int64
	if err := db.Model(&FaultCmdHistory{}).Where("approval_id = ?", approval.ID).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("history=%d %v", count, err)
	}
}
