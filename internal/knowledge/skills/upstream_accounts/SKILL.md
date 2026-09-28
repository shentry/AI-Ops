---
name: upstream_accounts
description: 上游账号报错或分组没有可调度账号，区分单账号故障、上游整体限流与额度问题
alerts: [Sub2APIUpstreamAccountErrors, Sub2APIGroupNoAvailableAccount]
tools: [prom_instant_query, prom_range_query, loki_query]
---
## 排查步骤
1. 看 upstream_accounts 证据段：每个账号的 GroupID、Available、TempUnschedulable、Errors，以及 RealtimeEnabled、Sampled（为 true 时按账号的计数偏少）。
2. 判断错误分布：sub2api_account_upstream_errors_5m 是集中在一两个账号，还是同一分组、同一上游的所有账号都在报错。
3. 看分组容量：sub2api_group_accounts{state="available"} 与 {state="total"}；隔离一个账号后该分组是否还有可用账号。
4. 用 loki_query(service=sub2api, contains=状态码或 rate_limit) 区分错误类型：
   - 429 / rate_limit_exceeded：上游限流或额度用尽；
   - 401/403：账号凭据失效或被封禁；
   - 上游 5xx、超时：上游服务故障。
5. 本地健康、依赖都正常时，结论落在上游，不要归因到数据库或网关进程。

## 常见误判
- 所有账号同时报 429 是上游整体限流或额度问题，隔离单个账号没有帮助。
- 分组没有可用账号时，隔离只会让情况更糟；需要人工补充账号或调整分组。
- TempUnschedulable 是 sub2api 自身的临时退避，不等于账号失效。

## 结论与处置边界
- 错误集中在少数账号、且隔离后分组仍有足够可用账号时，upstream_quarantine 可作为建议，目标是证据中出现的账号 ID。
- 整体限流、额度用尽、分组无可用账号时，action 为 none，建议人工退避、扩充额度或补充账号。
