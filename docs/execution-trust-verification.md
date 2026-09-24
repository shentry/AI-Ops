# 执行安全与恢复验证：实施与验收记录

对应 [execution-trust-design.md](execution-trust-design.md)。范围保持单实例 Go 单体、MySQL 持久队列、可信网络匿名控制台；未向业务环境发布，未提交 Git commit。

## 验证环境与结果

- Go 1.24.4、Node 25.9、MySQL 8.0.46；真实依赖实验使用独立 Sub2API 0.2.4、PostgreSQL 18、Redis 8、Prometheus、blackbox-exporter、Alertmanager。
- 最新全仓 `go test -race -count=1 ./...`：**745 个测试/子测试通过**。三个独立 MySQL DSN 均设置；新增 API、诊断及存储集成测试实际执行，没有以跳过代替通过。
- 全仓 `go build ./...`、`go vet ./...`、Go 格式检查通过；前端 **53 项 Playwright 契约/交互测试通过**，TypeScript 检查及生产构建通过。
- 全仓运行中的两个 skip 有明确边界：真实付费 LLM 测试未执行；要求空库的迁移测试通过单独命令，在新建 empty/legacy 数据库中执行并通过。
- 浏览器连接真实 Go 后端，不拦截 API；实际核对审批字段、提交拒绝、活跃处理冲突、执行/验证双状态、演练/拒绝/过期/人工核查页面，以及重新构建后的流程图。唯一良性网络提示是 `/favicon.ico` 的 404，没有页面 API 加载错误或 JS 异常。
- 浏览器完整截图已保存供人工查看；本次模型不能直接查看图像，因此不把截图文件存在冒充像素级视觉审查。

## T1–T17 对照

| 编号 | 结论 | 主要证据与覆盖范围 |
|---|---|---|
| T1 | 通过 | `internal/config/config_test.go` 拒绝未知/删除键；`tests/acceptance/live.py native/up` 实际检查回环监听、容器向内部 18080 Webhook 投递并收到 202。 |
| T2 | 通过 | `internal/api/dto_test.go`、飞书卡片测试、`web/tests/approval.spec.ts`；真实浏览器核对目标、单容器范围、L2、明确布尔演练模式、理由、Hash、过期时间，并实际拒绝审批。 |
| T3 | 通过 | `internal/incident/execution_test.go` 规范化内容 Hash；`TestApprovalMySQLWebFeishuRace`、`TestApprovalMySQLChangedContentFailsClosed`、`TestApprovalMySQLRequiredHashAndCanonicalProjection` 经真实 Handler/Service/MySQL 检查并发与内容漂移；race 重复运行通过。 |
| T4 | 通过 | Executor/Verifier 绑定与模式单测、`TestVerificationMySQLScopeRecheckedBeforeHTTP`；旧真实审批不因开关变化变成演练，演练不升级，漂移不请求新地址。 |
| T5 | 通过 | 真实依赖实验在第一张验证任务 pending 时执行另一个 Incident；真实 SSE/续传和浏览器双状态；`TestControlRoomVerificationOutcomeSurvivesEventWindowEviction`、`TestControlRoomFlowDoesNotReusePreviousRunAction` 防止步骤完成或旧 Run 事件伪造验证结论。 |
| T6 | 通过 | `TestVerificationMySQL503Then2xxWithPipelineMemory`、`TestVerificationMySQLPersistent503AtDeadline` 使用真实 MySQL + httptest HTTP；实际 Sub2API 实验中直接健康通过先于独立 resolved 到达。 |
| T7 | 通过 | `TestVerificationMySQLInconclusivePreservesObservationAndMemory` 覆盖无观测、过旧观测、不可达、迟到 200；不重诊、不降级，保留最后真实观测时间与原 deadline。 |
| T8 | 通过 | Policy/HTTP 单测、真实混合故障实验、诊断 MySQL scope 测试；`TestClaimApprovalExecutionScopeRace`、`TestFinalizeVerificationScopeRace`、`TestCompleteRunScopeRace` 在 MySQL 实际锁等待后提交新 Slow 成员，拒绝旧范围；`TestUpdatingLinkedAlertSerializesWithExistingIncident` 验证旧成员版本变更也受父锁约束。 |
| T9 | 通过 | 条件 SQL `SIGNAL` 导致执行结果事务回滚；`TestExecutionResultAndVerifyTaskRollbackTogether`、幂等完成测试、`TestBoundedExecutionResultPreservesReplayIdentity`；实际 Docker 转发记录证明提交重试未重调动作，Executor 单测覆盖已提交但回包丢失。 |
| T10 | 通过 | 真实 backend 停启恢复 pending 验证；真实 SIGKILL 留下未持久化执行结果，恢复为 failed/manual_check，不重放 Docker 动作；存储/Executor 恢复测试。 |
| T11 | 通过 | Worker 取消测试、`TestVerificationStaleClaimCannotCompleteNewClaim`、诊断 MySQL lease/replay 测试；旧 claimed_at 不能完成新领取，deadline 不延长；`TestRecoveryCountsOnlyCommittedChanges` 验证提交前取消不虚报成功计数。 |
| T12 | 通过 | `TestConcurrentManualAndRetryAdmissionCreatesOneCycle` 的 20 个并发请求只创建一个新周期；`TestAdmissionExcludesApprovalAndVerification`；冷却/HTTP 429/Retry-After 测试与真实浏览器 active_processing 拒绝。 |
| T13 | 通过 | `TestVerificationRetryBudgetEndsInDurableEscalation` 首诊后仅两次重诊，并持久化 manual_check/escalation；缺失父链不能开启新预算。 |
| T14 | 通过 | `TestPipelineMySQLCriticalAuditGates` 注入开始事件、普通/工具步骤、审批发布故障；验证审计/记忆/重诊 SQL 回滚测试；`TestFailedPublicationCanRecoverSameRunWithRepeatedStepSequence` 验证同一 Run 可以恢复并保留重复 Seq 的尝试历史。 |
| T15 | 通过 | 真实 dry-run 无 Docker 请求、verify_task 或成功记忆；迁移保留 NULL 上下文和历史 Hash、不补任务；DTO/前端旧记录未知测试，真实浏览器演练及失败人工核查展示。 |
| T16 | 通过 | `tests/migrations/execution-upgrade.sh` 在两个新库执行空库 001–011 和含旧数据的升级；实际 CLI 退役/只读检查；`live.py` 构建前端后嵌入新 Go 二进制、比对服务返回 JS/CSS 字节，真实故障时间见下节。 |
| T17 | 通过 | 配置与 Verifier 测试独立校验 timeout/interval/window；证据采集超时变化不改变验证超时，验证单次超时严格小于 30 秒领取上限。 |

