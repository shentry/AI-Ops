# 认领复验与 CAS

> 所属：[亮点四 · 可信执行引擎](README.md)

## 一句话

执行器取到一张 approved 审批后，要在一个事务里依次锁住 incident、审批单、服务，把 Policy 当初检查过的条件**全部重查一遍**。有任何一项不满足，审批单就置为 expired（作废，不换目标）；全部满足，才用条件更新（CAS）把状态改为 executing，并记下操作编号。

## 先弄懂：CAS（Compare-And-Swap）

CAS 是「比较并交换」：**只有当前值等于预期值时，才更新成新值**。在 SQL 里就是带条件的 UPDATE：

```sql
UPDATE approval SET status = 'executing' WHERE id = 42 AND status = 'approved';
-- 影响行数 = 1：我抢到了
-- 影响行数 = 0：别人先改了，我放弃
```

它比「先读、再判断、再写」安全：判断和写入在数据库里一步完成，中间不会被别人插队。

## 为什么批准之后还要再查一遍

从 Policy 冻结快照、人批准，到执行器认领，中间可能隔了几分钟甚至半小时。这段时间里：

| 可能的变化 | 如果不查 |
|---|---|
| 审批过期了 | 执行一个早已过时的决定 |
| 规则改了、降级为 observe、动作定义升级了 | 按旧规则执行 |
| 告警已经恢复 | 对一个已经好了的服务执行重启 |
| incident 里来了新告警 | 批准的故障范围已经变了 |
| 有人按下了急停 | 无视急停 |
| 同一服务上另一个动作正在执行 | 两个动作叠加 |
| 规则预算已经被别的 incident 用完 | 超出次数上限 |
| 进入了维护窗口 | 和人工维护冲突 |
| 规则被阻断（最近执行失败或验证失败） | 重复一个已知有问题的动作 |

## 认领的实现

```go
// 摘自 internal/store/execution.go（有删减）
func (db *DB) ClaimApprovalExecution(ctx context.Context, id uint64, now time.Time, policy RemediationPolicy) (row Approval, claimed bool, err error) {
	err = db.Transaction(func(tx *gorm.DB) error {
		// 加锁顺序固定：先锁 incident，再锁审批单（所有路径都这个顺序，避免死锁）
		row, parent, err := lockApproval(ctx, tx, id)
		if row.Status != "approved" {
			return nil // 已经被处理了
		}
		reason, err := claimRefusal(ctx, tx, row, parent, now, policy) // 全部条件重查
		if err != nil {
			return err
		}
		if reason != "" {
			// 有问题：作废，并记录原因
			tx.Model(&Approval{}).Where("id = ? AND status = ?", id, "approved").Update("status", "expired")
			return appendApprovalEvent(ctx, tx, row, "approval.expired", "expired", reason, now)
		}
		operation := fmt.Sprintf("op-%d", row.ID)
		// CAS：只有仍是 approved 才改为 executing
		result := tx.Model(&Approval{}).Where("id = ? AND status = ?", id, "approved").
			Updates(map[string]any{"status": "executing", "operation_id": operation, "operation_started_at": now})
		if result.RowsAffected != 1 {
			return nil // 被别人抢先
		}
		appendApprovalEvent(ctx, tx, row, "execution.started", "executing", "approval execution started as "+operation, now)
		claimed = true
		return nil
	}, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	return row, claimed, err
}
```

## 重查了什么

