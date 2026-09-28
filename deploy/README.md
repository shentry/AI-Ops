# sub2api 生产部署

> 2026-09-26：无人值守修复与实际验收见 [验证记录](../docs/unattended-remediation-verification.md)。当前数据库需迁移到 015；自动规则缺少管理员、通知、业务探针或持续验证条件会被启动检查拒绝。

对应 [自动处置方案](../docs/production-auto-remediation-plan.md) §9 与阶段 A/B/E。Agent 作为 sub2api 宿主机上的 systemd 进程运行；监控栈是独立 Compose 项目（[monitoring/](monitoring/)）。两者都不随 sub2api 发布或重启。

凭据只放在宿主机的受限文件里（下文的 `/etc/oncall-agent/env`、`monitoring/.env`、`monitoring/secrets/`），不写进仓库、方案、评测数据或聊天。

## 1. 阶段 A：只有维护人能完成的事项

以下事项决定哪些动作可以开放，代码无法替你确认。结论记录在[方案](../docs/production-auto-remediation-plan.md) 3.1 与第 12 节；“状态”列为 2026-09-25 的核查结果。

| 事项 | 做法 / 核对命令 | 状态 | 影响 |
| --- | --- | --- | --- |
| 确认运行版本 | `docker exec sub2api /app/sub2api --version` 与 `docker inspect --format '{{index .Config.Labels "org.opencontainers.image.version"}}' sub2api` 一致，且 `docker diff sub2api` 不含 `/app/sub2api` | 已确认 v0.2.8。此前镜像标签是 v0.1.164，实际运行的是容器内被替换的 0.2.0（备份文件命名与后台在线更新一致） | 只比镜像 ID 发现不了在线更新；不一致时按实际运行版本复核方案 3.1 |
| 固定镜像 | `/opt/sub2api` 的 Compose 把 `image:` 写成 `weishaw/sub2api@sha256:…`，`docker compose up -d --no-deps sub2api` | 已固定为 v0.2.8（`sha256:9bad8d33…`） | 不用后台“系统更新 / 回滚”：它替换容器内的二进制，绕过发布入口和部署锁，容器重建后丢失 |
| 登记首个发布 | Agent 启动后 `POST /api/v1/changes`（`change_type=release`、`image_ref` 为上面的 digest、`db_migration` 按实际填写），再由操作人在“自动处置”页标记健康 | 已登记 v0.2.8（`db_migration=incompatible`，0.2.0 → v0.2.8 含列改名），待操作人标记健康 | 只有人工标记健康的发布可作为回退目标 |
| Prometheus 只在内网 | 停掉原来绑定 `0.0.0.0:9090` 的 Prometheus，改用本目录监控栈；`ss -ltnp` 确认 9090/9093 只在 127.0.0.1 | 已完成，旧 Prometheus / node-exporter 已删除 | Prometheus/Alertmanager 无认证 |
| 发布入口与部署锁 | 提供固定脚本（例如 `/opt/sub2api/deploy.sh <image_ref>`），CI/CD 和人工发布都 `flock` 同一个锁文件 | 生产没有 | 未配置 `service.release` 时发布回退不可用 |
| 数据库迁移行为 | 每次发布登记 `db_migration`（none / compatible / incompatible / unknown） | 每次启动自动迁移（已核实） | 当前发布为 incompatible 或 unknown 时自动回退被拒绝 |
| 配置历史 | 确认 Compose 环境变量是否有版本管理 | 没有 | `config_restore` 暂缓 |
| 管理员密钥 | 在 sub2api 后台生成管理员 API Key，Agent 与 exporter 共用；定期用 sub2api 审计日志核对调用 | 待生成 | sub2api 只有一个全局管理员密钥，重新生成会使旧值失效；密钥有全部权限，调用范围只靠 Agent 的接口白名单约束 |
| 上游与费用 | 各分组的可调度上游账号数量、是否有备用上游、业务探针的费用上限 | 部分分组只有 1 个可调度账号，1 个分组为 0；`cli-proxy-api` 不是 sub2api 上游 | 决定 `upstream_quarantine` 的 `min_available_accounts` 与是否启用探针 |
| 演练环境与通知 | 隔离的同版本 sub2api 演练环境；人工告警接收地址；外部心跳服务 | 待定 | §11 演练只在隔离环境进行；地址未配置前监控告警不会送达任何人 |

## 2. Agent（systemd）

### 状态库

Agent 的 MySQL 8 独立于 sub2api 的 PostgreSQL。生产用 apt 的 `mysql-server`（8.4）装在宿主机上，不放进 Docker：Docker 故障时 Agent 仍要能记录和通知。空库按文件名顺序执行 `migrations/*.sql` 全部迁移；已有库按 [升级文档](../docs/execution-trust-upgrade.md) 离线升级。同一状态库只能有一个 Agent：第二个实例拿不到执行锁（MySQL `GET_LOCK`）会启动失败。

