# Incident 归并与生命周期

> 所属：[亮点一 · 告警摄入与 Incident 归并](README.md)

## 一句话

同一个分组键、在时间窗内仍然活跃的告警，收进同一个 Incident。成员数够了才升级为 firing，升级的那一刻在同一事务里排队诊断；所有成员都恢复后，自动关单。

## 先弄懂：为什么要有 Incident

告警是信号，Incident 是故障。一次 sub2api 宕机可能同时产生 Down、错误率高、延迟高三条告警。如果以告警为单位处理，同一次故障会被诊断三次、处置三次，甚至重启三次。**之后所有的诊断、审批、执行、验证，都以 Incident 为单位。**

## 状态机

```text
     一条新告警，找不到可归并的 incident
                    │
                    ▼
              ┌───────────┐   成员数 ≥ min_alerts   ┌────────┐
              │ candidate │ ──────────────────────▶ │ firing │ ◀── 只在这一刻排队诊断
              └───────────┘                         └────────┘
                    │                                    │
                    └────── 所有成员都 resolved ──────────┤
                                                         ▼
                                                   ┌──────────┐
                                                   │ resolved │
                                                   └──────────┘

   acknowledged：人工接管。自动流程不再修改它
```

- **candidate（候选）**：一条孤立的告警可能只是抖动，先不下结论。
- **firing（确认）**：成员数达到 `min_alerts`，确认是一次故障。默认配置 `min_alerts: 1`，也就是来一条就确认；调大可以过滤抖动。
- **resolved（关单）**：所有成员都恢复了。
- **acknowledged（人工接管）**：人已经接手，自动流程不再改写它。

只有 candidate 和 firing 算「开着的」incident，才会吸收新告警、续命、被自动关单。

## 归并规则：找到或新建

```go
// 摘自 internal/store/incident.go 的 AssignIncident（有删减）
cutoff := observedAt.Add(-window) // 时间窗下界，默认 15 分钟
var row Incident
// FOR UPDATE：并发处理同一分组时，后到的事务等先到的提交，不会各建一个 incident。
// BINARY：表的排序规则大小写不敏感，不加 BINARY 的话 "DB" 和 "db" 会被当成同一个分组
query := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
	Where("BINARY group_key = BINARY ? AND status IN ? AND last_seen_at >= ?",
		input.GroupKey, []string{"candidate", "firing"}, cutoff).
	Order("last_seen_at DESC, id DESC"). // 窗口内有多个时，取最近活跃的那个
	First(&row)

if errors.Is(query.Error, gorm.ErrRecordNotFound) {
	// 没有可归并的：新建 candidate，标题为 "分组键: 告警名"
	row = Incident{GroupKey: input.GroupKey, Status: "candidate", Title: rule.Title() /* … */}
	tx.Create(&row)
}
before := State{Status: row.Status, Severity: int(row.Severity), AlertsCount: row.AlertsCount /* … */}

// 挂成员：ON CONFLICT DO NOTHING。同一指纹反复 firing 不会把成员数刷高，
// 只有真正的新成员（RowsAffected > 0）才计数
insert := tx.Clauses(clause.OnConflict{DoNothing: true}).
	Create(&IncidentAlert{IncidentID: row.ID, Fingerprint: input.Fingerprint})

after := Merge(before, rule, insert.RowsAffected > 0, minAlerts) // 纯函数，见下面
// ……把 after 写回 incident，并让 last_alert.incident_id 指向这个 incident……
return IncidentAssignment{IncidentID: row.ID, Promoted: Promoted(before, after) /* … */}
```

`last_alert.incident_id` 是一个反向指针：之后这条告警再完全重复推送，就能顺着它找到该给哪个 incident 续心跳。

## 归并时状态怎么变：三条规则写成纯函数

