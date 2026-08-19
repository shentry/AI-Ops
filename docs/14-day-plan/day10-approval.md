# Day10：Policy、权限分级与审批生命周期

> 阶段：P2 权限审批  
> 状态：已完成
> 依赖：Day09  
> 详细实现记录：[Day10 实现文档](../day10-implementation.md)

## 当日目标

将诊断 Plan 转换为确定性的安全决策，并完成 L3 审批单从创建到过期的生命周期。

## 实现清单

- [x] 定义 L1/L2/L3/L4 动作安全等级和动作注册表；
- [x] L1 只读动作直接允许；
- [x] L2 仅在白名单、target、限频、开关和 verify 条件全部满足时允许自动路径（开关不满足时降级审批；限频与 verify 由 D11 执行层落地）；
- [x] L3 统一创建 `approval(status=pending)`；
- [x] L4 注册即拒绝，审批不能解除拒绝；
- [x] 审批绑定 `incident_id`、`run_id`、tool、args、reason、plan_hash 和过期时间；
- [x] 实现 approve/deny HTTP API 和 Bearer 鉴权；
- [x] 重复 approve/deny 保持幂等，已决状态返回冲突；
- [x] 实现主动过期 worker；
- [x] 审批卡片不包含完整密钥、DSN 或敏感请求正文；
- [x] 审批通过后只允许执行原始 plan，不允许修改参数绕过 Guard。

## 关键文件

- `internal/approval/service.go`
- `internal/approval/worker.go`
- `internal/api/approval.go`
- `internal/tools/registry.go`
- `internal/diagnose/guard.go`
- `migrations/001_init.sql`

## 验收清单

- [x] L1 自动路径不创建审批单；
- [x] L3 Plan 创建 pending 审批单并发送通知（卡片含审批单号与 approve/deny curl）；
- [x] 无 Token 返回 401；
- [x] 重复 approve 返回 409 且不产生第二次决策；
- [x] deny 和 expired 均不能进入执行（ValidateExecution 拒绝非 approved 与过期单）；
- [x] 篡改 plan_hash、target 或 args 时执行前被拒绝；
- [x] L4 任意动作被硬规则拒绝；
- [x] `go test ./... && go build ./... && go vet ./...` 通过。

## 不包含

- 真实 Docker 变更；
- Verify 和失败重诊；
- 记忆写入。
