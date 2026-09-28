package knowledge

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"oncall-agent/internal/store"
	"oncall-agent/internal/tools"
)

func TestRepoEntriesSplitBySection(t *testing.T) {
	entries, err := RepoEntries()
	if err != nil {
		t.Fatal(err)
	}
	refs := map[string]store.KnowledgeEntry{}
	for _, entry := range entries {
		if _, dup := refs[entry.Ref]; dup || entry.Title == "" || entry.Body == "" || len(entry.SHA256) != 64 || len(entry.Body) > maxEntryBody {
			t.Fatalf("bad entry %+v", entry)
		}
		refs[entry.Ref] = entry
	}
	got, ok := refs["dependencies.md#PostgreSQL 常见错误的含义"]
	if !ok || got.Title != "PostgreSQL 与 Redis 依赖 / PostgreSQL 常见错误的含义" || !strings.HasPrefix(got.Body, "## PostgreSQL 常见错误的含义\n") || !strings.Contains(got.Body, "28P01") {
		t.Fatalf("section entry = %+v", got)
	}
}

func TestSplitSectionsKeepsIntroAndFencedHeadings(t *testing.T) {
	title, sections, err := splitSections("# 手册\n开篇说明\n## 一\n正文\n```\n## 不是标题\n```\n## 二\n\n")
	if err != nil || title != "手册" || len(sections) != 2 {
		t.Fatalf("title=%q sections=%+v err=%v", title, sections, err)
	}
	if sections[0].heading != "" || sections[0].body != "开篇说明" || sections[1].heading != "一" || !strings.Contains(sections[1].body, "## 不是标题") {
		t.Fatalf("sections = %+v (an empty section is dropped)", sections)
	}
	if _, _, err := splitSections("no title\n## a\n"); err == nil {
		t.Fatal("a file without a title was accepted")
	}
}

type fakeReader struct {
	query, source string
	hits          []store.KnowledgeHit
	entry         store.KnowledgeEntry
	err           error
}

func (f *fakeReader) SearchKnowledge(_ context.Context, query, source string, limit int) ([]store.KnowledgeHit, error) {
	f.query, f.source = query, source
	if limit != searchLimit {
		return nil, errors.New("unexpected limit")
	}
	return f.hits, f.err
}

func (f *fakeReader) GetKnowledge(_ context.Context, id uint64) (store.KnowledgeEntry, error) {
	if id != f.entry.ID {
		return store.KnowledgeEntry{}, store.ErrKnowledgeNotFound
	}
	return f.entry, nil
}

func TestKnowledgeToolsGoThroughTheRegistry(t *testing.T) {
	reader := &fakeReader{
		hits:  []store.KnowledgeHit{{KnowledgeEntry: store.KnowledgeEntry{ID: 7, Source: "repo", Ref: "a.md#b", Title: "A / B", Body: "前文。应用账号密码错误 SQLSTATE 28P01。"}, Score: 1.5}},
		entry: store.KnowledgeEntry{ID: 7, Source: "incident", Ref: "incident/3", Title: "复盘", Body: "根因 password=hunter2", UpdatedAt: time.Unix(0, 0)},
	}
	registry := tools.NewRegistry()
	if err := RegisterTools(registry, reader); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	out, err := registry.Execute(ctx, ToolKnowledgeSearch, json.RawMessage(`{"query":" 28P01 ","source":"repo"}`))
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Results []struct {
			ID      uint64 `json:"id"`
			Snippet string `json:"snippet"`
		} `json:"results"`
		Note string `json:"note"`
	}
	if err := json.Unmarshal([]byte(out), &body); err != nil || len(body.Results) != 1 || body.Results[0].ID != 7 || !strings.Contains(body.Results[0].Snippet, "28P01") || body.Note == "" {
		t.Fatalf("search output = %s (%v)", out, err)
	}
	if reader.query != "28P01" || reader.source != "repo" {
		t.Fatalf("search args = %q %q", reader.query, reader.source)
	}
	for _, bad := range []string{`{"query":""}`, `{"query":"x","source":"web"}`, `{"query":"` + strings.Repeat("x", 257) + `"}`} {
		if _, err := registry.Execute(ctx, ToolKnowledgeSearch, json.RawMessage(bad)); err == nil {
			t.Fatalf("%s accepted", bad)
		}
	}
	// The id arrives as a number or a string; output is sanitized like any tool's.
	for _, args := range []string{`{"id":7}`, `{"id":"7"}`} {
		out, err := registry.Execute(ctx, ToolKnowledgeRead, json.RawMessage(args))
		if err != nil || !strings.Contains(out, "不是本次事件的证据") || strings.Contains(out, "hunter2") {
			t.Fatalf("%s: read = %q %v", args, out, err)
		}
	}
	for _, bad := range []string{`{"id":0}`, `{"id":"x"}`, `{"id":8}`} {
		if _, err := registry.Execute(ctx, ToolKnowledgeRead, json.RawMessage(bad)); err == nil {
			t.Fatalf("%s accepted", bad)
		}
	}
}

func TestSnippetCentersOnTheFirstTermFound(t *testing.T) {
	body := strings.Repeat("甲", 300) + "关键词" + strings.Repeat("乙", 300)
	if got := Snippet(body, "不存在 关键词"); !strings.Contains(got, "关键词") || !strings.HasPrefix(got, "…") || len([]rune(got)) != snippetRunes+1 {
		t.Fatalf("snippet = %q", got)
	}
	if got := Snippet("## 标题\n- **SQLSTATE 28P01**：`password` 错误", "28P01"); got != "标题 - SQLSTATE 28P01：password 错误" {
		t.Fatalf("snippet = %q", got)
	}
	// A Chinese query without spaces is located by its two-character pieces.
	if got := Snippet(strings.Repeat("甲", 300)+"应用账号密码错误"+strings.Repeat("乙", 300), "数据库密码不对"); !strings.Contains(got, "密码错误") {
		t.Fatalf("snippet = %q", got)
	}
	if got := Snippet("短文", "无"); got != "短文" {
		t.Fatalf("snippet = %q", got)
	}
}
