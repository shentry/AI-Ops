package ingest

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"oncall-agent/internal/config"
	"oncall-agent/internal/incident"
	"oncall-agent/internal/metrics"
	"oncall-agent/internal/store"
)

// 兜底轮询间隔。wake 信号错过了（进程刚重启、Notify 发生在 consume 启动前）
// 也能靠这个 ticker 把积压扫出来，不至于让 pending 行永远躺着。
const workerRetryInterval = time.Second

// pendingEventStore 只声明 worker 真正用到的三个方法，单测换假实现即可，
// 不用起 MySQL。*store.DB 天然满足这个接口。
type pendingEventStore interface {
	// 按 id 升序取最早一条 pending。队列空是正常状态，返回 found=false 而不是 error。
	NextPendingRawEvent(context.Context) (store.RawEvent, bool, error)
	// 单事务内写 alert/last_alert、跑关联 hook、把 raw_event 标 processed。
	// 事务失败则整条回滚，raw_event 留在 pending 等下一轮重试。
	ApplyRawEvent(context.Context, uint64, []store.AlertInput, time.Time, store.RawEventApplyHook) ([]store.AlertApplyResult, error)
	// 报文本身没救（格式错、缺必填字段）时标 failed，
	// 否则这条坏数据会卡在队首无限重试，后面的告警全部堵死。
	MarkRawEventFailed(context.Context, uint64, string, time.Time) error
}

// Worker 按 id 顺序消费 raw_event。channel 只是唤醒信号，
// 持久队列和顺序来源都是 MySQL —— 所以进程重启不丢事件。
type Worker struct {
	db            pendingEventStore
	cfg           config.IngestConfig
	correlator    *Correlator
	severityRoute map[string]string
	wake          chan struct{}
	retryInterval time.Duration
	logger        *log.Logger
	done          chan struct{} // consume 退出时关闭，供 Wait 阻塞等待
}

// NewWorker 是生产入口，收具体的 *store.DB；
// newWorker 收接口并允许改 retryInterval，只给单测用（秒级 ticker 会让测试很慢）。
func NewWorker(db *store.DB, ingestCfg config.IngestConfig, correlateCfg config.CorrelateConfig, severityRoute map[string]string, logger *log.Logger) *Worker {
	return newWorker(db, ingestCfg, correlateCfg, severityRoute, logger, workerRetryInterval)
}

func newWorker(db pendingEventStore, ingestCfg config.IngestConfig, correlateCfg config.CorrelateConfig, severityRoute map[string]string, logger *log.Logger, retryInterval time.Duration) *Worker {
	if logger == nil {
		logger = log.Default()
	}
	if retryInterval <= 0 {
		retryInterval = workerRetryInterval
	}
	return &Worker{
		// wake 容量 1：多次唤醒自动合并，因为每次 drain 都读到队列空才收手。
		db: db, cfg: ingestCfg, correlator: NewCorrelator(correlateCfg), severityRoute: severityRoute, wake: make(chan struct{}, 1), retryInterval: retryInterval,
		logger: logger, done: make(chan struct{}),
	}
}

// Start 先探一次 pending 队列，确认 DSN 和表结构可读；
// 探不通就在 HTTP 监听打开之前返回错误，避免对外收包却没人消费。
// 通了才起 consume，并 Notify 一次做启动对账 —— 补上一次进程没处理完的积压。
func (w *Worker) Start(ctx context.Context) error {
	if _, _, err := w.db.NextPendingRawEvent(ctx); err != nil {
		return err
	}
	go w.consume(ctx)
	w.Notify()
	return nil
}

// Notify 在 raw_event 落库之后唤醒 worker。default 分支保证非阻塞：
// 这是 HTTP handler 调用的，绝不能因为 worker 忙而拖慢请求。
// 多次唤醒合并成一次也不会漏，drain 会一直读到没有 pending 行为止。
func (w *Worker) Notify() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// Wait 等 consume 真正退出。main 在 server.Shutdown() 之后调用，
// 让在途事务收尾，而不是进程直接退出把事务掐断。
func (w *Worker) Wait() { <-w.done }