```sh
apt-get install -y mysql-server
printf '[mysqld]\nbind-address = 127.0.0.1\nmysqlx = OFF\nbinlog_expire_logs_seconds = 604800\n' > /etc/mysql/mysql.conf.d/oncall.cnf
systemctl restart mysql
mysql -e "CREATE DATABASE oncall CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;
  CREATE USER 'oncall'@'127.0.0.1' IDENTIFIED BY '…'; GRANT ALL PRIVILEGES ON oncall.* TO 'oncall'@'127.0.0.1';"
for f in migrations/*.sql; do mysql oncall < "$f"; done
# MYSQL_DSN=oncall:…@tcp(127.0.0.1:3306)/oncall?parseTime=true&charset=utf8mb4&loc=UTC&timeout=3s&readTimeout=5s&writeTimeout=5s
```

每日备份用 `/etc/cron.d/oncall-agent-backup`（`SHELL=/bin/bash`），保留 7 天；定期把最新备份恢复到临时库，核对表数和行数后删除：

```
30 3 * * * root set -o pipefail; install -d -m 0700 /var/backups/oncall-agent && mysqldump --single-transaction --routines --events oncall | gzip > /var/backups/oncall-agent/oncall-$(date +\%F).sql.gz && find /var/backups/oncall-agent -name "oncall-*.sql.gz" -mtime +7 -delete
```

### 构建与安装

在与宿主机同架构的环境构建（前端先构建，二进制会嵌入它）：

```sh
(cd web && npm ci && npm run build)
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/oncall-agent ./cmd/server
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/sub2api-exporter ./cmd/sub2api-exporter
```

```sh
useradd --system --home-dir /nonexistent --shell /usr/sbin/nologin oncall
install -d -m 0755 /opt/oncall-agent && install -m 0755 bin/oncall-agent /opt/oncall-agent/
install -d -m 0750 -g oncall /etc/oncall-agent
install -m 0640 -g oncall config.yaml /etc/oncall-agent/config.yaml   # 按 config.example.yaml 编写，只含 ${VAR} 占位符
install -m 0600 /dev/null /etc/oncall-agent/env                       # 在宿主机上编辑，填入密钥
install -m 0644 deploy/oncall-agent.service /etc/systemd/system/
systemctl daemon-reload && systemctl enable --now oncall-agent
journalctl -u oncall-agent -f   # 看到 "oncall-agent: server ready"
```

`env` 中是 `config.yaml` 引用的变量，例如 `MYSQL_DSN`、`AUTH_TOKEN`、`SUB2API_ADMIN_API_KEY`、`ARK_KEY`、`ONCALL_ADMIN_TOKEN_SHA256`。`AUTH_TOKEN` 与监控栈的 `secrets/agent_token` 相同，`SUB2API_ADMIN_API_KEY` 与监控栈 `.env` 相同，都在宿主机上直接拷贝。以下情况会让启动失败：未知或已删除的配置键；引用了未设置的 `${VAR}`；规则引用了未启用的动作。例如没有 `service.release` 时不能写 `deployment_rollback` 规则。不配 `llm` 时诊断 worker 不启动，Agent 只接收、记录和验证。

### 网络

- Alertmanager 在容器里通过 `host.docker.internal:18080` 访问 Agent（`host-gateway` 即 docker0 地址，通常是 `172.17.0.1`）。把 `server.listen_addr` 设为该地址（`ip -4 addr show docker0`），不要用 `0.0.0.0`。宿主防火墙若限制入站，只放行监控网络网段到 18080。
- 控制台通过 SSH 隧道访问：`ssh -L 18080:172.17.0.1:18080 <host>`，`web.base_url` 写 `http://127.0.0.1:18080`。控制台没有匿名访问，每个操作人一个令牌（配置里只存 SHA-256）。
- Agent 访问的地址都在宿主机本地：`tools.prometheus.base_url: http://127.0.0.1:9090`，`service.base_url` 为 sub2api 在宿主机上的端口（例如 `http://127.0.0.1:8080`）。sub2api 的 PostgreSQL / Redis 未映射到宿主机时，`service.postgres_dsn` / `redis_addr` 留空，依赖状态由 exporter 经 Prometheus 提供。

### Docker 与发布入口

