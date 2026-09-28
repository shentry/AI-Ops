# PostgreSQL 与 Redis 依赖

## 监控方式
- PostgreSQL 由 postgres-exporter 采集，使用只授予 `pg_monitor` 的只读账号，连接数上限 3。`pg_up` 表示 exporter 能否用这个账号连上数据库；它为 1 不代表 sub2api 的应用账号能认证，为 0 也可能是 exporter 自身故障（同时看 `MonitoringTargetDown`）。
- 连接使用：`sum(pg_stat_activity_count) / max(pg_settings_max_connections)`，超过 0.9 持续 5 分钟触发 `Sub2APIPostgresConnectionsHigh`。
- Redis 由 redis-exporter 采集：`redis_up`、`redis_memory_used_bytes`、`redis_memory_max_bytes`（为 0 表示没有设置 maxmemory，此时不存在"接近上限"）、`redis_connected_clients`。
- 拓扑页与诊断证据中的 postgres、redis 节点：容器状态来自 docker inspect，健康来自 `max(pg_up)`、`max(redis_up)`。
- Agent 配置了直连 DSN 时，诊断证据另有直连探测（`SELECT 1` 与 pg_stat_activity 统计、Redis PING/INFO）；生产未配置时这两段证据为 missing。

## PostgreSQL 常见错误的含义
- **SQLSTATE 28P01 / password authentication failed**：应用账号凭据错误，常见于密码轮换没有同步到 sub2api 的配置。数据库本身在线。处置：人工核对并修正凭据后重新加载应用配置；重启 sub2api 不会改变结果。
- **SQLSTATE 53300 / sorry, too many clients already**：连接槽位耗尽。只凭这个错误不能断定是连接泄漏还是流量突增；要看连接数的增长曲线、连接来源和连接池配置。处置：人工排查连接占用与连接池，不在数据库上随意终止会话。
- **connection refused / no route to host**：数据库端点不可达，先看 postgres 容器是否在运行、是否在重启或被 OOM kill。
- **数据库磁盘写满**：写入失败、可能进入只读，看宿主机磁盘与数据卷。

## Redis 常见错误的含义
- **connection refused / i/o timeout**：Redis 端点不可达。应用报连接拒绝、独立 PING 也被拒绝，只能说明端点不可达；没有 Redis 进程或主机证据时，不能确定是进程退出、网络还是配置问题。
- **NOAUTH / WRONGPASS**：认证配置不一致。
- **OOM command not allowed when used memory > 'maxmemory'**：内存达到上限且淘汰策略不允许写入。
- **MISCONF**：持久化失败（常见于磁盘满），Redis 拒绝写入。

## 依赖故障时的处置边界
依赖不可用时，重启 sub2api、发布回退和上游隔离都不能恢复服务，Guard 会拒绝重启并转人工。Agent 没有数据库或 Redis 的写操作，也不清理数据；这些由人工处理，诊断只需写清楚证据和建议。
