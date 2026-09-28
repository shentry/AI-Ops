# 统一的 Worker 模型：表、轮询、认领

> 所属：[亮点六 · 持久工作流与可回放审计](README.md)

## 一句话

系统里所有异步环节都是同一个模型：**任务是表里的一行，worker 定时轮询，用条件更新认领，处理完提交结果；超时的认领被回收，放回队列**。只要理解一个 worker，就理解了全部。

## 先弄懂：为什么不用 goroutine + channel

最简单的异步做法是 HTTP handler 把任务塞进 channel，后台 goroutine 消费。问题是：

- 进程一重启，channel 里的任务全丢；
- 处理到一半崩溃，没有任何记录说明「这个任务做到哪了」；
- 多个 goroutine 或多个进程同时处理时，没有协调机制。

把任务放进表里，这三个问题都有了答案：任务持久化，状态字段记录进度，数据库的锁和条件更新负责协调。

## 七个 worker 一览

| Worker | 队列表 | 状态流转 | 触发方式 | 崩溃恢复 |
|---|---|---|---|---|
| 告警摄入 `ingest.Worker` | `raw_event` | pending → processed / failed | channel 唤醒 + 1 秒兜底 | 事务未提交就保持 pending |
| 诊断 `diagnose.Worker` | `agent_run` | pending → running → succeeded / failed | 每秒轮询 | running 超过 5 分钟重新入队 |
| 审批过期 `approval.ExpiryWorker` | `approval` | pending / approved → expired | 每分钟扫描 | 无状态，下次扫描继续 |
| 执行 `approval.Executor` | `approval` | approved → executing → executed / aborted / failed | 每秒轮询 | 启动时对账 executing（不重放） |
| 验证 `diagnose.VerificationWorker` | `verify_task` | pending → running → …（亮点五） | 每秒，处理到期任务 | 认领超过 30 秒重新入队 |
| 追问 `conversation.Worker` | `conversation_message` | queued → running → completed / failed | 每秒轮询 | 超过 5 分钟重新入队 |
| 通知 `notify.Worker` | `notification_task` | 未投递 → 已投递 | 每秒，处理到期任务 | 未投递的继续重试 |

## 一个 worker 的标准结构

以诊断 worker 为例，其他 worker 结构相同：

```go
// 摘自 internal/diagnose/worker.go（有删减）

// Start：先做预检和对账，再启动消费 goroutine
func (w *Worker) Start(ctx context.Context) error {
	// ① 预检：探一次队列。数据库或表结构不对，就在 HTTP 监听打开之前失败
	if _, _, err := w.db.NextPendingAgentRun(ctx); err != nil {
		return err
	}
	// ② 启动对账：把上次进程死在中途留下的超时 running 放回 pending
	staleBefore := time.Now().UTC().Add(-runningStaleAfter) // 5 分钟
	w.db.RequeueStaleAgentRuns(ctx, staleBefore)
	// ③ 启动消费循环
	go w.consume(ctx)
	return nil
}

func (w *Worker) consume(ctx context.Context) {
	defer close(w.done)
	ticker := time.NewTicker(time.Second)
	for {
		w.drain(ctx)        // 处理到队列为空
		w.requeueStale(ctx) // 每轮都回收一次超时认领
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (w *Worker) drain(ctx context.Context) {
	for {
		run, found, err := w.db.NextPendingAgentRun(ctx) // 取 id 最小的 pending
		if err != nil || !found {
			return
		}
		claimed, err := w.db.ClaimAgentRun(ctx, run.ID, time.Now().UTC()) // CAS 认领
		if err != nil {
			return
		}
		if !claimed {
			continue // 被别人抢先：取下一个
		}
		w.pipeline.Run(ctx, run) // 处理；单个任务失败不影响队列，进程也不退出
	}
}
```

## 认领：锁 + 条件更新

```go
// 摘自 internal/store/agentrun.go 的 ClaimAgentRun（有删减）
// 在短事务里锁定 pending 行，完成 pending → running 的 CAS，
// 并把 run.started 事件和状态变更一起提交。RowsAffected = 0 表示被抢先
err = db.Transaction(func(tx *gorm.DB) error {
	var run AgentRun
	query := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ? AND status = ?", id, "pending").First(&run)
	if errors.Is(query.Error, gorm.ErrRecordNotFound) {
		return nil // 已经不是 pending
	}
	// ……pending → running，写 run.started 事件……
})
```

「先取一个 → 再认领」分两步，是因为取的时候不加锁（便宜），认领的时候才加锁并二次确认状态（安全）。

## 租约：超时就回收

每个 worker 都有自己的「超时」定义：

| Worker | 超时判定 | 为什么是这个值 |
|---|---|---|
| 诊断 | running 超过 5 分钟 | 诊断整体有 3 分钟超时，5 分钟没结束只可能是进程死了 |
| 验证 | 认领超过 30 秒 | 单次观测超时必须小于 30 秒 |
| 追问 | 认领超过 5 分钟 | 同诊断 |

超时回收要和处理超时**配套**：处理超时必须小于回收阈值，否则一个正常运行的任务会被回收，导致重复处理。这就是诊断用 3 分钟超时的原因。

验证任务还多了一层保护：提交结果时带着认领时间做条件更新（`WHERE claimed_at = ?`）。一个已经被回收、又被重新认领的任务，旧的处理者即使完成了也写不进去。

## 优雅停机的顺序

```go
// 摘自 cmd/server/main.go（有删减）
// 每个 worker 都在依赖装配完成之后才启动。只有一条清理路径，
// 所以启动失败和正常停机一样安全
defer func() {
	stop() // 取消 context：所有 worker 开始退出
	if conversationWorker != nil { conversationWorker.Wait() }
	if diagnoseWorker != nil     { diagnoseWorker.Wait() }
	if verificationWorker != nil { verificationWorker.Wait() }
	if executor != nil           { executor.Wait() }
	if expiryWorker != nil       { expiryWorker.Wait() }
	if notificationWorker != nil { notificationWorker.Wait() }
	worker.Wait() // 摄入 worker
}()
// ……再往外一层的 defer 释放执行器锁：所有 worker 退出之后才释放
```

- `ctx.Err() != nil` 时产生的错误不记日志：停机时进行中的查询返回 `context canceled`，这是正常现象，不是故障。
- 验证 worker 收到取消信号时，**不写任何结论**，把认领留给重启后的租约恢复。停机不是一次观测。
- 执行器正在执行的动作，由动作自己的超时控制；结果写库的重试会响应取消。

## 为什么诊断不用 channel 唤醒

摄入 worker 用 channel 唤醒 + 兜底轮询，诊断 worker 却只轮询，原因写在代码注释里：诊断任务是在摄入事务里创建的，而配置缺少 LLM 时诊断 worker 根本不启动，进程内不一定有消费者。纯轮询就够了，不需要引入第二套唤醒机制。

## 常见追问

- **能不能多开几个进程提高吞吐？** 摄入、诊断的认领本身是并发安全的（CAS），但整个 server 进程受执行器锁保护，只能跑一个实例。对告警级别的负载，单实例就足够了。压测记录见 `docs/load-test-2026-09-17.md`。
- **某个任务一直处理失败，会不会卡住队列？** 不会。诊断失败会把 run 标为 failed；摄入遇到坏报文会标为 failed。失败的任务不会一直停在 pending。
