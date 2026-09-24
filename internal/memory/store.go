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
	TouchFaultMemory(ctx context.Context, fingerprint string, now time.Time) error
	ListCmdHistory(ctx context.Context, fingerprint string, limit int) ([]store.FaultCmdHistory, error)
}

// Store 负责诊断时的记忆读取与命中统计。成功写回和失败降级仅由验证终态事务处理。
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

// RecentCmds 取同指纹的最近命令历史，供证据注入（参考信息，不是权限依据）。
func (s *Store) RecentCmds(ctx context.Context, fingerprint string, limit int) ([]store.FaultCmdHistory, error) {
	return s.db.ListCmdHistory(ctx, fingerprint, limit)
}
