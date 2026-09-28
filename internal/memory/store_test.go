package memory

import (
	"context"
	"testing"
	"time"

	"oncall-agent/internal/store"
)

type fakeMemoryStore struct {
	entries map[string]store.FaultMemory
	history []store.FaultCmdHistory
	touched int
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

func (f *fakeMemoryStore) TouchFaultMemory(context.Context, string, time.Time) error {
	f.touched++
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
