package approval

import (
	"context"
	"log"
	"time"

	"oncall-agent/internal/metrics"
)

// expiryStore 是过期 worker 对存储层的收窄接口。
type expiryStore interface {
	ExpireApprovals(ctx context.Context, now time.Time) (int64, error)
}

// ExpiryWorker 主动把过期 pending/approved 审批单置为 expired。
// 状态、approval.expired 事件和 approval_near_expiry 问题变更由 store 在同一
// 短事务中完成；worker 只负责按周期调用这个高层 sweep。
type ExpiryWorker struct {
	db       expiryStore
	interval time.Duration
	logger   *log.Logger
	done     chan struct{}
}

func NewExpiryWorker(db expiryStore, logger *log.Logger) *ExpiryWorker {
	return newExpiryWorker(db, time.Minute, logger)
}

func newExpiryWorker(db expiryStore, interval time.Duration, logger *log.Logger) *ExpiryWorker {
	if logger == nil {
		logger = log.Default()
	}
	return &ExpiryWorker{db: db, interval: interval, logger: logger, done: make(chan struct{})}
}

func (w *ExpiryWorker) Start(ctx context.Context) {
	go w.loop(ctx)
}

func (w *ExpiryWorker) Wait() { <-w.done }

func (w *ExpiryWorker) loop(ctx context.Context) {
	defer close(w.done)
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		expired, err := w.db.ExpireApprovals(ctx, time.Now().UTC())
		if err != nil {
			w.logger.Printf("approval: expire sweep: %v", err)
		} else if expired > 0 {
			metrics.Inc(metrics.ApprovalExpired)
			w.logger.Printf("approval: expired %d pending approvals", expired)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