```go
// 摘自 internal/store/execution.go 的 claimRefusal（有删减）
// 在 incident、审批单、服务三把锁之下，把 Policy 冻结快照时检查过的条件全部重查。
// 返回非空原因，审批单就作废：旧快照永远不会被换成新目标
func claimRefusal(ctx context.Context, tx *gorm.DB, row Approval, parent Incident, now time.Time, policy RemediationPolicy) (string, error) {
	if !row.ExpiresAt.After(now) {
		return "approval expired", nil
	}
	snapshot, parseErr := ParseExecutionContext(row.ExecutionContext)
	hash, hashErr := PlanHash(row.ToolName, row.ArgsJSON, row.ExecutionContext)
	if parseErr != nil || hashErr != nil || hash != row.PlanHash {
		return "approval execution snapshot is invalid", nil // 内容被改过
	}
	if err := snapshot.ValidateBinding(row.ToolName, policy.Binding); err != nil {
		return err.Error(), nil // 服务、规则版本、动作版本变了
	}
	primary := snapshot.Kind == KindPrimary
	if primary && parent.Status != StatusFiring {
		return "incident is no longer firing", nil // 故障已经恢复
	}
	members, _ := listIncidentExecutionMembers(ctx, tx, parent.ID)
	if err := snapshot.ValidateMembers(members, true); err != nil {
		return err.Error(), nil // 故障范围变了
	}
	lockService(ctx, tx, snapshot.Service, now) // 锁住服务：同一服务的认领串行
	state, _ := remediationState(ctx, tx, /* 服务、规则、incident、时间窗 */)
	switch {
	case state.Stopped:
		return "emergency stop is active: " + state.StopReason, nil
	case state.BusyWith != 0:
		return fmt.Sprintf("service %s is busy with approval %d", snapshot.Service, state.BusyWith), nil
	case !primary:
		return "", nil // 补偿动作：不受以下限制（它本身就是为了撤销）
	case policy.Maintenance != "":
		return "maintenance window: " + policy.Maintenance, nil
	case budget.Max < 1 || state.Executions >= budget.Max:
		return "rule budget exhausted", nil
	case snapshot.Rule.Mode == ModeAuto && state.Blocked != "":
		return "rule is blocked: " + state.Blocked, nil
	case snapshot.Rule.Mode == ModeAuto && state.IncidentActions > 0:
		return "a primary action already ran in this incident; a new action needs a new authorization", nil
	}
	return "", nil
}
```

**服务锁**是 `service_lock` 表里每个服务一行，认领时 `SELECT … FOR UPDATE` 锁住它。这样同一服务的两个审批不可能同时通过「服务是否正忙」的检查。

**「服务正忙」**指同一服务上有审批正在执行、有已经排队的补偿动作，或者有恢复验证正在进行中。一个动作从执行到验证结束（包括失败后的补偿），整个过程中不会有第二个动作插进来。

## 补偿动作为什么限制更少

补偿动作是「撤销上一个动作」，比如隔离上游账号失败后恢复调度。它的授权在主动作批准时就一起冻结了。验证失败时，维护窗口、预算、规则阻断都不应该妨碍撤销，否则会把系统卡在「做了一半」的状态。但急停和服务忙仍然会拦住它。

## 认领之后、执行之前：再验一次

```go
// 摘自 internal/approval/executor.go（有删减）
func (e *Executor) execute(ctx context.Context, row store.Approval) store.ExecutionCompletion {
	action, op, err := e.operation(row) // 再算一次 hash、再验一次版本绑定
	if err != nil {
		// 认领时刚在锁内验过同样的内容；走到这里说明这一行在我们脚下被改了。什么都没写
		return e.completion(row, "aborted", /* … */)
	}
	snapshot, _ := incident.ParseExecutionContext(row.ExecutionContext)
	if snapshot.Kind == incident.KindPrimary && snapshot.Rule.Mode == incident.ModeAuto {
		// 自动执行要求此刻能读到业务流量：看不到业务状态，就不自动动手
		if _, err := tools.ReadBusinessTraffic(ctx, e.registry); err != nil {
			return e.completion(row, "aborted", /* "automatic execution requires current business metrics" */)
		}
	}
	if err := e.db.CheckExecutionLease(ctx); err != nil {
		return e.completion(row, "aborted", /* 锁丢了：绝不写 */)
	}
	runCtx, cancel := context.WithTimeout(ctx, action.Definition().Timeout)
	defer cancel()
	receipt, execErr := action.Execute(runCtx, op) // 真正的写操作（下一篇）
	// ……
}
```

## 常见追问

- **为什么作废而不是「修正后执行」？** 人批准的是冻结快照里的那一份内容。现场变了，就该重新诊断、重新授权，而不是让执行器自己改目标。执行器没有决策权，只有执行权。
- **认领事务为什么用 READ COMMITTED？** 这里的正确性靠行锁（FOR UPDATE）保证，不靠快照读。READ COMMITTED 下，锁住之后读到的是最新提交的数据，也能减少间隙锁带来的死锁。
- **同一张审批会被认领两次吗？** 不会。认领有行锁和 CAS 双重保护，而且全局只有一个执行器。