纯规则/HTTP 适配、MySQL 事务、实际依赖和浏览器承担不同验证范围，不能互相替代。表中“通过”是这些范围的联合证据，不表示所有分支都在真实业务 Sub2API 上注入过。

## 终审修复与事务约束

1. **MySQL 旧快照**：锁前普通身份查询会在 REPEATABLE READ 中固定读视图。实际锁等待回归先复现错误执行、成功记忆、重诊和审批发布，再验证修复。`ClaimApprovalExecution`、`FinishExecution`、`FinalizeVerification`、`CompleteRun`、`RequestRun` 使用 GORM 原生事务级 `READ COMMITTED`；不改全局/会话隔离配置，不手写事务包装层。
2. **成员写入锁**：`ApplyRawEvent` 在同一事务执行 `applyAlert` 与关联 hook；`AssignIncident` 锁父 Incident 后挂成员。已关联指纹的内容变更在 `applyAlert` 中先锁原 Incident，避免换分组时绕过原成员的安全读取。
3. **TTL 单一裁决点**：已提交的执行领取是 TTL 权威；Executor 不在接收领取结果后用另一个时钟把它遗留为无人处理的 executing。存储测试仍保证过期审批不会领取。
4. **同一事实来源**：流程图使用当前 Run 的持久化审批/验证状态；普通 Step 的 FinishedAt、最近 20 条事件或旧 Run 的事件都不能替代恢复结论。最近变更面板继续保留历史动作。
5. **回放与表映射**：超长执行结果保存规范化摘要，不同省略内容仍冲突；恢复计数仅在提交确认后递增；补齐 `IntegrationEventReceipt.TableName()`，与迁移的单数表名一致。

