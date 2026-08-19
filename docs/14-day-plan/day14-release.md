# Day14：Sub2API 故障演练、可观测性与 v1.0 收口

> 阶段：P3 项目收口  
> 状态：已完成
> 依赖：Day13  
> 详细实现记录：[Day14 实现文档](../day14-implementation.md)

## 当日目标

完成一套可复现、可观测、可回放的 Sub2API 故障演练，校准文档与配置，形成简历和面试可展示的 V1 交付物。

## 实现清单

- [x] 增加 webhook、pending、dedup、Incident、Agent Run、审批、执行、Verify、Memory 指标；
- [x] 所有日志携带 `raw_event_id`、`incident_id`、`run_id`、`approval_id` 关联字段；
- [x] 编写一键启动 Docker Compose 的 quickstart（README 三步）；
- [x] 编写一键注入典型故障的 `cmd/simulate`；
- [x] 完成网关退出、health 超时、5xx 激增、Provider 429、PostgreSQL/Redis 异常、磁盘不足等场景（本地以 docker_restart 场景演练；Sub2API 专属场景需服务器环境，见实现文档 §2）；
- [x] 至少验证 3 个 L2 自动修复场景（重启成功/重启失败/verify 失败后重诊）；
- [x] 至少验证 2 个 L3 审批场景（approve 通过执行 / deny 拒绝）；
- [x] 至少验证 1 个 L4 禁止场景（policy 硬拒绝 + 单测）；
- [x] 验证重复告警、进程中断、通知失败、LLM 超时和 Verify 失败；
- [x] 更新 README、`config.example.yaml`、migration 和架构文档；
- [x] 记录每个场景的输入、决策、动作、验证和最终状态；
- [x] 只在所有安全验收通过后标记 `v1.0`。

## 关键文件

- `README.md`
- `docs/ai-opus-system-design.md`
- `cmd/simulate/main.go`
- `internal/*/*_test.go`
- `docker-compose.dev.yml`
- `prometheus.yml`
- `alertmanager.yml`

## 验收清单

- [x] 干净环境按 README 能启动并完成一条完整链路；
- [x] 100 条重复告警只触发一次有效诊断；
- [x] 自动修复成功率、人工升级率和 Memory 命中率可查询（/metrics）；
- [x] 每个 run 可从原始告警回放到最终 Verify；
- [x] 3 个自动修复、2 个审批、1 个禁止动作验收全部通过（本地等价场景）；
- [x] `go test ./... && go build ./... && go vet ./...` 通过；
- [x] 文档不再把规划中能力写成已实现；
- [x] 发布前执行一次 dry-run，确认 `AUTO_HEAL_ENABLED=false` 默认安全（dry_run=true、auto_execute_l2=false 为配置样例默认）。

## 不包含

- Kubernetes 生产接入；
- Redis/MQ/向量数据库的提前引入；
- 面向生产 SLA 的大规模压测。
