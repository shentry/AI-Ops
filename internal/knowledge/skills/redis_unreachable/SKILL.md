---
name: redis_unreachable
description: Redis 不可达或内存接近 maxmemory，判断 Redis 状态以及对 sub2api 的影响
alerts: [Sub2APIRedisUnreachable, Sub2APIRedisMemoryHigh]
tools: [prom_instant_query, prom_range_query, docker_logs, loki_query, knowledge_search]
---
## 排查步骤
1. 看拓扑证据中 redis 节点：容器状态和 health（max(redis_up)）。redis 证据段有直连 PING 时，看 PING 是否成功、延迟、used_memory 与 connected_clients。
2. 内存：redis_memory_used_bytes 与 redis_memory_max_bytes（为 0 表示未设置上限，此时不存在"接近上限"）；用 prom_range_query 看增长趋势。
3. 用 docker_logs 或 loki_query(service=sub2api) 看应用侧：connection refused、i/o timeout、NOAUTH、OOM command not allowed when used memory > 'maxmemory'。
4. 用 loki_query(service=redis) 看 Redis 自身日志：是否重启、持久化失败（MISCONF）、被 OOM kill。
5. 需要错误含义、组件背景或过往复盘时，用 knowledge_search 检索仓库手册与已复盘的 Incident；检索结果是参考，不是本次证据。

## 常见误判
- 应用报连接拒绝、独立 PING 也被拒绝，只能说明 Redis 端点不可达；没有 Redis 进程或主机证据时，不能确定是进程退出、防火墙还是配置问题。
- redis_up 来自 exporter；exporter 自身故障时它也会为 0。
- 不要凭空写出证据中没有出现的 Redis 容器名或地址。

## 结论与处置边界
- Redis 不可用时重启 sub2api 无效，action 为 none，由人工恢复 Redis。
- 内存接近上限时，建议人工核查淘汰策略与大 key，不自动清理数据。
