package diagnose

import (
	"context"
	"fmt"
	"time"

	"oncall-agent/internal/metrics"
	"oncall-agent/internal/notify"
	"oncall-agent/internal/store"
)

// retryStore 是 RetryScheduler 对存储层的收窄接口。
type retryStore interface {
	GetAgentRun(ctx context.Context, id uint64) (store.AgentRun, error)
	CreateRetryAgentRun(ctx context.Context, run store.AgentRun, reason string) (store.AgentRun, bool, error)
	ListIncidentRunIDs(ctx context.Context, incidentID uint64) ([]uint64, error)
}

// escalationNotifier is the narrow provider-neutral notification contract.
type escalationNotifier interface {
	Send(context.Context, notify.Notification) (notify.Delivery, error)
}

// RetryScheduler implements the retry and escalation loop.

// RetryScheduler 实现 D12 的重诊闭环：Verify 失败后沿 retry_of 链
// 最多两次自动重诊，超限则升级人工。同一 incident 同时只允许一个活跃 run。
type RetryScheduler struct {
	db         retryStore
	notifier   escalationNotifier
	maxRetries int
}

func NewRetryScheduler(db retryStore, notifier escalationNotifier, maxRetries int) *RetryScheduler {
	if maxRetries < 0 {
		maxRetries = 0
	}
	return &RetryScheduler{db: db, notifier: notifier, maxRetries: maxRetries}
}

// ScheduleRetry 在一次失败的 run 之后决定下一步：还能重试就原子落
// retry_of=failedRunID 的新 pending run 及其事实事件；不能就升级人工。
// 返回 created=true 表示新 run 已入队。
func (s *RetryScheduler) ScheduleRetry(ctx context.Context, incidentID, failedRunID uint64, reason string) (bool, error) {
	// 事件、升级通知都只接受脱敏且有限长度的失败摘要。
	safeReason := TruncateForStep(Sanitize(ToSafeText(reason)))
	// 预算按 retry_of 链长算，不按 incident 总 run 数：
	// 人工手动重诊（retry_of 为空）不吃自动重试预算。
	chainLen, err := s.chainLength(ctx, failedRunID)
	if err != nil {
		return false, err
	}
	// 链长 = 当前失败的 run 在链上的位置（首诊=1）。超过 maxRetries 就升级。
	if chainLen > s.maxRetries {
		return false, s.escalate(ctx, incidentID, safeReason)
	}

	// CreateRetryAgentRun 在同一事务内锁定 incident 并检查 pending/running，
	// 将原先 HasActiveRun→CreateAgentRun 的 TOCTOU 收敛成一次原子操作。
	_, created, err := s.db.CreateRetryAgentRun(ctx, store.AgentRun{
		IncidentID: incidentID,
		Mode:       "full", // 重诊用 full 预算：light 已失败，不要再省
		Status:     "pending",
		RetryOf:    &failedRunID,
		StartedAt:  time.Now().UTC(),
	}, safeReason)
	if err != nil {
		return false, fmt.Errorf("diagnose: create retry run: %w", err)
	}
	if !created {
		return false, nil
	}
	return true, nil
}

// chainLength 沿 retry_of 往上数到首诊 run。链断裂（父 run 不存在）
// 视为到顶 —— 宁可少重试也不多重试。
func (s *RetryScheduler) chainLength(ctx context.Context, runID uint64) (int, error) {
	length := 1
	cursor := runID
	for range s.maxRetries + 2 {
		run, err := s.db.GetAgentRun(ctx, cursor)
		if err != nil {
			return length, nil // 父 run 读不到就当链断了
		}
		if run.RetryOf == nil {
			return length, nil
		}
		length++
		cursor = *run.RetryOf
	}
	return length, nil
}

// escalate 升级人工：通知带失败原因和完整 run 链。升级失败也是返回 error，
// 由调用方记日志 —— 升级丢了比 run 失败更危险，绝不能静默吞掉。
func (s *RetryScheduler) escalate(ctx context.Context, incidentID uint64, reason string) error {
	runIDs, err := s.db.ListIncidentRunIDs(ctx, incidentID)
	if err != nil {
		return err
	}
	notification := notify.Notification{
		Kind:       notify.NotificationEscalationRequired,
		IncidentID: incidentID,
		Title:      "需要人工介入",
		Summary:    reason,
		Payload:    map[string]any{"reason": reason, "run_ids": runIDs},
	}
	if _, err := s.notifier.Send(ctx, notification); err != nil {
		return fmt.Errorf("diagnose: escalate incident %d: %w", incidentID, err)
	}
	metrics.Inc(metrics.EscalationSent)
	return nil
}
