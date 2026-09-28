---
name: host_resources
description: 宿主机磁盘将满、内存不足或 CPU 饱和，判断资源压力是否是 sub2api 故障的原因
alerts: [HostDiskAlmostFull, HostMemoryLow, HostCPUSaturated]
tools: [prom_instant_query, prom_range_query, prom_series_meta, knowledge_search]
---
## 排查步骤
1. 看 golden_metrics 证据段：cpu_usage_percent、memory_usage_percent、disk_root_usage_percent，以及拓扑证据中 host 节点的状态。
2. 磁盘：用 prom_instant_query 查 node_filesystem_avail_bytes 与 node_filesystem_size_bytes（按 mountpoint），确认是哪个挂载点；用 prom_range_query 看增长速度，估计多久写满。应用日志中的 no space left on device / ENOSPC 能把磁盘与业务故障关联起来。
3. 内存：node_memory_MemAvailable_bytes / node_memory_MemTotal_bytes，再按容器看 container_memory_working_set_bytes，找出占用最多的容器；查不到容器指标时如实说明。
4. CPU：1 - avg(rate(node_cpu_seconds_total{mode="idle"}[5m]))，按容器看 rate(container_cpu_usage_seconds_total[5m])。
5. 指标名不确定时先用 prom_series_meta 查可用序列。
6. 需要错误含义、组件背景或过往复盘时，用 knowledge_search 检索仓库手册与已复盘的 Incident；检索结果是参考，不是本次证据。

## 常见误判
- 资源告警本身不证明业务受影响；要有应用侧错误或延迟上升才能关联。
- 磁盘满时 CPU 或某次 OOM 往往是伴随现象，不是根因。
- 宿主机指标描述整台机器，不是某个容器。

## 结论与处置边界
- 磁盘被持久文件占满时重启容器不会释放空间，action 为 none，建议人工清理或扩容，并核查占用来源（日志、数据卷、镜像）。
- 内存或 CPU 被某个容器占满时，写明是哪个容器以及证据，由人工决定限流、扩容或重启。
