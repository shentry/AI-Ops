# Day14 实现文档：可观测性、收口与 V1 发布

> 本文对应 `docs/14-day-plan/day14-release.md`。目标：指标、文档校准、可复现演练和 v1.0 收口。

## 1. Day14 做了什么

- `internal/metrics`：进程内计数器 + `/metrics`（Prometheus 文本格式，不引 client_golang）；指标名集中在 `metrics.go` 常量；
- 埋点：webhook 接收、raw event 处理/拒绝、incident 促发/关单、agent_run 入队/成功/失败、memory hit/miss、审批创建/批准/拒绝/过期/执行成功/执行失败、verify 通过/失败、人工升级；事务内的计数只在提交后增加；
- README 重写为 V1 quickstart：三步跑通 + 全 API 一览 + 安全边界；
- `config.example.yaml` 与代码校准（approval 的 auto_execute_l2/dry_run/verify_delay_seconds、tools.docker.allowed_containers、diagnose.evidence 全段）。

## 2. 演练记录（本地 docker compose 环境）

| 场景 | 输入 | 决策/动作 | 验证 | 终态 |
|---|---|---|---|---|
| 100 条告警（60% 重复） | `simulate -n 100 -dup 0.6` | 40 唯一 fingerprint → 1 incident | alerts_count=40 | 1 个 agent_run（一次有效诊断） |
| incident 关单 | `simulate -n 40 -resolved` | 全成员 resolved → 关单 | incident resolved + incident_resolved 指标 | resolved |
| L2 自动修复（重启） | approved 审批单 + 白名单容器 | 原子领取 → 真重启 | State.StartedAt 变化 + executed + verify step | executed |
| L2 verify 失败 | 同上但成员不 resolved | verify passed=false → 重诊 run（retry_of） | retry run 创建 | executed + 重诊 pending |
| L3 审批 | API approve/deny | 幂等决策 | 重复 409、无 token 401、缺 operator 400 | approved/denied |
| L4/未注册动作 | policy 判定 | 硬拒绝 | denied，无审批单、不执行 | denied |
| 篡改 plan_hash | 改 hash 后领取 | 执行前失配 | approval=failed，未触达 Docker | failed |
| 通知失败 | 不可达 webhook | 3 次重试后记 step | run 终态仍 succeeded | succeeded |
| LLM 超时/429/空输出 | mock server | 报错不重试（非契约错误） | run failed，进程存活 | failed |
| Prometheus 停机 | 停容器 | 证据段 error、诊断降级完成 | 端点 200、无 500 | succeeded（降级） |
| 记忆命中 | 预置 high 记忆 + 同类告警 | memory_hit，0 次 LLM | tokens=0，无 llm step，hits+1 | succeeded |

Sub2API 真实故障集（/health 超时、5xx 激增、Provider 429、PG/Redis 异常、磁盘不足）需服务器环境，本仓库内未验收；对应 collector 路径（missing/error 容错）已覆盖。

## 3. 安全验收

- dry_run 默认开启（`approval.dry_run: true`）、L2 自动执行默认关闭（`auto_execute_l2: false`）；
- L4 硬拒绝、L3 未审批不执行、过期不执行、篡改拒执均有测试；
- LLM 工具面只有 L1；任意 shell 不存在；target 必须命中白名单。

## 4. 回放链

`raw_event_id → incident_id → run_id → approval_id`：raw_event 记录原文，incident 关联成员，agent_run/step 记录诊断全程，approval 关联 run 与 incident，verify 结论回落 step。任一环可从 `/api/v1/incidents/{id}` 和数据库逐级回查。

## 5. 与计划的偏差

- "至少 3 个 L2 自动修复场景"：本地只有 docker_restart 一个 L2 工具，3 场景（重启成功/失败/重诊）均以其为载体；PG 连接池、Redis 等场景需真实 Sub2API 环境；
- 自然语言聊天入口、多集群不在 V1。

```bash
go test ./...    # 含 TEST_MYSQL_DSN、TEST_PROMETHEUS_URL 集成测试
go build ./... && go vet ./...
```
