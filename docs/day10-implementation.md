# Day10 实现文档：Policy、权限分级与审批生命周期

> 本文对应 `oncall-agent-开发SPEC.md` 的 D10 和 `docs/14-day-plan/day10-approval.md`。目标：诊断 Plan → 确定性安全决策 + L3 审批单全生命周期。
>
> D10 不做真实变更执行（D11）、不做 Verify/重诊（D11/D12）、不写记忆（D13）。

## 1. Day10 做了什么

- `approval.Policy`：Plan.action 必须在工具注册表注册（GC-12，不在名单即拒绝）；按工具 SafetyLevel 分级决策——L1 直接允许、L2 满足 `auto_execute_l2 && !dry_run` 才走自动路径否则降级审批、L3 一律审批、L4 硬拒绝且审批不能解除（GC-10）；
- `plan_hash`（`migrations/003_approval_plan_hash.sql`）：审批单绑定 tool + 规范化 args（target kind/name）的 SHA-256，执行前重算比对（GC-13）；
- `approval.Service`：Create（pending + TTL）/ Decide（approve|deny，条件更新保证幂等）/ ValidateExecution（approved + 未过期 + hash/args/tool 全等）/ List；
- `approval.ExpiryWorker`：每分钟 sweep 过期 pending → expired（主动过期，不靠决策时顺带检查）；
- `api/approval.go`：`GET /api/v1/approvals?status=`、`POST /api/v1/approvals/{id}/approve|deny`；Bearer 鉴权 + `X-Operator` 必填（审批必须能回答"谁批的"）；已决/已过期 409、不存在 404；
- Pipeline 加第 4 阶段 policy：Guard 后的 Plan 经 Policy 决策，L3/降级 L2 落 pending 审批单并记 `kind=approval` step；审批单落不了库 → run failed（拿不到执行许可不能假装成功）；
- 通知闭环：DiagnosisMessage 增加 PolicyDecision/ApprovalID/BaseURL，审批卡片渲染 approve/deny curl 命令；
- 审批 reason 入库前经 `diagnose.Sanitize`（GC-19）。

## 2. 决策语义

| Plan.action | 注册表等级 | Policy 结论 |
|---|---|---|
| 空 / none | — | none |
| 未注册 | — | denied |
| 已注册 | L1 | auto_l1（D10 不执行，D11 执行层消费） |
| 已注册 | L2 | 开关全满足 → auto_l2；任一不满足 → approval |
| 已注册 | L3 | approval |
| 已注册 | L4 | denied（审批不可解除） |

## 3. 与 SPEC 的一处偏差

SPEC 写"curl 自带 Bearer token"。实际渲染 `Bearer ${AUTH_TOKEN}` 环境变量占位：真实凭据不进 IM 通道（GC-19 优先）。操作者复制 curl 后自行带 token 执行。

## 4. 测试覆盖

- Policy：九个分支（none/未注册/L1/L2 开关组合/L3/L4）；plan_hash 稳定性与 target 敏感性；
- Service：创建 pending + TTL；approve → 重复 409 → deny 已决 409 → 不存在 404；ValidateExecution 全等校验（篡改 hash/args/tool 全拒）、过期拒执；
- store 集成：条件更新幂等、过期窗口外拒绝、ExpireApprovals sweep（过期 pending 化、已批准不动）、列表过滤；
- API：401/400（缺 X-Operator）/400（非法 id）/405/approve/deny/409/404/503/列表过滤；
- Pipeline：L3 决策落审批单 + approval step；审批创建失败 run failed；
- Notify：审批卡片含审批单号、approve/deny curl、BaseURL 尾斜杠归一。

## 5. 真实端到端验收

对运行中的服务验收审批 API：无 token 401 → 缺 operator 400 → approve 200 → 重复 approve 409 → deny 已决 409 → 不存在 404 → 列表 status=approved 过滤命中。L3 Plan 建单 + 通知由 pipeline 单测与渲染单测覆盖（本环境无注册的 L3 工具，真实 L3 动作在 D11+ 接入）。

```bash
go test ./...    # 含 TEST_MYSQL_DSN、TEST_PROMETHEUS_URL 集成测试
go build ./... && go vet ./...
```
