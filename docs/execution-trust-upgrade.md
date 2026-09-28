# Execution-trust 离线升级与回退

对应 [设计 §9.3](execution-trust-design.md#93-发布迁移) 和 [自动处置方案 §5.2、§9](production-auto-remediation-plan.md)。当前版本包含迁移 **001–015**。适用 **MySQL 8、单实例、先停旧进程再启新进程**；不支持新旧 server 重叠运行。历史迁移不改写。以下命令从仓库根目录执行；数据库和服务进程必须由操作者明确确认，不要直接照搬测试库到业务环境。

- 已有 001–008 的库：按下文第 1–3 步完整执行（退役旧审批，再执行 009–015）。
- 已有 009–011 的库：执行第 1 步和“从 011 升级到 015”，再做第 3 步。
- 已有 001–014 的库：停进程并备份后执行 `migrations/015_notification_task.sql`，再运行 `-check`；新版本授权摘要会使旧待执行审批失效，旧在途验证因授权不匹配进入人工核查。
- 空库：按文件名顺序执行全部迁移一次。

## 不变量与命令

```sh
# MYSQL_DSN 从部署密钥管理/受限环境注入，不写进脚本、版本库或日志。
go run ./cmd/retire-approvals -check  # 只读；不合格返回非零
# 仅在停止所有写入者并备份后执行：
go run ./cmd/retire-approvals -apply
```

必须且只能给一个 `-check` 或 `-apply`；无参数、两个参数同时给出、额外位置参数均拒绝。`-apply` 是操作者的离线确认，**不是进程锁，也不会替你停服务**。命令只连接 MySQL，不载入 server 配置、不启动 Worker、不探测目标、不调用 Docker/变更工具。

| 数据 | `-apply` 处理 |
|---|---|
| 009 前 pending / approved | expired；一条 `approval.expired` 事件说明升级失效 |
| 009 前 executing | failed；结果记录 `manual_check=true`、outcome unknown；同事务写 `execution.failed` 和 open/critical `manual_check` 问题 |
| 009 后以上状态且 **SQL NULL** execution_context | 同上 |
| 009 后非 NULL execution_context | 不动，包括现代 pending / approved / executing 快照 |
| 历史 executed / failed / denied / expired，以及 simulated | 不动 |

保留历史 tool/args、Hash、原裁决人/来源/时间、TTL 和 NULL 上下文；不猜测旧执行是否 dry-run，不补建 verify_task，不写命令执行历史，不自动重诊或重放动作。仅对结果未知的 executing 替换 result_json 为人工核查结果；**failed 在这里不表示已证明外部动作未发生**。同一 Incident 的 `manual_check` 使用现有问题表的 incident/code 唯一键聚合。

每张审批独立事务，依次锁 Incident、审批并重新检查状态/SQL NULL；审批状态、事件、必要问题一起提交。某张失败会停止，前面已提交的审批保持提交。输出数量只包含数据库确认提交的行。提交回包丢失时不推断该行是否提交；排查数据库后离线重跑即可，已终结的行不会重复产生事件/问题。命令有一分钟超时，超时同样允许重跑，不需要手工重置状态。

`-check` 与 server 启动前使用同一个 `CheckExecutionReady`：检查 009–014 的执行、验证、诊断及处置表字段/索引，以及 015 的持久通知表，并拒绝残留的 active NULL-context 审批。检查不写数据，不自动迁移、不隐式失效审批。现代快照由裁决、领取、恢复及验证的契约检查把关。离线退役事件标记为 `phase=upgrade`，由维护人处理，因此该命令仍可在迁移 015 之前运行。

## 升级步骤（已有 001–008 数据库）

### 1. 停入口、停进程、备份

1. 在部署入口暂停 Webhook、审批/重诊、飞书回调和其他写入口，暂停会自动重启 server 的 supervisor。
2. 等待在途请求结束，通过实际部署管理器停止旧 server，确认进程已退出、没有第二实例或离线写入程序。不要在旧 server 仍运行时执行迁移或维护命令。
3. 人工核查 executing 对应目标与外部日志，记录证据。**不能为了升级把 executing 改为 executed，也不能再次 restart 来“确认”。** 维护命令保留结果未知的人工处理语义。
4. 保存同一时点的旧二进制（包括其前端产物）、旧配置和全库备份。下面使用 MySQL login-path，密码交互输入或由受限密钥文件提供；不要把密码放在命令行。

```sh
set -euo pipefail
# 首次配置；将地址/端口/用户替换为已确认的维护连接。
mysql_config_editor set --login-path=oncall-maint --host=127.0.0.1 --port=3306 --user=MAINTENANCE_USER --password
export DB_NAME=YOUR_CONFIRMED_DATABASE
case "$DB_NAME" in ''|*[!A-Za-z0-9_]*) echo 'invalid DB_NAME' >&2; exit 1;; esac
export BACKUP_DIR=/secure/backups/oncall-before-execution-trust
umask 077
mkdir -p "$BACKUP_DIR"
# OLD_SERVER_BINARY / OLD_CONFIG_FILE 指向当前实际部署文件。
cp "$OLD_SERVER_BINARY" "$BACKUP_DIR/server.old"
cp "$OLD_CONFIG_FILE" "$BACKUP_DIR/config.old.yaml"
mysqldump --login-path=oncall-maint --single-transaction --routines --triggers --events \
  --hex-blob --set-gtid-purged=OFF --no-tablespaces "$DB_NAME" > "$BACKUP_DIR/database.sql"
test -s "$BACKUP_DIR/database.sql"
mysql --login-path=oncall-maint "$DB_NAME" -e \
  "SELECT id,incident_id,run_id,status,plan_hash FROM approval WHERE status IN ('pending','approved','executing') ORDER BY id" \
  > "$BACKUP_DIR/legacy-active.txt"
```

备份必须验证可恢复（建议恢复到另一个空库），文件包含敏感业务数据，应限制权限。不要使用 `--databases` 生成含原库 `USE` 的备份，因为回退步骤会恢复到新空库。

### 2. 显式退役旧审批，再执行新增 DDL

确认 `MYSQL_DSN` 指向上面的同一个 `DB_NAME`，使用 `parseTime=true&loc=UTC`。在新源码目录运行维护命令，无需先运行新 server：

```sh
go run ./cmd/retire-approvals -apply
mysql --login-path=oncall-maint "$DB_NAME" -e \
  "SELECT COUNT(*) AS remaining_legacy_active FROM approval WHERE status IN ('pending','approved','executing')"
# 此时应为 0；有错误或残留则停止排查，不启动 server。
for migration in migrations/009_*.sql migrations/01[0-5]_*.sql; do
  mysql --login-path=oncall-maint "$DB_NAME" < "$migration"
done
go run ./cmd/retire-approvals -check
```

MySQL DDL 会隐式提交，009–015 **不是一个事务，也不是可整批重复执行的幂等脚本**。任何失败都应保持停机，检查 `SHOW CREATE TABLE` / 索引及已执行语句，确认进度后只执行尚未完成的 DDL；不盲目重跑、不改历史迁移。如果 009 已先执行，维护命令仍可处理 NULL-context 活跃记录，现代非 NULL 快照不受影响；全部完成后再 `-check`。

空库则按文件名顺序执行 **全部迁移一次**，无需退役数据：

```sh
for migration in migrations/*.sql; do
  mysql --login-path=oncall-maint "$DB_NAME" < "$migration"
done
go run ./cmd/retire-approvals -check
```

### 从 011 升级到 015（多动作自动处置）

012–015 同样是**离线迁移**：先按第 1 步停入口、停进程、备份。执行快照为版本 3，规则、服务配置、验证标准都绑定授权摘要；新增持久通知保证故障结果提交后仍可重试投递。旧快照不会被默认为新授权：

| 升级时的旧记录 | 新进程的处理 |
|---|---|
| pending / approved 的旧快照 | 领取时判定失效（expired），不执行；需要时重新诊断生成新快照 |
| 未完成的旧 verify_task | 结束为 inconclusive，需人工确认目标状态 |
| executing 的旧审批 | 按结果未知处理：failed + `manual_check`，不重放 |
| 历史终态 | 不动；没有规则 ID 的旧执行不计入任何规则预算或阻断 |

因此升级前最好等在途执行和验证结束，并把仍要处理的待审批单先在旧版本裁决或放弃。然后：

```sh
for migration in migrations/01[2-5]_*.sql; do
  mysql --login-path=oncall-maint "$DB_NAME" < "$migration"
done
go run ./cmd/retire-approvals -check
```

失败时的处理与上面相同：保持停机，确认已执行到哪条语句，只补执行未完成的 DDL。

### 3. 检查事实、配置与构建，最后启服务

```sh
mysql --login-path=oncall-maint "$DB_NAME" <<'SQL'
SELECT id, incident_id, status, plan_hash, execution_context, result_json
FROM approval WHERE execution_context IS NULL ORDER BY id;
SELECT approval_id, event_type, status, summary, created_at
FROM incident_event WHERE summary LIKE '%execution-context upgrade%' ORDER BY id;
SELECT incident_id, run_id, code, status, severity, summary
FROM incident_problem WHERE code = 'manual_check' ORDER BY id;
SELECT COUNT(*) AS unsafe_active FROM approval
WHERE execution_context IS NULL AND status IN ('pending','approved','executing');
SELECT COUNT(*) AS legacy_verification_tasks
FROM verify_task v JOIN approval a ON a.id = v.approval_id WHERE a.execution_context IS NULL;
SQL
```

最后两个计数应为 0。与备份比对历史终态、Hash、裁决及命令记录；历史 NULL 上下文仍表示“旧版未知”，不是验证成功。

- 配置按 [config.example.yaml](../config.example.yaml) 重写，已删除的键会让启动失败：`approval.dry_run`、`approval.auto_execute_l2`、`approval.verify_delay_seconds`、`tools.docker`、`diagnose.evidence` 下的目标地址（`docker_container`、`sub2api_base_url`、`postgres_dsn`、`redis_addr`）和 `diagnose.verification`。
- 被监控服务的身份、容器、健康地址、管理密钥和发布入口只写在 `service`；恢复验证参数在 `remediation.verification`（`0 < timeout < interval < window`、`timeout < 30`、连续通过次数和复发观察窗口）。
- 处置规则写在 `remediation.rules`，同时设置 `rules_version`。升级后首次运行把所有规则设为 `observe`：只记录本会采取的动作，不产生写操作；核对无误后逐条改为 `manual`，完成演练后才改为 `auto`。
- 控制台没有匿名访问：启用 `web.base_url` 时必须配置 `web.operators`（令牌只存 SHA-256）。明确 `server.listen_addr` 与可信网络边界。
- 使用新配置，不用旧配置启动新二进制。先构建前端，再构建包含该前端的 Go 二进制：

```sh
(cd web && npm ci && npm run build)
go build -o bin/oncall-agent ./cmd/server
# 用部署管理器启动唯一的新实例；直接运行方式如下：
CONFIG_FILE=/secure/config/oncall-new.yaml ./bin/oncall-agent
```

在可信入口触发新诊断，确认 observe 规则只在 policy 步骤记录“would …”，没有审批、verify_task 或真实变更；旧待审批不能复活。完成独立部署验收、处理人工核查问题后，才能逐条把规则改为 manual/auto 并恢复全部入口。本维护脚本的通过不替代前端、工具调用、健康验证或真实故障实验验收。

## 回退（不能只换回旧二进制）

1. 再次停全部写入口、停新 server/自动重启器，确认所有新进程退出。先另存当前库与外部动作记录。
2. 人工决定是否接受丢失备份点之后的数据，并核对其间真实外部变更。**恢复数据库不会撤销容器 restart 等外部动作。** 旧库中可能重新出现旧 pending/approved/executing；在释放入口/执行器前必须完成对应人工核查，禁止用旧队列自动重放结果不明的动作。
3. 把升级前全库备份恢复到一个全新的空库，使用旧配置/旧二进制；不要把旧表覆盖回当前新库，留下 verify_task/新数据混用。以下 `CREATE DATABASE` 若库已存在即失败，不使用 `IF NOT EXISTS`。

```sh
export RESTORE_DB=YOUR_CONFIRMED_ROLLBACK_DATABASE
case "$RESTORE_DB" in ''|*[!A-Za-z0-9_]*) echo 'invalid RESTORE_DB' >&2; exit 1;; esac
mysql --login-path=oncall-maint -e \
  "CREATE DATABASE \`$RESTORE_DB\` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci"
mysql --login-path=oncall-maint "$RESTORE_DB" < "$BACKUP_DIR/database.sql"
cp "$BACKUP_DIR/config.old.yaml" /secure/config/oncall-rollback.yaml
# 给原应用用户授予仅 RESTORE_DB 所需权限；仅把旧配置的 DSN 库名改成 RESTORE_DB。
# 核对数据/人工处理未知动作后才启动（仍保持外部入口暂停，按旧版本安全规程验收）。
CONFIG_FILE=/secure/config/oncall-rollback.yaml "$BACKUP_DIR/server.old"
```

恢复旧配置必须连恢复库，旧二进制不得消费新模式审批。保留升级前备份、新库和事故记录直到回退验收结束；不提供逆向 ALTER、不重算 Hash、不维护双写/双算法兼容。若无法安全核对备份点后的外部动作，继续停机人工处理，而不是强行回退运行。

## 可重复的隔离迁移演练

`tests/migrations/execution-upgrade.sh` **只接受**名为 `oncall-execution-69a3ee64-mysql`、label `oncall.goal=69a3ee64-5465-4d6c-a023-76ee5bebffbe`、映射 `127.0.0.1:23306` 的沙箱。每次创建全新 `oncall_upgrade_69a3_empty_*` / `oncall_upgrade_69a3_legacy_*` 库；仅对新库授权现有 `oncall_test` 用户。不修改共享 `oncall_test` 数据库、全局配置、父任务触发器或其他容器，不启动 server/外部动作。

```sh
# 从受限环境提供沙箱凭据；脚本无默认密码，也不会输出密码。
export MYSQL_TEST_ROOT_PASSWORD
export MYSQL_TEST_APP_PASSWORD
bash tests/migrations/execution-upgrade.sh
```

覆盖空库顺序 001–015；含七种旧状态的 001–008 库通过真实 `go run ./cmd/retire-approvals -apply` 再升级 009–015；逐阶段只读 preflight 拒绝；现代快照/旧终态保护；重复命令；事件/问题触发器故障回滚；提交前取消只统计成功提交；两个维护者同时看见同一候选的行锁幂等性。常规 store 测试使用 `TEST_MYSQL_DSN`，迁移测试额外要求 `TEST_EXECUTION_UPGRADE_MODE=empty|legacy` 且目标库为空，否则拒绝；脚本显式设置这些参数，不会把 skip 算作通过。

证据输出到 `/tmp/oncall-execution-acceptance/upgrade-<UTC>_<PID>/verification.log`（实际目录时间/PID 以脚本输出为准，可用 `EXECUTION_UPGRADE_EVIDENCE_DIR` 覆盖）。测试数据会按测试清理，新建库结构保留用于检查。该日志仅证明离线迁移/维护范围，不代表整个 execution-trust 目标或真实故障发布验收已完成。
