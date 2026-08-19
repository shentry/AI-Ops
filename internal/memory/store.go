package memory

import (
	"context"
	"errors"
	"fmt"
	"time"

	"oncall-agent/internal/metrics"
	"oncall-agent/internal/store"
)

// memoryStore 是 Store 对存储层的收窄接口。
type memoryStore interface {
	GetFaultMemory(ctx context.Context, fingerprint string) (store.FaultMemory, error)
	UpsertFaultMemory(ctx context.Context, entry store.FaultMemory) error
	TouchFaultMemory(ctx context.Context, fingerprint string, now time.Time) error
	DemoteFaultMemory(ctx context.Context, fingerprint string, now time.Time) error
	ListCmdHistory(ctx context.Context, fingerprint string, limit int) ([]store.FaultCmdHistory, error)
}

// Store 是故障记忆的读写面。Lookup 只返回 high 置信且 TTL 未过期的条目；
// 命中计数（hits/last_used）的刷新是审计行为，不改变安全等级。
type Store struct {
	db                 memoryStore
	ttlSeconds         int
	onlyHighConfidence bool
}

func NewStore(db memoryStore, ttlSeconds int, onlyHighConfidence bool) *Store {
	return &Store{db: db, ttlSeconds: ttlSeconds, onlyHighConfidence: onlyHighConfidence}
}

// Lookup 命中返回条目和 true。未命中原因（miss/expired/low）只作日志区分，
// 不改变调用方行为 —— 一律走完整诊断。
func (s *Store) Lookup(ctx context.Context, fingerprint string) (store.FaultMemory, bool, error) {
	entry, err := s.db.GetFaultMemory(ctx, fingerprint)
	if errors.Is(err, store.ErrMemoryNotFound) {
		return store.FaultMemory{}, false, nil
	}
	if err != nil {
		return store.FaultMemory{}, false, err
	}
	if s.onlyHighConfidence && entry.Confidence != "high" {
		return store.FaultMemory{}, false, nil
	}
	// TTL 以 last_success 为基准：记忆的有效期看"上次验证成功"。
	if entry.LastSuccess.Add(time.Duration(s.ttlSeconds) * time.Second).Before(time.Now().UTC()) {
		metrics.Inc(metrics.MemoryExpired)
		return store.FaultMemory{}, false, nil
	}
	if err := s.db.TouchFaultMemory(ctx, fingerprint, time.Now().UTC()); err != nil {
		return store.FaultMemory{}, false, fmt.Errorf("memory: touch on hit: %w", err)
	}
	return entry, true, nil
}

// Commit 写入记忆。门槛在代码里硬编码：只有 high 置信允许入库（GC-16），
// 验证成功与 Guard 未改写由调用方（执行器）保证。
func (s *Store) Commit(ctx context.Context, entry store.FaultMemory) error {
	if entry.Fingerprint == "" || entry.PlanJSON == nil {
		return errors.New("memory: fingerprint and plan are required")
	}
	if entry.Confidence != "high" {
		return fmt.Errorf("memory: only high confidence entries are accepted, got %q", entry.Confidence)
	}
	now := time.Now().UTC()
	entry.FirstSeen = now
	entry.LastSuccess = now
	entry.TTLSeconds = s.ttlSeconds
	return s.db.UpsertFaultMemory(ctx, entry)
}

// Demote 命中后验证失败 → 置信度降 low（拉黑），同指纹下次不再命中。
func (s *Store) Demote(ctx context.Context, fingerprint string) error {
	metrics.Inc(metrics.MemoryDemoted)
	return s.db.DemoteFaultMemory(ctx, fingerprint, time.Now().UTC())
}

// RecentCmds 取同指纹的最近命令历史，供证据注入（参考信息，不是权限依据）。
func (s *Store) RecentCmds(ctx context.Context, fingerprint string, limit int) ([]store.FaultCmdHistory, error) {
	return s.db.ListCmdHistory(ctx, fingerprint, limit)
}
