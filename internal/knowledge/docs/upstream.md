# 上游账号与分组

## 调度模型
sub2api 把上游账号组织成分组，请求按分组在可调度账号之间分配。上游返回限流或错误时，sub2api 会自动把账号设为临时不可调度（TempUnschedulable），一段时间后恢复；这是 sub2api 自身的退避，不等于账号失效。

`sub2api_group_accounts{state="available"}` 为 0 而 `state="total"` 大于 0 时触发 `Sub2APIGroupNoAvailableAccount`：该分组的请求全部无法调度。一个分组长期只有 0 个可调度账号属于容量配置问题，需要人工补充账号或调整分组，不是 Agent 能修复的故障。

## 错误类型怎么区分
- **429 / rate_limit_exceeded**：上游限流或额度用尽。所有账号同时 429 说明是上游整体限流或额度问题，隔离单个账号没有帮助，应退避或扩充额度。
- **401 / 403**：账号凭据失效或被上游封禁，只影响该账号。
- **上游 5xx、超时**：上游服务故障，通常多个账号同时出现。
- 错误集中在一两个账号、同分组其他账号正常：单账号问题，才考虑隔离。

## 上游隔离动作（upstream_quarantine）
隔离是调用 `POST /api/v1/admin/accounts/:id/schedulable` 把账号设为不可调度。前提：错误集中在明确的账号；同分组其他账号正常；sub2api 的自动临时停调度没有覆盖或已失效；隔离后同分组可用账号不低于规则的 `min_available_accounts`。执行前保存原调度状态；验证同分组的上游错误率、请求成功率回落，无效时只有当前状态仍等于本次写入的值才恢复原状态（补偿）。

证据中的 `upstream_accounts` 段给出每个账号的 GroupID、Available、TempUnschedulable、最近 5 分钟错误数；`Sampled=true` 表示按账号的计数来自截断的分页，会偏少。
