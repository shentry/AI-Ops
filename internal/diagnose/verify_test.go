package diagnose

import (
	"context"
	"errors"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"oncall-agent/internal/store"
)

type fakeVerifyStore struct {
	members  []store.IncidentMember
	steps    []store.AgentRunStep
	stepErr  error
	listErr  error
	listCtxs []context.Context
}

func (f *fakeVerifyStore) ListIncidentMembers(ctx context.Context, _ uint64) ([]store.IncidentMember, error) {
	f.listCtxs = append(f.listCtxs, ctx)
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.members, nil
}

func (f *fakeVerifyStore) AppendRunStepRecord(_ context.Context, record store.RunStepRecord) error {
	f.steps = append(f.steps, record.Step)
	return f.stepErr
}

func TestVerifyPassesWhenAllResolved(t *testing.T) {
	db := &fakeVerifyStore{members: []store.IncidentMember{
		{Fingerprint: "a", Status: "resolved"},
		{Fingerprint: "b", Status: "resolved"},
	}}
	verifier := NewVerifier(db, discardLogger())
	result := verifier.VerifyAfterExecution(context.Background(), 11, 7, 0)
	if !result.Passed {
		t.Fatalf("result = %+v, want passed", result)
	}
	if len(db.steps) != 1 || db.steps[0].Kind != "verify" || db.steps[0].RunID != 11 {
		t.Fatalf("steps = %+v", db.steps)
	}
	if !strings.Contains(string(*db.steps[0].OutputJSON), "passed=true") {
		t.Fatalf("step output = %v", *db.steps[0].OutputJSON)
	}
}

func TestVerifyFailsWhileFiring(t *testing.T) {
	db := &fakeVerifyStore{members: []store.IncidentMember{
		{Fingerprint: "a", Status: "resolved"},
		{Fingerprint: "b", Status: "firing"},
	}}
	result := NewVerifier(db, discardLogger()).VerifyAfterExecution(context.Background(), 11, 7, 0)
	if result.Passed || result.Inconclusive || !strings.Contains(result.Detail, "1/2") {
		t.Fatalf("result = %+v, want failed", result)
	}
}

// 空成员列表不是"全部恢复"：没有可复查对象时必须判不可判定，
// 否则任何动作都会被标记成功并进记忆提交流程。
func TestVerifyEmptyMembersIsInconclusive(t *testing.T) {
	db := &fakeVerifyStore{}
	result := NewVerifier(db, discardLogger()).VerifyAfterExecution(context.Background(), 11, 7, 0)
	if result.Passed || !result.Inconclusive {
		t.Fatalf("result = %+v, want inconclusive", result)
	}
	if !strings.Contains(result.Detail, "no members") {
		t.Fatalf("detail = %q", result.Detail)
	}
	if len(db.steps) != 1 || !strings.Contains(string(*db.steps[0].OutputJSON), "inconclusive=true") {
		t.Fatalf("steps = %+v", db.steps)
	}
}

// 读库失败同样是不可判定，不能被当成"故障没恢复"。
func TestVerifyReadFailureIsInconclusive(t *testing.T) {
	db := &fakeVerifyStore{listErr: errors.New("db down")}
	result := NewVerifier(db, discardLogger()).VerifyAfterExecution(context.Background(), 11, 7, 0)
	if result.Passed || !result.Inconclusive {
		t.Fatalf("result = %+v, want inconclusive", result)
	}
}

func TestVerifyDelayRespectsContext(t *testing.T) {
	db := &fakeVerifyStore{}
	verifier := NewVerifier(db, discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := verifier.VerifyAfterExecution(ctx, 11, 7, time.Hour)
	if result.Passed || !result.Inconclusive || !strings.Contains(result.Detail, "canceled") {
		t.Fatalf("result = %+v, want canceled", result)
	}
	// 取消的路径也落 step。
	if len(db.steps) != 1 {
		t.Fatalf("steps = %d, want 1", len(db.steps))
	}
}

// 调用方 ctx 已取消时（进程正在关闭），复查查询不能带着取消的 ctx 去读库 ——
// 那会返回 context canceled 并被误记成"故障没恢复"；审计 step 也必须照样写进去。
func TestVerifyDetachesContextFromCancellation(t *testing.T) {
	db := &fakeVerifyStore{members: []store.IncidentMember{{Fingerprint: "a", Status: "resolved"}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// delay=0：跳过等待分支，直接进入复查。
	result := NewVerifier(db, discardLogger()).VerifyAfterExecution(ctx, 11, 7, 0)
	if !result.Inconclusive {
		t.Fatalf("result = %+v, want inconclusive on canceled ctx", result)
	}
	if len(db.steps) != 1 {
		t.Fatalf("steps = %d, want the verdict recorded anyway", len(db.steps))
	}
}

// 写 step 失败不能静默：结论必须出现在日志里，否则回放时分不清
// 是"没跑 verify"还是"跑了没写进去"。
func TestVerifyLogsStepWriteFailure(t *testing.T) {
	db := &fakeVerifyStore{
		members: []store.IncidentMember{{Fingerprint: "a", Status: "resolved"}},
		stepErr: errors.New("insert failed"),
	}
	var logs strings.Builder
	result := NewVerifier(db, log.New(&logs, "", 0)).VerifyAfterExecution(context.Background(), 11, 7, 0)
	if !result.Passed {
		t.Fatalf("result = %+v", result)
	}
	if !strings.Contains(logs.String(), "insert failed") || !strings.Contains(logs.String(), "passed=true") {
		t.Fatalf("log = %q, want the write failure and the verdict", logs.String())
	}
}

func discardLogger() *log.Logger { return log.New(io.Discard, "", 0) }
