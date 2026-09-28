# 单执行者：MySQL 命名锁与租约

> 所属：[亮点四 · 可信执行引擎](README.md)

## 一句话

服务启动时，先在一条**专用数据库连接**上执行 `GET_LOCK('oncall-agent-executor', 0)`，拿不到就启动失败。之后每秒检查一次「锁还是不是我的」，丢了就停掉整个进程。这样全局只有一个执行器，启动恢复和认领逻辑都可以基于这个前提来写。

## 先弄懂：MySQL 命名锁

MySQL 提供了一组「按名字加锁」的函数，和表、行都无关：

| 函数 | 作用 |
|---|---|
| `GET_LOCK(name, timeout)` | 拿锁。返回 1 表示成功，0 表示超时（被别人占着） |
| `RELEASE_LOCK(name)` | 释放锁 |
| `IS_USED_LOCK(name)` | 返回当前持有这把锁的**连接 ID**，没人持有则返回 NULL |

关键特性：**锁属于连接**。连接断开（进程崩溃、网络中断），锁自动释放。不会出现「持有者已经死了，锁还占着」的问题，也就不需要像 Redis 分布式锁那样设置过期时间、续期。

## 拿锁：必须用专用连接

```go
// 摘自 internal/store/remediation.go（有删减）
// 在一条专用连接上持有 MySQL 命名锁，直到进程结束。
// 启动恢复和认领都假设只有一个活跃的执行器；第二个实例会启动失败，而不是和第一个抢
func (db *DB) AcquireExecutorLock(ctx context.Context, name string) (release func(), err error) {
	sqlDB, _ := db.DB.DB()
	conn, err := sqlDB.Conn(ctx) // 从连接池里「借出」一条连接，独占使用
	if err != nil {
		return nil, err
	}
	var got sql.NullInt64
	// 超时为 0：拿不到立即返回，不等待
	if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, 0)", name).Scan(&got); err != nil {
		conn.Close()
		return nil, err
	}
	if !got.Valid || got.Int64 != 1 {
		conn.Close()
		return nil, errors.New("store: another executor instance holds the execution lock")
	}
	db.executionLease = &executionLease{conn: conn, name: name}
	return func() {
		conn.ExecContext(context.Background(), "SELECT RELEASE_LOCK(?)", name)
		conn.Close()
	}, nil
}
```

**为什么必须用专用连接？** Go 的 `database/sql` 有连接池，每次查询可能用池里的不同连接。如果在一条池连接上拿锁，这条连接用完会被放回池里，锁就「挂」在了一条谁都可能用到的连接上；连接被池回收时，锁也会悄悄释放。`sqlDB.Conn()` 借出的连接不会被别人复用，它的生命周期就是锁的生命周期。

## 租约检查：锁丢了就停

```go
// 摘自 internal/store/execution_lease.go
// 必须用拿锁的那条连接检查。连接丢了就是权力丢了，
// 即使连接池里的普通连接能重新连上也不算数
func (db *DB) CheckExecutionLease(ctx context.Context) error {
	if db.executionLease == nil {
		return errors.New("store: execution lock was not acquired")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var held sql.NullBool
	err := db.executionLease.conn.QueryRowContext(ctx,
		"SELECT IS_USED_LOCK(?) = CONNECTION_ID()", db.executionLease.name).Scan(&held)
	if err != nil || !held.Valid || !held.Bool {
		return errors.New("store: execution lock connection lost; restart required")
	}
	return nil
}
```

`IS_USED_LOCK(name) = CONNECTION_ID()` 的意思是：「持有这把锁的连接，是不是我现在用的这条？」

主进程每秒检查一次：

```go
// 摘自 cmd/server/main.go（有删减）
releaseExecutorLock, err := db.AcquireExecutorLock(ctx, "oncall-agent-executor")
if err != nil {
	return err // 另一个实例在跑：直接退出
}
defer releaseExecutorLock()
go func() {
	ticker := time.NewTicker(time.Second)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := db.CheckExecutionLease(ctx); err != nil && ctx.Err() == nil {
				leaseFailure <- err
				stop() // 锁丢了：停掉整个进程（所有 worker 优雅退出）
				return
			}
		}
	}
}()
```

执行器在两个关键位置还会再检查一次租约：每轮认领之前，以及**真正调用动作之前**。执行前的这次检查尤其重要：认领和执行之间，锁可能恰好丢了；锁丢了意味着另一个实例可能已经启动，此时绝不能写。

## 为什么启动顺序很重要

```go
// cmd/server/main.go 的启动顺序（简化）
db.CheckExecutionReady(ctx)                 // 1. 检查表结构和数据升级是否完成
releaseExecutorLock := AcquireExecutorLock  // 2. 先拿锁，再启动任何 worker
ingest.Worker.Start                         // 3. 各个 worker
approval.Executor.Start                     //    其中执行器启动时会对账上次中断的执行
diagnose.VerificationWorker.Start
...
server.Start                                // 4. 最后才打开 HTTP 监听
```

执行器启动时要处理「上次进程中断的执行」。如果还有另一个实例在跑，「中断的执行」可能其实正在进行中，对账就会出错。所以**拿锁必须在所有 worker 启动之前**；释放锁必须在所有 worker 退出之后（`defer` 的顺序保证了这一点）。

## 常见追问

- **这不是单点吗？进程挂了怎么办？** 是单点，而且是有意为之。执行器挂了，最坏结果是「暂时不执行」，告警照样落库，人照样收到通知。多个执行器并发写生产，代价要大得多。进程重启后会自动拿锁、对账、继续工作。
- **会不会两个实例都认为自己拿到了锁？** 不会。`GET_LOCK` 由 MySQL 保证互斥。网络分区时，旧实例的连接会断，它的锁由 MySQL 释放；旧实例在下一秒的租约检查中发现锁丢了，就会自行停止。在这一秒的窗口里，执行前还有一次检查兜底。
- **为什么其他 worker（摄入、诊断）不需要这把锁？** 它们也受这把锁保护：整个 server 进程都在拿到锁之后才启动。只是执行器对「只有一个」的依赖最强，所以在执行路径上额外做了检查。
