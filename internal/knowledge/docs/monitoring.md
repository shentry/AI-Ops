# 监控与观测的已知限制

## 告警分层
告警的 `layer` 标签分三层：`service` 是用户能感知的（健康、业务成功率、延迟）；`locate` 用来定位（容器、宿主机、数据库、Redis、上游）；`monitoring` 表示"看不见了"（抓取失败、ops 读取失败、队列滞后、通知失败），它从不表示 sub2api 已经故障或已经恢复。`layer=monitoring` 的告警同时直接通知人工，因为出问题的可能正是 Agent 自己。

## exporter 的 up 与服务的 up
`up{job=...} == 0` 说明 Prometheus 抓不到这个 exporter，不说明被监控的服务宕机。`pg_up`、`redis_up` 是 exporter 连依赖的结果，exporter 自身故障时也会为 0。`sub2api_ops_up=0` 表示业务数据读不到，此时错误率为空不等于没有错误。观测不完整时，证据状态为 error/partial/missing，不能当作健康，也不能当作已确认故障。

## 容器指标依赖 cAdvisor
容器的重启、OOM、CPU、内存指标来自 cAdvisor，`Sub2APIContainerRestarting` 和 `Sub2APIContainerOOM` 依赖它。Docker 29 默认使用 containerd 镜像存储，cAdvisor v0.54 之前的版本在这种存储下认不出任何容器（日志 `failed to identify the read-write layer ID`），所有 `container_*{name="..."}` 指标缺失，这两个告警实际失效。Docker 升级后要确认 `count(container_start_time_seconds{name!=""})` 大于 0。

## 日志采集范围与保留
Loki 只收 sub2api 的 Compose 项目、监控栈项目的容器日志和 Agent 的 journald 日志，保留 7 天；`loki_query` 能查到的就是这个范围。`docker_logs` 只包含当前容器实例的日志，容器重建或重启前的日志要用 `loki_query` 查。Alloy 首次启动会补读容器已有的全部日志，超过保留期的行在 Alloy 内丢弃，不计入 `LogPipelineDropping`。

## Agent 自身的告警
- `OncallAgentQueueLag`：诊断、验证、通知队列超过处理预算。没有配置诊断模型时，诊断 worker 不启动，待诊断的任务会一直积压，这个告警会持续触发；
- `OncallAgentStateStoreUnavailable`：Agent 读不到状态库，诊断和恢复写入必须停止；
- `AlertmanagerNotificationsFailing`：Alertmanager 发不出通知，常见于人工通知地址或心跳地址没有配置或不可达；
- `Watchdog` 始终触发，用于外部心跳（dead man's switch），不是故障。