无验证任务时：可信 pending/approved/executing 为 `not_started`；simulated/denied/expired/failed 为 `not_applicable`。执行失败仍需人工检查目标，“不适用”不代表已恢复。旧/不可验证快照，以及现代 executed 异常缺失必要任务，保持 `unknown`。

## 实际故障观测（UTC，2026-09-11）

| 事实 | 观测时间 |
|---|---|
| 首次真实 Docker restart 请求开始 | 17:32:15.741321 |
| Docker Engine 返回 204 | 17:32:15.826843 |
| 结果提交重试成功、原子产生验证任务 | 17:32:18.881 |
| 直接 Sub2API 健康检查完成并持久化 passed | 17:32:23.669 |
| Alertmanager resolved 独立到达转发边界 | 17:32:42.411708 |

实验对三张真实审批产生三次物理 restart 请求；其中两次结果成功持久化，第三次在提交前崩溃后转人工核查，没有重放。首张任务的通过观测发生在同一目标的两次独立批准重启之后，**不能归因于第一次重启，也不能作为默认 10/120/5 参数的恢复时延校准**。详见 [实验说明](../tests/acceptance/experiment.md)。

LLM 仅用本地确定性 OpenAI 兼容边界，未调用真实付费模型；Docker、Sub2API、PostgreSQL/Redis、Prometheus/blackbox/Alertmanager 与 MySQL 都是真实隔离实例。该 Sub2API 版本运行中的 `/health` 固定返回 200，故字面 503 分支由真实 HTTP 测试服务器与 MySQL 联测验证，不伪称实际 Sub2API 产生了 503。

## 重复验证

```sh
# 三个库相互隔离，均为已执行 001–011 的可丢弃 MySQL 8 测试库。
export TEST_MYSQL_DSN TEST_API_MYSQL_DSN TEST_DIAGNOSE_MYSQL_DSN
# 可选，指向自己的隔离 Prometheus：
export TEST_PROMETHEUS_URL

gofmt -l cmd internal
go build ./... && go vet ./...
go test -race -count=1 ./...
(cd web && npm ci && npx playwright install chromium && npm test && npm run typecheck && npm run build)

# 只接受有指定名称/label/端口的隔离 MySQL；每次创建新的空/旧数据演练库。
export MYSQL_TEST_ROOT_PASSWORD MYSQL_TEST_APP_PASSWORD
bash tests/migrations/execution-upgrade.sh
```

条件触发器故障测试需要专用测试账号的 TRIGGER 权限；binlog 信任设置只用于可丢弃测试实例。实际依赖与浏览器步骤见实验说明；不对业务库运行这些命令。

## 证据位置与边界

主要本地证据位于 `/tmp/oncall-execution-acceptance/`：

- `final/go-race.jsonl`：逐项执行、通过和跳过记录；`gofmt.txt` 为空。
- `final/upgrade-final/verification.log`：父任务亲自重跑的空库/旧数据迁移与维护回滚。
- `final/frontend-tests.log`：53 项交互/契约测试；`final/prometheus-tools.log`：实际 Prometheus 只读接口。
- `final/scope-race-red.log`：锁等待旧快照的修复前确定性失败；源码中的相同测试已在修复后重复通过。
- `live/result.json`、`commands.jsonl`、`boundary.jsonl`、数据库快照、版本化 build-manifest：真实动作、HTTP 观测、resolved 到达及嵌入产物比对。
- `browser/`：真实浏览器字段/提交/流程图前后对比、最终 QA、DOM 断言与完整截图。裁决产生于单独 browser-ui Incident，未增加物理 restart。
- `final/model-schema-audit.txt`：17 个当前持久化模型与迁移表名一致；`module-tidy.diff` 为空，未更换依赖版本。
- `final/live-database.sql` 与 `final/cleanup.txt`：清理前导出实验数据库；已移除八个实验容器、网络、socket 卷及专用 live 数据库/用户，关闭测试浏览器。保留独立 MySQL 沙箱与存储/API/诊断测试库供复验，未操作业务容器。

GitHub 托管 CI 本身未运行；仓库工作流已检查并配置三个隔离数据库、显式迁移演练与前端门禁。npm 报告的 9 项既存依赖审计项没有在本次功能改造中批量升级。没有声称跨 Docker/MySQL exactly-once、个人身份保证、多实例执行安全、IM 必达或通用故障恢复。