- `oncall` 用户在 `docker` 组，等同 root 权限；只有动作实现使用 Docker，控制台和模型不能提交任意 Docker 操作。
- `service.release` 的命令以参数数组执行，末尾追加批准的 `repo@sha256:…`；运行时只有 `PATH`，工作目录为 `work_dir`，超时为 `timeout_seconds`。脚本须用该镜像引用更新 Compose 并 `docker compose up -d`，成功时退出 0。
- Agent 以非阻塞方式 `flock` 锁文件；锁被占用即放弃本次回退。`oncall` 用户需要锁文件的写权限，CI/CD 和人工发布必须持有同一把锁。

### 升级与急停

升级 Agent：停服务（SIGTERM 停止各 Worker，被中断的执行在下次启动时按目标实际状态对账，不重放）→ 备份状态库 → 离线执行新迁移 → 替换二进制 → 启动。急停在控制台“自动处置”页由管理员执行，立即阻止新的写操作；已开始的外部操作不能瞬时撤回。

## 3. 监控栈

```sh
cd deploy/monitoring
install -d -m 0700 secrets
printf %s "$AUTH_TOKEN"       > secrets/agent_token        # 与 server.auth_token 相同
printf %s "$HUMAN_WEBHOOK"    > secrets/human_webhook_url
printf %s "$HEARTBEAT_URL"    > secrets/heartbeat_url
chmod 0600 secrets/* && chown -R 65534:65534 secrets       # Alertmanager 以 nobody 运行
install -m 0600 /dev/null .env                              # 填入下表变量
docker compose --env-file .env up -d
```

| `.env` 变量 | 说明 |
| --- | --- |
| `SUB2API_NETWORK` | sub2api Compose 项目的网络名（`docker network ls`），默认 `sub2api_default`；生产为 `sub2api_sub2api-network` |
| `POSTGRES_EXPORTER_DSN` | 只读监控账号的 DSN，例如 `postgresql://monitor:…@sub2api-postgres:5432/postgres?sslmode=disable`。账号只授予 `pg_monitor`：`CREATE ROLE monitor LOGIN PASSWORD '…' CONNECTION LIMIT 3; GRANT pg_monitor TO monitor;` |
| `REDIS_EXPORTER_ADDR` / `REDIS_EXPORTER_PASSWORD` | 默认 `redis://sub2api-redis:6379` |
| `SUB2API_ADMIN_API_KEY` | 与 Agent 共用的管理员密钥（sub2api 只有一个）；exporter 只调用只读 ops 接口 |
| `SUB2API_PROBE_API_KEY` / `SUB2API_PROBE_MODEL` / `SUB2API_PROBE_INTERVAL` | 可选业务探针：专用测试密钥、低成本模型、低频率。每次都会真实请求上游并产生费用 |

exporter 二进制来自上面的构建（`bin/sub2api-exporter`，Compose 以只读方式挂载）。`alerts.yml` 直接使用仓库根目录的规则文件。`SUB2API_ADMIN_API_KEY` 是必填变量，未填时本目录的任何 `docker compose` 命令都会报插值错误，先生成密钥再操作整个栈。

### 告警送达与心跳

- 所有告警送 Agent；`layer=monitoring` 的告警（抓取失败、ops 读取失败、队列滞后、通知失败等）同时直接送人工，因为出问题的可能正是 Agent。`human_webhook_url` 必须能接收 Alertmanager webhook JSON；飞书/企业微信机器人需要一个中转。
- `Watchdog` 始终触发，每分钟发往 `heartbeat_url`。心跳服务放在这台宿主机之外（dead man's switch 类服务），一段时间收不到就通知人工：宿主机、Prometheus 或 Alertmanager 整体失联都由它发现，不依赖同机 Agent 自救。
- Agent 自身存活由 `up{job="oncall-agent"} == 0`（`MonitoringTargetDown`）直接通知人工。sub2api 公网入口建议再配一个宿主机之外的可用性检测。

### 部署检查

真实主机指标（node-exporter 挂载宿主根目录与 PID 命名空间）、容器日志轮转（`max-size 10m × 3`）、Prometheus 保留 15 天 / 5GB、所有端口只在 127.0.0.1、Webhook 鉴权、人工通知与心跳实际送达、状态库备份与恢复演练。开发用的根目录 Compose、`devtoken` 和端口不能作为生产部署。

## 4. 上线顺序（阶段 E）

1. 所有规则 `mode: observe` 运行，核对“自动处置”页的决策、覆盖面和效果报表；这一阶段不产生任何写操作。
2. 在隔离环境完成 §11 演练（真实容器验收见 [tests/acceptance](../tests/acceptance/experiment.md)）后，把单条规则改为 `manual`，同时修改 `rules_version`，重启 Agent 发布。
3. 人工批准的执行稳定后再改为 `auto`。任何一次动作被标错、结果未知、验证失败或复发，都会阻断该规则的自动执行，直到管理员复位。