```go
// 摘自 internal/incident/merge.go
func Merge(current State, in MergeInput, isNewMember bool, minAlerts int) State {
	next := current
	if isNewMember {
		next.AlertsCount++
	}
	// 规则 1：severity 只升不降。否则后来的一条 info 会把 critical 故障「降级」
	if in.Severity > next.Severity {
		next.Severity = in.Severity
	}
	// 规则 2：last_seen_at 只前进不后退。积压重放时告警可能乱序到达
	if observedAt := in.ObservedAt.UTC(); observedAt.After(next.LastSeenAt) {
		next.LastSeenAt = observedAt
	}
	// 规则 3：成员数达到 minAlerts 的那一刻，candidate → firing
	if next.Status == StatusCandidate && next.AlertsCount >= minAlerts {
		next.Status = StatusFiring
	}
	return next
}

// Promoted 判断的是「状态跃迁」：进来时是 candidate、出去时是 firing。
// 不能判断成「现在是 firing」，否则每挂一个新成员都会触发一次诊断
func Promoted(before, after State) bool {
	return before.Status == StatusCandidate && after.Status == StatusFiring
}
```

**设计要点：规则和数据库分开。** `internal/incident` 包不依赖数据库，只写「当前状态 + 输入 → 新状态」的纯函数；`internal/store` 负责加锁、读当前状态、调用纯函数、写回结果。这样时间窗、只升不降、升级时机这些规则，不需要 MySQL 就能用普通单元测试覆盖。

## 升级的那一刻：同一事务里排队诊断

归并在一个事务回调（hook）里执行。发现 `Promoted` 之后，在**同一个事务里**写入诊断任务：

```go
// 摘自 internal/ingest/worker.go 的 hook（有删减）
if assignment.Promoted {
	tx.AppendIncidentEvent(ctx, incidentEvent(assignment.IncidentID, "incident.promoted" /* … */))
	// 按级别决定诊断力度
	mode := incident.RouteMode(assignment.Severity, w.severityRoute)
	queued := incident.NewQueueRun(assignment.IncidentID, mode, time.Now().UTC())
	run := store.AgentRun{IncidentID: queued.IncidentID, Mode: queued.Mode, Status: queued.Status /* … */}
	tx.EnqueueAgentRun(ctx, &run) // 内部走统一的诊断准入 requestRun
}
```

级别到诊断模式的路由（`diagnose.severity_route`）：

| 级别 | 模式 | 含义 |
|---|---|---|
| critical、high | full | 完整诊断，模型最多 32 步 |
| warning | light | 轻量诊断，模型最多 16 步 |
| info、low | skip | 不诊断，但仍然写一行状态为 succeeded 的记录，以便统计「这个 incident 被路由规则看过」 |
| 未配置或非法 | light | 折中：full 浪费预算，skip 可能漏诊 |

**为什么要在同一个事务里？** 如果「incident 升级」和「排队诊断」分成两个事务，进程恰好在两者之间崩溃，就会出现一个 firing 的 incident 永远没人诊断。

## 心跳与关单

- **完全重复 → 心跳**：`TouchIncident` 只推进 `last_seen_at`，级别只升不降。告警一直在响，incident 的时间窗就一直有效；**不会**产生新的诊断。
- **resolved → 关单判断**：resolved 告警不新建 incident，也不续时间窗，只更新成员状态。然后判断能不能关单：

```go
// 摘自 internal/incident/merge.go
// 还有任何一个成员没 resolved，incident 就继续开着；已关单或人工接管的不再处理
func CanResolve(status string, unresolvedMembers int64) bool {
	return IsOpen(status) && unresolvedMembers == 0
}
```

关单时追加一条 `incident.resolved` 事件。**注意：incident 的关单只由告警源决定**。系统自己的恢复验证通过，不会替告警源关单（见亮点五）。

## 常见追问

- **时间窗多长合适？** 默认 15 分钟。太短，同一次故障的告警会被拆成几个 incident；太长，隔了很久的复发会被缝进上一次故障。
- **过了时间窗再响，算新 incident 吗？** 算。只有 `last_seen_at` 在窗口内的开着的 incident 才能吸收新告警。
- **为什么只在升级那一刻排诊断？** 一个故障只该诊断一次。后续新成员、重复推送只更新 incident 本身。需要重新诊断时，可以由人手动触发，或者由验证失败自动触发，它们走的是同一个准入入口（见[下一篇](05-全链路-一条Webhook的一生.md)）。
