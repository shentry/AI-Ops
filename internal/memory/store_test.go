package memory

import (
	"context"
	"testing"
	"time"

	"oncall-agent/internal/store"
)

type fakeMemoryStore struct {
	entries  map[string]store.FaultMemory
	history  []store.FaultCmdHistory
	touched  int
	demoted  []string
	upserted []store.FaultMemory
}

func newFakeMemoryStore() *fakeMemoryStore {
	return &fakeMemoryStore{entries: map[string]store.FaultMemory{}}
}

func (f *fakeMemoryStore) GetFaultMemory(_ context.Context, fp string) (store.FaultMemory, error) {
	if e, ok := f.entries[fp]; ok {
		return e, nil
	}
	return store.FaultMemory{}, store.ErrMemoryNotFound
}

func (f *fakeMemoryStore) UpsertFaultMemory(_ context.Context, entry store.FaultMemory) error {
	f.entries[entry.Fingerprint] = entry
	f.upserted = append(f.upserted, entry)
	return nil
}

func (f *fakeMemoryStore) TouchFaultMemory(context.Context, string, time.Time) error {
	f.touched++
	return nil
}

func (f *fakeMemoryStore) DemoteFaultMemory(_ context.Context, fp string, _ time.Time) error {
	f.demoted = append(f.demoted, fp)
	entry := f.entries[fp]
	entry.Confidence = "low"
	f.entries[fp] = entry
	return nil
}

func (f *fakeMemoryStore) ListCmdHistory(_ context.Context, fp string, limit int) ([]store.FaultCmdHistory, error) {
	out := make([]store.FaultCmdHistory, 0)
	for _, row := range f.history {
		if row.Fingerprint == fp {
			out = append(out, row)
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func TestFaultFingerprintStable(t *testing.T) {
	a := FaultFingerprint("payments", "HighCPU")
	b := FaultFingerprint("payments", "HighCPU")
	if a != b || len(a) != 12 {
		t.Fatalf("fingerprint = %q vs %q", a, b)
	}
	if FaultFingerprint("payments", "Other") == a {
		t.Fatal("different alert name produced same fingerprint")
	}
}

func TestLookupGates(t *testing.T) {
	fake := newFakeMemoryStore()
	fresh := store.FaultMemory{Fingerprint: "fp1", Confidence: "high", LastSuccess: time.Now()}
	expired := store.FaultMemory{Fingerprint: "fp2", Confidence: "high", LastSuccess: time.Now().Add(-2 * time.Hour)}
	lowConf := store.FaultMemory{Fingerprint: "fp3", Confidence: "low", LastSuccess: time.Now()}
	fake.entries["fp1"] = fresh
	fake.entries["fp2"] = expired
	fake.entries["fp3"] = lowConf
	s := NewStore(fake, 3600, true)
	ctx := context.Background()

	// 命中：high + 未过期，hits 刷新。
	entry, hit, err := s.Lookup(ctx, "fp1")
	if err != nil || !hit || entry.Fingerprint != "fp1" {
		t.Fatalf("Lookup(fp1) = %v, %v, %v", entry, hit, err)
	}
	if fake.touched != 1 {
		t.Fatalf("touched = %d", fake.touched)
	}
	// 过期不命中，也不刷新 hits。
	if _, hit, _ := s.Lookup(ctx, "fp2"); hit {
		t.Fatal("expired entry hit")
	}
	// 低置信不命中。
	if _, hit, _ := s.Lookup(ctx, "fp3"); hit {
		t.Fatal("low confidence entry hit")
	}
	// 不存在不命中不报错。
	if _, hit, err := s.Lookup(ctx, "fp9"); hit || err != nil {
		t.Fatalf("missing lookup = %v, %v", hit, err)
	}
	if fake.touched != 1 {
		t.Fatalf("touched after misses = %d, want 1", fake.touched)
	}
}

func TestCommitOnlyHighConfidence(t *testing.T) {
	fake := newFakeMemoryStore()
	s := NewStore(fake, 3600, true)
	entry := store.FaultMemory{Fingerprint: "fp1", PlanJSON: []byte(`{"action":"none"}`), Confidence: "high"}
	if err := s.Commit(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
	// medium/low 直接拒绝（GC-16）。
	entry.Confidence = "medium"
	if err := s.Commit(context.Background(), entry); err == nil {
		t.Fatal("medium confidence committed")
	}
	// 缺字段拒绝。
	if err := s.Commit(context.Background(), store.FaultMemory{Confidence: "high"}); err == nil {
		t.Fatal("empty fingerprint committed")
	}
}

func TestDemoteBlocksFutureHits(t *testing.T) {
	fake := newFakeMemoryStore()
	fake.entries["fp1"] = store.FaultMemory{Fingerprint: "fp1", Confidence: "high", LastSuccess: time.Now()}
	s := NewStore(fake, 3600, true)
	ctx := context.Background()
	if err := s.Demote(ctx, "fp1"); err != nil {
		t.Fatal(err)
	}
	if _, hit, _ := s.Lookup(ctx, "fp1"); hit {
		t.Fatal("demoted entry still hits")
	}
}

func TestRecentCmdsLimit(t *testing.T) {
	fake := newFakeMemoryStore()
	for range 7 {
		fake.history = append(fake.history, store.FaultCmdHistory{Fingerprint: "fp1", ToolName: "docker_restart"})
	}
	fake.history = append(fake.history, store.FaultCmdHistory{Fingerprint: "other"})
	s := NewStore(fake, 3600, true)
	cmds, err := s.RecentCmds(context.Background(), "fp1", 5)
	if err != nil || len(cmds) != 5 {
		t.Fatalf("RecentCmds = %d, %v", len(cmds), err)
	}
	cmds, _ = s.RecentCmds(context.Background(), "other", 5)
	if len(cmds) != 1 {
		t.Fatalf("other fp = %d", len(cmds))
	}
}
