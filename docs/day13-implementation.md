# Day13 实现文档：精确故障记忆与命令历史

> 本文对应 `oncall-agent-开发SPEC.md` 的 D13 和 `docs/14-day-plan/day13-memory.md`。目标：不依赖向量数据库的精确记忆闭环，验证成功的同类故障零次 LLM 复用。
>
> V1 明确不引入向量数据库；语义检索未来只能作为参考。

## 1. Day13 做了什么

- `memory.FaultFingerprint(group_key, alert_name)`：md5 前 12 位十六进制，诊断前可得的确定性签名（A11：不用 RCA）；
- `memory.Store`：
  - `Lookup`：仅 high 置信且 TTL（以 last_success 为基准）未过期才命中；命中 hits+1、刷新 last_used，不改安全等级；
  - `Commit`：只收 confidence=high（GC-16 硬门槛）；
  - `Demote`：命中验证失败降 low 拉黑；
  - `RecentCmds`：同指纹最近命令历史；
- Pipeline 记忆阶段（seq 1，`kind=tool, name=memory_lookup`）：`retry_of` 为空的 run 才查；命中 → run mode 改 `memory_hit`、RCA/Plan 复用、跳过证据与 LLM（tokens=0），Guard/Policy/审批/Verify 照常（GC-17）；
- 记忆 miss 且有历史命令 → 注入证据前缀（标注"仅供参考，不构成权限依据"）；
- Executor 闭环：Verify 通过 + 非重诊 + 非命中 + plan.confidence=high + Guard 未改写 → Commit；memory_hit 且 Verify 失败 → Demote + 完整重诊（重诊 run retry_of 非空，天然不查记忆）；
- store：fault_memory CRUD（Get/Upsert/Touch/Demote）+ ListCmdHistory + UpdateAgentRunMode（running 才可改）；
- 指纹口径统一：`fault_cmd_history` 与 `fault_memory` 同用 `memory.FaultFingerprint`。

## 2. 门槛与防线

| 规则 | 落点 |
|---|---|
| 只有 high + 验证成功 + 首诊 + Guard 未改写才入库 | Executor.maybeCommitMemory + Store.Commit |
| 命中不提权 | Pipeline 命中后仍走 Guard/Policy/审批/Verify |
| 重诊不查记忆 | `run.RetryOf == nil` 才 Lookup |
| 命中失败即拉黑 | Executor.demoteMemoryIfHit |
| 命令历史只是证据 | renderCmdHistory 前缀声明 |

## 3. 测试覆盖

- memory：指纹稳定性与区分度；Lookup 四态（命中/过期/低置信/不存在）与 hits 语义；Commit 门槛；Demote 后不再命中；RecentCmds 限量与指纹隔离；
- Pipeline：命中跳过 LLM、tokens=0、mode=memory_hit、step 链无 evidence/llm；重诊不查记忆；miss 注入命令历史；
- Executor：验证成功且够格 → Commit（group_key/alert_name/high/指纹非空）；重诊 run 不入库；memory_hit 失败 → Demote + 重诊；
- store 集成：CRUD、upsert 覆盖、touch 计数、demote、命令历史倒序限量。

## 4. 真实端到端验收

预置 `payments + SimulatedAlert` 的高置信记忆（hits=3），simulate 触发同类故障：

- run 落为 `mode=memory_hit, status=succeeded, tokens_in=0, tokens_out=0`（0 次 LLM）；
- step 链 = memory_lookup → guard → policy → notify，无 evidence/llm 段；
- RCA 取自记忆原文；fault_memory hits 3 → 4。

```bash
go test ./...    # 含 TEST_MYSQL_DSN、TEST_PROMETHEUS_URL 集成测试
go build ./... && go vet ./...
```
