# 全链路：一条 Webhook 的一生

> 所属：[亮点一 · 告警摄入与 Incident 归并](README.md)

## 一句话

用一组具体数据，把前面四篇串起来：一条 `Sub2APIDown` 从推进来，到排上诊断任务，再到关单，数据库里每一步发生了什么。最后讲清楚诊断任务的**准入规则**，也就是什么情况下允许排一次诊断。

## 场景

sub2api 容器进程退出。配置使用默认值：`group_by: [labels.service]`、`window_minutes: 15`、`min_alerts: 1`、critical 路由到 full。

## 第 1 次推送（10:00:00）：firing

Alertmanager 推送：

```json
{"version":"4","status":"firing","alerts":[{"status":"firing",
  "labels":{"alertname":"Sub2APIDown","service":"sub2api","severity":"critical"},
  "startsAt":"2026-09-26T10:00:00Z"}]}
```

| 步骤 | 发生了什么 | 数据库变化 |
|---|---|---|
| ① HTTP | 机器 token 鉴权通过，1 个告警未超限，写入报文 | `raw_event#101` status=pending；返回 202 |
| ② 唤醒 | `Notify()` 唤醒 ingest worker，取到 #101 | — |
| ③ 解析 | 算出指纹 `fp1`、内容哈希 `h1`、级别 5、分组键 `sub2api` | — |
| ④ 开启事务 | 锁住 `raw_event#101`（仍是 pending） | — |
| ⑤ 去重 | `last_alert` 里没有 `fp1` → **New** | `alert#1`；`last_alert(fp1, firing_count=1)` |
| ⑥ 归并 | 窗口内没有 `sub2api` 的开着的 incident → 新建 candidate；挂成员；成员数 1 ≥ 1 → firing，`Promoted=true` | `incident#7` status=firing；`incident_alert(7, fp1)`；事件 `incident.created`、`incident.promoted` |
| ⑦ 排诊断 | 级别 5 → full，经过准入检查 | `agent_run#12` mode=full status=pending；事件 `run.queued` |
| ⑧ 提交 | 报文标记完成 | `raw_event#101` status=processed |

⑤ 到 ⑧ 在**同一个事务**里：任何一步失败，全部回滚，`raw_event#101` 回到 pending，下一轮重来。

接下来，诊断 worker 会在 1 秒内轮询到 `agent_run#12`，进入[亮点二](../02-证据驱动的ReAct诊断/README.md)。

## 第 2 次推送（10:04:00）：原样重复

| 步骤 | 结果 |
|---|---|
| 去重 | 指纹 `fp1` 存在，内容哈希仍是 `h1` → **Full** |
| 写库 | 不插入 alert 行，只更新 `last_alert.last_seen` |
| 归并 | 不走归并器，顺着 `last_alert.incident_id = 7` 给 `incident#7` 续心跳：`last_seen_at = 10:04` |
| 诊断 | **不会**排新的诊断 |

## 第 3 次推送（10:20:00）：resolved

| 步骤 | 结果 |
|---|---|
| 去重 | 指纹仍是 `fp1`（指纹不含 status），内容哈希变了 → **Partial** |
| 写库 | 插入 `alert#2`（resolved 版本），`last_alert` 指向它 |
| 关单判断 | resolved 告警不走归并器，直接看 `incident#7` 的成员：全部 resolved，且 incident 开着 → 关单 |
| 结果 | `incident#7` status=resolved；事件 `incident.resolved` |

## 数据库最终状态

```text
raw_event      #101 processed   #102 processed   #103 processed
alert          #1  fp1 firing   #2  fp1 resolved
last_alert     fp1 → alert#2, firing_count=2, incident_id=7
incident       #7  sub2api: Sub2APIDown   resolved   severity=5
incident_alert (7, fp1)
agent_run      #12 incident=7 mode=full
incident_event incident.created / incident.promoted / run.queued / … / incident.resolved
```

## 诊断任务的准入规则

排一次诊断有三个入口，**全部走同一个函数 `requestRun`**：

| 入口 | 触发者 | 例子 |
|---|---|---|
| `alert` | incident 升级为 firing 的那一刻 | 上面的第 ⑦ 步 |
| `manual` | 人在控制台点「重新诊断」 | 值班人员觉得结论不对 |
| `retry` | 恢复验证失败 | 重启之后业务没恢复（见亮点五） |

```go
// 摘自 internal/store/runrequest.go 的 requestRun（有删减）
parent, err := lockIncident(ctx, tx, request.IncidentID) // 先锁住 incident 行，所有入口加锁顺序一致
if parent.Status != "firing" {
	return &RunAdmissionError{Code: "incident_not_firing"} // 只诊断确认中的故障
}
if err := checkActiveProcessing(ctx, tx, request.IncidentID); err != nil {
	return err // 已经有处理在进行中
}
if request.Trigger == RunTriggerManual {
	// 人工重诊有 1 分钟冷却，防止连点
	if now.Before(lastQueued.CreatedAt.Add(time.Minute)) {
		return &RunAdmissionError{Code: "cooldown" /* … */}
	}
}
if request.Trigger == RunTriggerRetry {
	checkRetryBudget(ctx, tx, parent.ID, *request.RetryOf) // 自动重诊最多连续 2 次
}
// 通过：写 agent_run 和 run.queued 事件
```

「已经有处理在进行中」的判断（`checkActiveProcessing`）会检查三类状态，任何一个存在都拒绝：

1. 有 pending 或 running 的诊断；
2. 有 pending、approved 或 executing 的审批单；
3. 有正在进行中的恢复验证（验证之后的复发观察期不算）。

**为什么一个 incident 同时只能有一件事在处理？** 如果诊断 A 建议重启、还没执行，诊断 B 又建议回退，两个动作就可能叠加在同一个故障上，谁也说不清最后是哪个起了作用。所以同一个 incident 串行处理：一件事做完，才能开始下一件。

准入被拒绝时返回的是 `RunAdmissionError`（带原因码），而不是数据库错误。调用方据此告诉用户「为什么现在不能诊断」，比如控制台显示「冷却中，还需 40 秒」。

## 常见追问

- **准入为什么要先锁 incident？** 三个入口可能并发。都先锁同一行 incident，才能保证「检查没有进行中的处理」和「写入新任务」之间不会被别人插队，避免同时排出两个诊断。
- **跳过诊断的 incident 能手动诊断吗？** 能。人工重诊时如果路由结果是 skip，会改为 light，因为 skip 只允许告警入口使用。