// consume 是唯一的消费 goroutine，单并发 —— raw_event 的顺序语义靠这个保证。
func (w *Worker) consume(ctx context.Context) {
	defer close(w.done)
	ticker := time.NewTicker(w.retryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.wake: // 新事件落库
		case <-ticker.C: // 兜底对账
		}
		// ctx.Err() == nil 是为了区分真错误和关机：
		// 停机时 in-flight 查询会返回 context canceled，那不是故障，不该打日志。
		if err := w.drain(ctx); err != nil && ctx.Err() == nil {
			w.logger.Printf("ingest: pending reconciliation failed: %v", err)
		}
	}
}

// drain 一次排空到队列见底。中途出错就直接返回，
// 当前这条 raw_event 仍是 pending，下一轮唤醒或 ticker 会重新取到它。
func (w *Worker) drain(ctx context.Context) error {
	for {
		event, found, err := w.db.NextPendingRawEvent(ctx)
		if err != nil {
			return err
		}
		if !found {
			return nil
		}
		if err := w.process(ctx, event); err != nil {
			return err
		}
	}
}

// process 处理单条 raw_event：解析 → 校验 → 按配置重算身份 → 落库并关联 incident。
//
// 两种失败要分清：
//   - 报文本身的问题 → reject()，标 failed，返回 nil 让 drain 继续往下走；
//   - 数据库/事务的问题 → 原样 return err，raw_event 保持 pending 等重试。
func (w *Worker) process(ctx context.Context, event store.RawEvent) error {
	alerts, err := ParseWebhook(event.Payload)
	if err != nil {
		return w.reject(ctx, event.ID, err)
	}
	// received_at 取落库时刻而不是 time.Now()：重放积压时时间线不会被压平到当前。
	receivedAt := event.CreatedAt.UTC()
	inputs := make([]store.AlertInput, 0, len(alerts))
	for index := range alerts {
		alert := &alerts[index] // 取指针，下面要就地改写身份字段
		// ParseWebhook 允许 alertname 缺失（它只做格式校验），但落库必须有名字。
		if strings.TrimSpace(alert.Name) == "" {
			return w.reject(ctx, event.ID, fmt.Errorf("ingest: alerts[%d].labels.alertname is required", index))
		}
		// alert.starts_at 是 NOT NULL，且 D04 的时间窗要靠它，零值不能入库。
		if alert.StartsAt.IsZero() {
			return w.reject(ctx, event.ID, fmt.Errorf("ingest: alerts[%d].startsAt is required", index))
		}
		// 配置了指纹字段却一个都没命中：此时指纹会退化成空串的哈希，
		// 所有这类告警会被并成同一个对象。宁可拒收，也不让无关告警互相污染。
		if len(w.cfg.FingerprintFields) > 0 && len(fingerprintKeys(alert.Labels, w.cfg.FingerprintFields)) == 0 {
			return w.reject(ctx, event.ID, fmt.Errorf("ingest: alerts[%d] has none of the configured fingerprint fields", index))
		}
		// ParseWebhook 是纯函数、不读配置，用的是默认值（全部 labels、"severity"）。
		// 这里才有 cfg，必须重算。顺序有依赖：AlertHash 含 Fingerprint 和 Severity，放最后。
		alert.ReceivedAt = receivedAt
		alert.Fingerprint = Fingerprint(alert.Labels, w.cfg.FingerprintFields)
		alert.Severity = Severity(alert.Labels, w.cfg.SeverityLabel)
		alert.AlertHash = FullHash(*alert)
		inputs = append(inputs, store.AlertInput{
			Fingerprint: alert.Fingerprint, AlertHash: alert.AlertHash, Source: alert.Source,
			Name: alert.Name, Severity: alert.Severity, Status: alert.Status,
			Labels: alert.Labels, Annotations: alert.Annotations, GeneratorURL: alert.GeneratorURL,
			StartsAt: alert.StartsAt, ReceivedAt: alert.ReceivedAt,
		})
	}
	// hook 在 store 的事务里跑，所以只能往闭包里攒结果，日志留到提交之后再打 ——
	// 事务里打了日志又回滚，就会出现"已升级"的假记录。
	promoted := make([]uint64, 0)
	resolved := make([]uint64, 0)
	hook := func(ctx context.Context, tx store.IncidentTx, result store.AlertApplyResult) error {
		// D05 resolved 传播：resolved 不新建 incident、不续时间窗，
		// 但它挂着的 incident 可能因此满足"全部成员 resolved"而关单。
		if result.Input.Status != "firing" {
			if result.Input.Status == "resolved" && result.Last.IncidentID != nil {
				closed, err := tx.ResolveIncident(ctx, *result.Last.IncidentID, result.Input.ReceivedAt)
				if err != nil {
					return err
				}
				if closed {
					resolved = append(resolved, *result.Last.IncidentID)
				}
			}
			return nil
		}
		correlationInput := CorrelationInput{
			Fingerprint: result.Input.Fingerprint,
			Name:        result.Input.Name,
			Labels:      result.Input.Labels,
			Severity:    result.Input.Severity,
			ObservedAt:  result.Input.ReceivedAt,
		}
		switch result.Dedup {
		case store.DedupFull:
			// 内容完全没变的重复推送。已挂 incident 就续一下 last_seen_at 和级别，
			// 让时间窗不至于在告警持续 firing 期间过期；还没挂就什么都不做。
			if result.Last.IncidentID == nil {
				return nil
			}
			return tx.TouchIncident(ctx, *result.Last.IncidentID, correlationInput.ObservedAt, correlationInput.Severity)
		case store.DedupNew, store.DedupPartial:
			// 新告警或内容有变化：交给 D04 关联器决定并入哪个 incident 还是新建。
			assignment, err := w.correlator.Assign(ctx, tx, correlationInput)
			if err != nil {
				return err
			}
			if assignment.Promoted {
				// D05 促发分流：在促发同一事务里按 severity route 落 agent_run。
				// skip 直接落 succeeded（可统计），full/light 落 pending 等 D09 消费。
				mode := incident.RouteMode(assignment.Severity, w.severityRoute)
				if err := tx.EnqueueAgentRun(ctx, incident.NewQueueRun(assignment.IncidentID, mode, time.Now().UTC())); err != nil {
					return err
				}
				promoted = append(promoted, assignment.IncidentID)
			}
		}
		return nil
	}
	// processed_at 用当前时间：它记录的是"什么时候处理完"，和 received_at 是两件事。
	results, err := w.db.ApplyRawEvent(ctx, event.ID, inputs, time.Now().UTC(), hook)
	if err != nil {
		return err
	}
	// 事务已提交，下面的日志才代表真实发生过的事。
	for index, result := range results {
		if result.Dedup == store.DedupFull {
			metrics.Inc(metrics.DedupFull)
			w.logger.Printf("ingest: raw event %d alert %d full duplicate", event.ID, index)
		}
	}
	for _, incidentID := range promoted {
		metrics.Inc(metrics.IncidentPromoted)
		metrics.Inc(metrics.AgentRunEnqueued)
		w.logger.Printf("ingest: incident %d promoted to firing", incidentID)
	}
	for _, incidentID := range resolved {
		metrics.Inc(metrics.IncidentResolved)
		w.logger.Printf("ingest: incident %d resolved", incidentID)
	}
	metrics.Inc(metrics.RawEventProcessed)
	return nil
}

// reject 把这条 raw_event 判死刑并落原因，返回 nil 让 drain 继续处理后面的事件。
// 只有标 failed 这一步本身也失败时才返回 error —— 那说明库有问题，不能装作处理完了。
func (w *Worker) reject(ctx context.Context, id uint64, cause error) error {
	message := safeEventError(cause)
	if err := w.db.MarkRawEventFailed(ctx, id, message, time.Now().UTC()); err != nil {
		return fmt.Errorf("%s; mark failed: %w", message, err)
	}
	metrics.Inc(metrics.RawEventFailed)
	w.logger.Printf("ingest: raw event %d rejected: %s", id, message)
	return nil
}

// safeEventError 清洗错误文本，因为它既进日志又进 raw_event.error，
// 而内容来自外部报文：换行会伪造日志行，超长会撑爆 TEXT 列。
// 截断按 rune 不按 byte，免得把一个 UTF-8 字符切成两半。
func safeEventError(err error) string {
	message := strings.NewReplacer("\r", " ", "\n", " ").Replace(err.Error())
	runes := []rune(message)
	if len(runes) > 2048 {
		message = string(runes[:2048])
	}
	return message
}
