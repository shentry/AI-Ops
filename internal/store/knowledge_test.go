package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// Repository sync inserts, rewrites and deletes by ref; Chinese full-text
// search goes through the ngram index; incident entries are written and
// removed with their audit events, repository entries cannot be removed.
func TestKnowledgeSyncSearchAndIncidentEntries(t *testing.T) {
	db := openIntegrationDB(t)
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	t.Cleanup(func() { db.Where("source = ?", KnowledgeRepo).Delete(&KnowledgeEntry{}) })
	entry := func(ref, title, body string) KnowledgeEntry {
		return KnowledgeEntry{Ref: ref, Title: title, Body: body, SHA256: sha256Hex(title + body)}
	}
	first := []KnowledgeEntry{
		entry("pg.md#认证", "PostgreSQL / 认证失败", "应用账号密码错误，SQLSTATE 28P01，重启无效"),
		entry("redis.md#连接", "Redis / 连接拒绝", "Redis 端点不可达，connection refused"),
	}
	if a, u, d, err := db.SyncRepoKnowledge(ctx, first, now); err != nil || a != 2 || u != 0 || d != 0 {
		t.Fatalf("first sync = +%d ~%d -%d %v", a, u, d, err)
	}
	second := []KnowledgeEntry{entry("pg.md#认证", "PostgreSQL / 认证失败", "应用账号密码错误，SQLSTATE 28P01，需要人工修正凭据")}
	if a, u, d, err := db.SyncRepoKnowledge(ctx, second, now); err != nil || a != 0 || u != 1 || d != 1 {
		t.Fatalf("second sync = +%d ~%d -%d %v", a, u, d, err)
	}
	hits, err := db.SearchKnowledge(ctx, "数据库密码不对", KnowledgeRepo, 5)
	if err != nil || len(hits) != 1 || hits[0].Ref != "pg.md#认证" || hits[0].Score <= 0 || !strings.Contains(hits[0].Body, "人工修正") {
		t.Fatalf("search = %+v %v", hits, err)
	}

	incident := insertTestIncident(t, db, now, "knowledge")
	saved, err := db.SaveIncidentKnowledge(ctx, incident.ID, KnowledgeEntry{Title: "Incident 复盘", Body: "根因：凭据轮换未同步", SHA256: strings.Repeat("f", 64), CreatedBy: "ops", UpdatedAt: now})
	if err != nil || saved.Source != KnowledgeIncident || saved.ID == 0 {
		t.Fatalf("save = %+v %v", saved, err)
	}
	if again, err := db.SaveIncidentKnowledge(ctx, incident.ID, KnowledgeEntry{Title: "Incident 复盘", Body: "根因：凭据轮换未同步（已修正）", SHA256: strings.Repeat("e", 64), CreatedBy: "ops", UpdatedAt: now}); err != nil || again.ID != saved.ID {
		t.Fatalf("re-adding an incident must rewrite its entry: %+v %v", again, err)
	}
	repo, _ := db.SearchKnowledge(ctx, "认证失败", KnowledgeRepo, 1)
	if err := db.DeleteIncidentKnowledge(ctx, repo[0].ID, "admin", now); !errors.Is(err, ErrKnowledgeReadOnly) {
		t.Fatalf("deleting a repository entry = %v", err)
	}
	if err := db.DeleteIncidentKnowledge(ctx, saved.ID, "admin", now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetKnowledge(ctx, saved.ID); !errors.Is(err, ErrKnowledgeNotFound) {
		t.Fatalf("deleted entry still readable: %v", err)
	}
	events, err := db.ListIncidentEvents(ctx, incident.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, event := range events {
		if strings.HasPrefix(event.EventType, "knowledge.") {
			kinds = append(kinds, event.EventType)
		}
	}
	if strings.Join(kinds, ",") != "knowledge.added,knowledge.added,knowledge.deleted" {
		t.Fatalf("knowledge events = %v", kinds)
	}
}
