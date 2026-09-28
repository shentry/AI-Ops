---
name: postgres_connection
description: PostgreSQL 不可达、连接数接近上限或认证失败，判断数据库本身是否故障以及是否影响 sub2api
alerts: [Sub2APIPostgresUnreachable, Sub2APIPostgresConnectionsHigh]
tools: [prom_instant_query, prom_range_query, docker_logs, loki_query]
---
## 排查步骤
1. 看拓扑证据中 postgres 节点：容器状态（running、restart_count、OOMKilled）和 health（max(pg_up)）。postgres 证据段有直连探测时，看 SELECT 1 是否成功、total/waiting 连接数。
2. 连接数：sum(pg_stat_activity_count) 与 max(pg_settings_max_connections) 的比值，用 prom_range_query 看是缓慢上涨（疑似连接泄漏或连接池配置过大）还是随流量突增。
3. 用 docker_logs 或 loki_query(service=sub2api) 看应用侧错误：
   - password authentication failed / SQLSTATE 28P01：应用账号凭据错误；
   - SQLSTATE 53300 / sorry, too many clients already：连接槽位耗尽；
   - connection refused / no route to host：数据库端点不可达。
4. 再用 loki_query(service=postgres) 看数据库自身日志：是否重启、崩溃恢复、磁盘写满。

## 常见误判
- pg_up 是 exporter 用只读监控账号连接的结果；它为 1 不代表应用账号能认证，为 0 也可能是 exporter 自身故障（看 MonitoringTargetDown）。
- 连接数高但应用日志没有连接错误时，不能认定为根因。
- 53300 只证明槽位耗尽，不能凭它断定连接泄漏或恶意流量。

## 结论与处置边界
- 认证失败属于凭据/配置问题，连接耗尽需要查连接使用与连接池配置，数据库不可达需要人工恢复数据库：这些情况下重启 sub2api 无效，action 为 none。
- 本系统没有数据库写操作，不建议 kill 会话或修改参数，写进 plan.reason 作为人工建议。
