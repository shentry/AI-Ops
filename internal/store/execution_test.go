package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

// 执行闸门：限频计数与崩溃恢复。

// CountRecentExecutions 是 L2 限频护栏的数据源：只数同 plan_hash、
// 窗口内、且真的进入过执行的单子。pending/denied 不算 —— 那些没动过外部系统。
func TestCountRecentExecutions(t *testing.T) {
	db := openIntegrationDB(t)
	ctx := context.Background()
	planHash := "ratelimit-" + md5Hex(t.Name())
	otherHash := planHash + "-other"
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	var ids []uint64
	t.Cleanup(func() {
		if len(ids) > 0 {
			db.Where("id IN ?", ids).Delete(&Approval{})
		}
		db.Close()
	})

	create := func(hash, status string, createdAt time.Time) uint64 {
		t.Helper()
		created, err := db.CreateApproval(ctx, Approval{
			IncidentID: 1, RunID: 2, ToolName: "docker_restart", ArgsJSON: []byte(`{"target_name":"sub2api"}`),
			Reason: "r", PlanHash: hash, ExpiresAt: createdAt.Add(30 * time.Minute), CreatedAt: createdAt,
		})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, created.ID)
		if status != "pending" {
			if err := db.Model(&Approval{}).Where("id = ?", created.ID).Update("status", status).Error; err != nil {
				t.Fatal(err)
			}
		}
		return created.ID
	}

	create(planHash, "executed", now.Add(-10*time.Minute))
	create(planHash, "failed", now.Add(-20*time.Minute))   // 失败的重复动作也要算
	create(planHash, "executing", now.Add(-time.Minute))   // 正在执行的也占额度
	create(planHash, "pending", now.Add(-5*time.Minute))   // 还没执行 → 不算
	create(planHash, "denied", now.Add(-6*time.Minute))    // 被拒 → 不算
	create(planHash, "executed", now.Add(-90*time.Minute)) // 窗口外 → 不算
	create(otherHash, "executed", now.Add(-time.Minute))   // 别的动作 → 不算

	count, err := db.CountRecentExecutions(ctx, planHash, now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("CountRecentExecutions() = %d, want 3", count)
	}
	// 空 plan_hash 是调用方 bug，不能静默返回 0。
	if _, err := db.CountRecentExecutions(ctx, "  ", now); err == nil {
		t.Fatal("empty plan hash accepted")
	}
}

// 进程在 executing 状态崩掉的审批单必须在启动时被回收成 failed：
// 动作可能已经触达外部系统，不能自动重放，只能要求人工核查。
func TestRecoverExecutingApprovals(t *testing.T) {
	db := openIntegrationDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 20, 13, 0, 0, 0, time.UTC)
	var ids []uint64
	t.Cleanup(func() {
		if len(ids) > 0 {
			db.Where("id IN ?", ids).Delete(&Approval{})
		}
		db.Close()
	})

	stuck, err := db.CreateApproval(ctx, Approval{
		IncidentID: 1, RunID: 2, ToolName: "docker_restart", ArgsJSON: []byte(`{}`),
		Reason: "r", PlanHash: "recover-" + md5Hex(t.Name()), ExpiresAt: now.Add(time.Hour), CreatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	ids = append(ids, stuck.ID)
	if _, err := db.DecideApproval(ctx, stuck.ID, "approved", "ops", "test", "api", now); err != nil {
		t.Fatal(err)
	}
	claimed, err := db.ClaimApprovalExecution(ctx, stuck.ID, now)
	if err != nil || !claimed {
		t.Fatalf("ClaimApprovalExecution() = %v, %v", claimed, err)
	}
	// executing 不会被 TTL sweep 带走 —— 只有 recover 能收拾它。
	if _, err := db.ExpireApprovals(ctx, now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.GetApproval(ctx, stuck.ID); got.Status != "executing" {
		t.Fatalf("status after sweep = %q, want executing", got.Status)
	}
	recovered, err := db.RecoverExecutingApprovals(ctx, now)
	if err != nil || recovered < 1 {
		t.Fatalf("RecoverExecutingApprovals() = %d, %v", recovered, err)
	}
	got, err := db.GetApproval(ctx, stuck.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "failed" {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if got.ResultJSON == nil || !strings.Contains(string(*got.ResultJSON), "manual verification") {
		t.Fatalf("result = %v, want the manual-check reason", got.ResultJSON)
	}
}
