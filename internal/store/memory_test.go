package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// 故障记忆读写。

func TestFaultMemoryCRUD(t *testing.T) {
	db := openIntegrationDB(t)
	ctx := context.Background()
	fp := "testfp" + md5Hex(t.Name())[:6]
	t.Cleanup(func() {
		db.Where("fingerprint = ?", fp).Delete(&FaultMemory{})
		db.Where("fingerprint = ?", fp).Delete(&FaultCmdHistory{})
		db.Close()
	})

	now := time.Date(2026, 8, 19, 15, 0, 0, 0, time.UTC)
	if _, err := db.GetFaultMemory(ctx, fp); !errors.Is(err, ErrMemoryNotFound) {
		t.Fatalf("missing lookup = %v", err)
	}
	entry := FaultMemory{
		Fingerprint: fp, GroupKey: "payments", AlertName: "HighCPU",
		RCAText: "容器退出", PlanJSON: []byte(`{"action":"docker_restart"}`),
		Confidence: "high", FirstSeen: now, LastSuccess: now, TTLSeconds: 3600,
	}
	if err := db.UpsertFaultMemory(ctx, entry); err != nil {
		t.Fatal(err)
	}
	// Upsert 覆盖同指纹。
	entry.RCAText = "新 RCA"
	if err := db.UpsertFaultMemory(ctx, entry); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetFaultMemory(ctx, fp)
	if err != nil {
		t.Fatal(err)
	}
	if got.RCAText != "新 RCA" || got.Confidence != "high" {
		t.Fatalf("got = %+v", got)
	}
	// 命中计数。
	if err := db.TouchFaultMemory(ctx, fp, now); err != nil {
		t.Fatal(err)
	}
	got, _ = db.GetFaultMemory(ctx, fp)
	if got.Hits != 1 || got.LastUsed == nil {
		t.Fatalf("after touch = %+v", got)
	}
	// 降级。
	if err := db.DemoteFaultMemory(ctx, fp, now); err != nil {
		t.Fatal(err)
	}
	got, _ = db.GetFaultMemory(ctx, fp)
	if got.Confidence != "low" {
		t.Fatalf("after demote confidence = %q", got.Confidence)
	}
	// 命令历史倒序限量。
	for i := range 7 {
		if err := db.InsertFaultCmdHistory(ctx, FaultCmdHistory{
			Fingerprint: fp, ToolName: "docker_restart", ArgsJSON: []byte(`{}`),
			ResultBrief: "ok", CreatedAt: now.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
	}
	cmds, err := db.ListCmdHistory(ctx, fp, 5)
	if err != nil || len(cmds) != 5 {
		t.Fatalf("ListCmdHistory = %d, %v", len(cmds), err)
	}
}
