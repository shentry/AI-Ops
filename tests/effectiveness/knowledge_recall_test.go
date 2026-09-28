package effectiveness

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"testing"

	"oncall-agent/internal/knowledge"
	"oncall-agent/internal/store"
)

type recallCase struct {
	Query    string   `json:"query"`
	Expected []string `json:"expected"`
}

// Knowledge recall runs the real ngram search over the handbook compiled into
// the binary. TEST_KNOWLEDGE_MYSQL_DSN points at its own migrated database: the
// test rewrites the repository entries, so it must not share one with store tests.
func TestKnowledgeRecall(t *testing.T) {
	dsn := os.Getenv("TEST_KNOWLEDGE_MYSQL_DSN")
	if dsn == "" {
		t.Skip("TEST_KNOWLEDGE_MYSQL_DSN is not set")
	}
	raw, err := os.ReadFile("knowledge_recall.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []recallCase
	if err := json.Unmarshal(raw, &cases); err != nil || len(cases) != 20 {
		t.Fatalf("recall cases: %d %v", len(cases), err)
	}
	db, err := store.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	if _, err := knowledge.SyncRepo(ctx, db); err != nil {
		t.Fatal(err)
	}
	entries, err := knowledge.RepoEntries()
	if err != nil {
		t.Fatal(err)
	}
	hits, top1, reciprocal := 0, 0, 0.0
	for _, c := range cases {
		for _, ref := range c.Expected {
			if !slices.ContainsFunc(entries, func(e store.KnowledgeEntry) bool { return e.Ref == ref }) {
				t.Fatalf("%q expects %q, which is not a handbook entry", c.Query, ref)
			}
		}
		results, err := db.SearchKnowledge(ctx, c.Query, store.KnowledgeRepo, 5)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, result := range results {
			got = append(got, result.Ref)
		}
		rank := slices.IndexFunc(got, func(ref string) bool { return slices.Contains(c.Expected, ref) })
		switch {
		case rank < 0:
			t.Logf("miss: %q -> %v", c.Query, got)
		case rank == 0:
			top1++
			fallthrough
		default:
			hits++
			reciprocal += 1 / float64(rank+1)
		}
		if rank > 0 {
			t.Logf("rank %d: %q -> %v", rank+1, c.Query, got)
		}
	}
	// With a corpus of a few dozen sections, top-5 covers a large share of it;
	// top-1 and MRR are the numbers that show ranking quality.
	t.Logf("corpus=%d recall@5=%d/%d recall@1=%d/%d MRR=%.2f", len(entries), hits, len(cases), top1, len(cases), reciprocal/float64(len(cases)))
	// Regression floor at the measured value; see the design doc §5.6.
	if hits < recallFloor {
		t.Fatalf("recall@5 = %d/%d, below the floor %d", hits, len(cases), recallFloor)
	}
}

// Measured 2026-09-28: 20/20 over 22 sections (recall@1 14/20, MRR 0.81).
const recallFloor = 18
