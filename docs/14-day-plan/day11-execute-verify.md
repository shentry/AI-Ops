# Day11：Docker Runtime Adapter、L2 执行和 Verify

> 阶段：P2 权限审批  
> 状态：已完成
> 依赖：Day10  
> 详细实现记录：[Day11 实现文档](../day11-implementation.md)

## 当日目标

在 Sub2API Docker Compose 测试环境中完成一个受控低风险动作，并以独立 Verify 判定恢复结果。

## 实现清单

- [x] 定义平台无关的 `RuntimeAdapter` 接口（以 Registry 工具契约为边界，docker_restart 是首个实现）；
- [x] 实现 Docker Adapter 的容器查询、日志读取和受控重启；
- [x] target 必须映射到配置允许的 Sub2API 服务（`tools.docker.allowed_containers` 白名单）；
- [x] 执行器只从 Tool Registry 取动作，不接受任意 shell；
- [x] 执行前再次校验 approval、plan_hash、target、参数和资源状态；
- [x] 动作设置超时、幂等键和输出上限（Registry.Execute 统一纪律；领取即 executing 即幂等键）；
- [x] 将 approved 状态领取为执行中，完成后写回 executed/failed 和 result_json；
- [x] 记录 `fault_cmd_history` 的基础结果；
- [x] 实现 Verify：health、告警状态、5xx/延迟和副作用检查（V1 定死 last_alert 复查，其余经证据段间接覆盖）；
- [x] Verify 结果写入 `agent_run_step(kind=verify)`。

## 关键文件

- `internal/tools/docker.go`
- `internal/tools/runtime.go`
- `internal/approval/executor.go`
- `internal/diagnose/verify.go`
- `internal/store/store.go`
- `docker-compose.dev.yml`

## 验收清单

- [x] 审批通过后只重启目标 Sub2API 容器；
- [x] 不存在或不允许的 target 被拒绝；
- [x] 重复领取同一 approval 不产生两次重启；
- [x] 执行结果写回 approval，失败不静默；
- [x] `simulate -resolved` 可演示 Verify 成功；
- [x] 不发送 resolved 或健康检查失败时 Verify 判定失败；
- [x] Docker Socket 访问凭据不进入 LLM Prompt（socket 路径只在配置，工具参数只含容器名）；
- [x] `go test ./... && go build ./... && go vet ./...` 通过。

## 不包含

- Systemd/Kubernetes adapter；
- 数据库结构变更动作；
- 自动重试策略。
