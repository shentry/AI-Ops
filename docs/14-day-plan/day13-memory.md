# Day13：精确故障记忆与命令历史

> 阶段：P3 故障记忆  
> 状态：已完成
> 依赖：Day12  
> 详细实现记录：[Day13 实现文档](../day13-implementation.md)

## 当日目标

实现不依赖向量数据库的精确记忆闭环，让验证成功的同类故障可以零次 LLM 快速处理，同时保留安全门槛。

## 实现清单

- [x] 实现 `FaultFingerprint(group_key, alert_name)`；
- [x] Lookup 仅返回 high confidence 且 TTL 未过期的记忆；
- [x] 命中时更新 hits/last_used，但不改变安全等级；
- [x] 命中后复用 Plan，仍经过 Guard、审批和 Verify；
- [x] Commit 仅允许验证成功且 confidence=high 的案例；
- [x] Guard 改写或重诊成功的案例不得直接写入 high memory；
- [x] Memory hit 验证失败时 Demote 为 low，并转完整重诊；
- [x] L2 执行结果写入 `fault_cmd_history`（D11 执行器已落，键口径与 fault_memory 统一）；
- [x] Memory miss 时注入最近命令历史作为证据，不作为权限依据；
- [x] 记录 memory hit、miss、expired、demote 指标和审计步骤（memory_lookup step + hits/demote 计数）；
- [x] 明确 V1 不引入向量数据库，未来语义检索只能作为参考。

## 关键文件

- `internal/memory/fingerprint.go`
- `internal/memory/store.go`
- `internal/diagnose/pipeline.go`
- `internal/store/store.go`
- `migrations/001_init.sql`

## 验收清单

- [x] 同一故障第二次触发为 `mode=memory_hit`；
- [x] Memory hit 的 `tokens_in=0` 且没有 LLM step；
- [x] Memory hit 仍会执行 Verify，失败后自动 Demote；
- [x] 低置信、过期和失败案例不会命中；
- [x] 成功执行结果可在命令历史查询到；
- [x] 历史命令只进入 Evidence/Prompt，不改变动作等级；
- [x] `go test ./... && go build ./... && go vet ./...` 通过。

## 不包含

- Redis、RabbitMQ/Kafka、Milvus/PGVector 等基础设施引入；
- 自然语言聊天入口；
- 跨项目共享记忆。
