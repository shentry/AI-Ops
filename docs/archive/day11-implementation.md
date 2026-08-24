# Day11 实现文档：Docker Runtime Adapter、L2 执行和 Verify

> 本文对应 `oncall-agent-开发SPEC.md` 的 D11 和 `docs/14-day-plan/day11-execute-verify.md`。目标：批准的 L2 动作真实执行、结果回写、独立 Verify 判定恢复。
>
> SPEC D11 的样板工具写的是 `exec_change_sql`，plan 文档改为 Docker 受控重启（`docker_restart`）——按 plan 文档实现，SQL 变更工具未做。

## 1. Day11 做了什么

- `docker_restart`（L2 工具）：unix socket POST `/containers/{name}/restart?t=10`；名字形态 + 白名单 + 限频三重校验（白名单来自 `tools.docker.allowed_containers`，空白名单=不注册=动作不存在；限频来自 `restart_min_interval_seconds` / `restart_max_per_hour`，在发请求之前判定并记账，被拒时容器没被动过）；幂等；
- `approval.Executor`：轮询 `approved` 且未过期的审批单 → 原子领取（`approved→executing`，重复领取无效）→ 执行前重算 plan_hash 比对（GC-13）→ `registry.Execute` → 写 `fault_cmd_history` → Verify → **最后**回写 `executed/failed` + `result_json`；
- 崩溃恢复：Verify 排在回写之前，进程在"已执行但没验证"的窗口崩掉时审批单停在 executing；启动时 `RecoverExecutingApprovals` 把遗留的 executing 标成 failed 并写明需要人工核查——executing 意味着动作可能已经触达外部系统，不能自动重放，也不能永远卡在队列外；
- 执行前置闸：plan_hash 失配拒绝、工具未注册拒绝、过期 approved 单既不能被领取（`ClaimApprovalExecution` 带 expires_at）也会被 `ExpireApprovals` 扫成 expired（sweep 范围从 pending 扩到 pending+approved）；
- 自动 L2 路径统一走审批单：`Service.CreateSystemApproved` 落"系统批准"单（decided_by=system:auto_l2），执行面只有一个入口；
- `diagnose.Verifier`：执行后延时复查 incident 成员 `last_alert` 全 resolved（V1 定死这一种，配合 `simulate -resolved` 可控演示两条路径），结果落 `agent_run_step(kind=verify)`，挂在审批来源 run 上（GC-18）；查询与审计写入都用脱离取消的 ctx（各带 10s 时限），写 step 失败会打日志——关闭中的进程不能把验证结论写成"失败"，也不能悄悄丢掉它；
- Verify 三态：passed / failed / **inconclusive**（不可判定：取消、读库失败、incident 没有可复查成员）。不可判定既不进故障记忆也不触发重诊或记忆降级，只记 `verify_inconclusive` 指标并要求人工核查。空成员列表尤其不能判成"全部恢复"——那会让任何动作都被标记成功并进入记忆提交流程；
- dry_run：执行器只演练不真重启，result_json 标记 dry_run，跳过 Verify；
- `plan_hash` 归一化修复：MySQL JSON 列重排键序和空白，`PlanHash`/`ValidateExecution` 改为 Unmarshal→Marshal 重编码后比对（`normalizeJSON`）。

## 2. 数据流

```text
Pipeline policy 阶段
  ├─ L3/降级 L2 → approval(pending) → 人工 approve → approved
  └─ auto_l2    → approval(approved, decided_by=system:auto_l2)
                        │
Executor（轮询）          ▼
  claim approved→executing → plan_hash 重算校验 → registry.Execute(tool, args)
    → fault_cmd_history
    → VerifyAfterExecution（延时复查 last_alert 全 resolved）→ agent_run_step(kind=verify)
    → 记忆提交 / 降级 / 重诊调度（inconclusive 时全部跳过）
    → FinishApprovalExecution(executed|failed, result_json)   ← 最后一步

进程重启
  → RecoverExecutingApprovals：遗留 executing → failed（要求人工核查）
```

## 3. 测试覆盖

- docker_restart：白名单为空不注册、注册后为 L2 且不进 `ForLLM()`、白名单外容器拒绝、socket 缺失报错；限频最小间隔与每小时上限（按容器独立、窗口滚出后恢复）、限频在发请求前拦下；
- Executor：approved 执行成功 + 回写 + 命令历史 + Verify 触发；重复 drain 不重复执行（幂等）；plan_hash 篡改拒绝（failed）；未注册工具拒绝；dry_run 不执行不验证但回写 executed；过期 approved 单不执行；verify 不可判定时不重诊、不碰记忆、审批仍落 executed；
- Verifier：全 resolved 通过 + step 落库；有 firing 成员判失败；空成员列表判不可判定；读库失败判不可判定；ctx 取消也落 step（不留"永远等不到 verify"的审计洞）；ctx 已取消时复查不带取消 ctx 去读库；写 step 失败打日志带结论；
- PlanHash：键序/空白重排后哈希不变，值变更失配；
- store：approved 过期 sweep、领取条件更新幂等、FinishApprovalExecution 只允许 executing→终态、`RecoverExecutingApprovals` 回收遗留 executing、`CountRecentExecutions` 限频计数（含 failed、排除 pending/denied/窗口外/别的 plan_hash）。

## 4. 真实端到端验收（本机 Docker）

目标容器 `alpine sleep 600`（白名单 `d11-target`），incident 由 simulate 促发：

1. 失败路径：approved 审批单 → 容器被真实重启（State.StartedAt 改变）、approval=executed + result_json、fault_cmd_history 落行、成员仍 firing → verify step `passed=false`；
2. 成功路径：`simulate -resolved` 关单后再来一张审批单 → 重启执行、verify step `passed=true`；
3. 两张审批单各执行一次，无重复重启；
4. （修复前实测）MySQL JSON 归一化导致的 plan_hash 失配会被拒执并落 failed —— 该缺陷已修复并加回归测试。

```bash
go test ./...    # 含 TEST_MYSQL_DSN、TEST_PROMETHEUS_URL 集成测试
go build ./... && go vet ./...
```
