package diagnose

import (
	"context"
	"log"
	"time"

	"oncall-agent/internal/metrics"
	"oncall-agent/internal/store"
)

// 诊断 worker 的轮询间隔与 running 超时阈值。
// agent_run 没有像 webhook 那样的 Notify 通道 —— D05 落队列时进程内
// 未必有 worker（配置缺 LLM 时不启动），所以诊断 worker 纯轮询，
// 不引入第二套唤醒机制。
const (
	pollInterval      = time.Second
	runningStaleAfter = 5 * time.Minute
)

// runQueue 是诊断 worker 对存储层的收窄接口。
type runQueue interface {
	NextPendingAgentRun(ctx context.Context) (store.AgentRun, bool, error)
	ClaimAgentRun(ctx context.Context, id uint64, startedAt time.Time) (bool, error)
	RequeueStaleAgentRuns(ctx context.Context, staleBefore time.Time) (int64, error)
}

// pipelineRunner 是诊断 worker 对流水线的收窄接口，单测换假实现。
type pipelineRunner interface {
	Run(ctx context.Context, run store.AgentRun) error
}

// Worker 是独立诊断 worker（GC-07）：与摄入 worker 分属两个 goroutine，
// LLM 慢、Prometheus 挂、通知超时都不会拖住告警摄入。
type Worker struct {
	db       runQueue
	pipeline pipelineRunner
	logger   *log.Logger
	done     chan struct{}
}

func NewWorker(db runQueue, pipeline pipelineRunner, logger *log.Logger) *Worker {
	if logger == nil {
		logger = log.Default()
	}
	return &Worker{db: db, pipeline: pipeline, logger: logger, done: make(chan struct{})}
}

// Start 先做启动对账：把上次进程死在中途留下的超时 running 放回 pending。
// 探不通数据库就在 HTTP 监听打开前失败（和摄入 worker 同一原则）。
func (w *Worker) Start(ctx context.Context) error {
	if _, _, err := w.db.NextPendingAgentRun(ctx); err != nil {
		return err
	}
	// Reconcile stale runs before the goroutine starts so a restart does not
	// wait for the first poll cycle (and startup still fails on DB errors).
	staleBefore := time.Now().UTC().Add(-runningStaleAfter)
	requeued, err := w.db.RequeueStaleAgentRuns(ctx, staleBefore)
	if err != nil {
		return err
	}
	if requeued > 0 {
		w.logger.Printf("diagnose: requeued %d stale running runs", requeued)
	}
	go w.consume(ctx)
	return nil
}

// Wait 等消费 goroutine 退出。
func (w *Worker) Wait() { <-w.done }

func (w *Worker) consume(ctx context.Context) {
	defer close(w.done)
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		w.drain(ctx)
		w.requeueStale(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// drain 一次排空 pending 队列。中途出错直接返回，等下一轮 ticker。
func (w *Worker) drain(ctx context.Context) {
	for {
		run, found, err := w.db.NextPendingAgentRun(ctx)
		if err != nil {
			w.logger.Printf("diagnose: poll agent_run: %v", err)
			return
		}
		if !found {
			return
		}
		claimed, err := w.db.ClaimAgentRun(ctx, run.ID, time.Now().UTC())
		if err != nil {
			w.logger.Printf("diagnose: claim run %d: %v", run.ID, err)
			return
		}
		if !claimed {
			continue // 被并发消费者抢先，取下一个
		}
		if err := w.pipeline.Run(ctx, run); err != nil {
			// run 已在 pipeline 内标 failed（除非标失败本身失败）。
			// 单个 run 失败不阻断队列 —— 进程不死，继续下一条。
			metrics.Inc(metrics.AgentRunFailed)
			w.logger.Printf("diagnose: run %d failed: %v", run.ID, err)
		} else {
			metrics.Inc(metrics.AgentRunSucceeded)
			w.logger.Printf("diagnose: run %d succeeded (incident %d)", run.ID, run.IncidentID)
		}
	}
}

// requeueStale 把超时 running 放回 pending。进程死在中途的 run 靠它补账。
func (w *Worker) requeueStale(ctx context.Context) {
	staleBefore := time.Now().UTC().Add(-runningStaleAfter)
	requeued, err := w.db.RequeueStaleAgentRuns(ctx, staleBefore)
	if err != nil {
		w.logger.Printf("diagnose: requeue stale runs: %v", err)
		return
	}
	if requeued > 0 {
		w.logger.Printf("diagnose: requeued %d stale running runs", requeued)
	}
}
