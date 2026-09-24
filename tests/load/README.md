# 隔离环境 k6 压测

本目录验证 **Webhook 持久化接收 → MySQL 异步摄入**，不验证 LLM、通知、审批或修复吞吐。实测报告见 [2026-09-17 压测报告](../../docs/load-test-2026-09-17.md)。

## 文件

- `compose.yaml`：专用 MySQL、server 和临时 k6 容器；固定镜像、资源限额、日志轮转。
- `config.bench.yaml`：按 service 归并，`min_alerts=1`，`info → skip`，禁用外部集成和真实修复。
- `alertmanager.k6.js`：开放模型恒定到达率；duplicate/unique 两类告警，加每秒一次控制台分页读取。
- `run.py`：只在已部署的 Linux/cgroup-v2 隔离环境中运行；采样数据库和主机、保护 Sub2API、自动停止、等待排空、核对数据形状并保存结果。
- `test_run.py`：停止线回归测试，无 Docker、网络或数据库依赖。

这是一套针对既有 AWS 测试机的脚本，不是通用部署工具。runner 校验 `oncall.scope=aws-k6-20260917`，使用固定的 benchmark 容器名，并检查本机 Sub2API `/health`。

## 已部署的位置

```text
主机：<bench-host>
目录：/home/ubuntu/oncall-agent-current
服务：http://127.0.0.1:18081（仅服务器回环）
数据库：oncall_benchmark（不发布数据库端口）
凭据：部署目录中的 .env，权限 0600，不纳入仓库
```

目录还包含当前源码构建的 Linux/amd64 `oncall-server`、`build-manifest.json`、`migrations/001–011` 和 `results/`。MySQL 初始化挂载的迁移只在**空数据卷首次启动**时执行，不能把已有数据卷重建当作升级方式。

容器配额：

| 服务 | CPU 上限 | 内存上限 |
|---|---:|---:|
| MySQL | 0.80 核 | 768 MiB |
| server | 0.60 核 | 384 MiB |
| k6 | 0.25 核 | 256 MiB |

无 Docker socket 或 Sub2API 数据挂载；server 为非 root、只读根文件系统。日志每文件 10 MiB、最多 3 个文件。**这些是测试环境的保护措施，不代表应用入口已经实现限流或背压。**

## 运行一个阶段

先登录已有测试部署，再逐阶段运行；前一阶段失败时先检查结果，不要继续加压：

```sh
ssh -i "<your-key>.pem" ubuntu@<bench-host>
cd /home/ubuntu/oncall-agent-current

PYTHONDONTWRITEBYTECODE=1 python3 -m unittest test_run -v

# 参数：模式 Webhook/秒 持续秒数 每个Webhook的告警条数
python3 run.py duplicate 50 20 1
python3 run.py unique 25 20 10
```

每次运行自动生成不同的 RUN_ID 和合法 UTC `startsAt`，不会覆盖旧结果。允许范围：1–250 Webhook/s、5–60 秒、1–10 条告警/包。不同阶段串行执行，开始前要求旧 pending 已排空；保留历史测试数据，不执行 TRUNCATE、删卷或全局 prune。

本地语法与回归检查：

```sh
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s tests/load -p test_run.py -v
node --check tests/load/alertmanager.k6.js
```

### 停止条件

- 主机可用内存低于 768 MiB，或根盘可用空间低于 2 GiB。
- Sub2API 健康请求失败，或耗时超过 1 秒。
- MySQL 连接数超过 80；pending 超过 5,000 条或最老 pending 超过 60 秒；出现 failed 报文。
- 任一测试容器工作集超过内存上限的 95%，或出现 OOM kill。
- 主机忙碌 CPU 连续两个采样超过 90%。
- k6 自身 HTTP 成功率/P95 阈值不通过、发压超时；停压后 120 秒仍未排空。

工作集按 Docker cgroup-v2 口径计算：`memory.current - memory.stat.inactive_file`。原始总内存、inactive_file、工作集、内存事件均写入采样文件；不把可回收文件缓存直接当作进程耗尽内存。CPU/内存硬限额不因这个统计口径而改变。

runner 退出时只停止、移除自己创建且标签匹配的 k6 容器；不会停止数据库、server 或 Sub2API。主机/SQL/健康采样失败也会停止发压。

## 结果与统计口径

每阶段保存到部署目录 `results/<RUN_ID>.*`：

| 文件 | 内容 |
|---|---|
| `.summary.json` | k6 原始汇总、实际接受数量/速率、延迟、错误和 dropped_iterations |
| `.monitor.jsonl` | 约 2 秒一次采样，另含查询耗时；已提交 processed 数、pending/failed、队首年龄、连接/锁、CPU、内存、健康 |
| `.result.json` | 前后计数、阶段参数、停止原因、k6 退出码、容器身份/重启检查、数据形状/副作用检查 |
| `.metrics-before.txt` / `.metrics-after.txt` | 应用指标前后值 |
| `.k6.log` | k6 日志 |

必须同时检查 HTTP 和消费端，不能仅看 k6 退出码或 202：

1. 客户端 202 数、raw_event 持久化数、最终 processed 数一致，pending/failed 为零。
2. duplicate 每轮仅创建一个 incident/run 和每包条数对应的 alert；unique 每个 Webhook 创建一个 incident/run、每条告警产生一个新指纹。
3. 所有 run 为 `skip/succeeded`、tokens/steps/approvals 为零。
4. `dropped_iterations=0` 才能确认发生器实际维持了目标到达率。
5. 使用已提交 processed 数的采样差分计算消费速度。当前 `processed_at` 在 apply 事务开始前取值，**不是精确事务提交时间，不能用于端到端延迟 SLO**。

列表负载使用 `/api/v1/control-room/incidents?limit=20`。旧 `/api/v1/incidents?limit=20` 实际忽略 limit、返回全量，不能替代为“分页读取”来比较容量。

## 访问控制台

在本机建立 SSH 隧道：

```sh
ssh -i "<your-key>.pem" -N \
  -L 127.0.0.1:18081:127.0.0.1:18081 ubuntu@<bench-host>
```

打开 <http://127.0.0.1:18081>。控制台为匿名模式，因此保持回环监听，不要直接公开这个压测部署。模型状态接口返回 503 是此配置未启用 LLM 的预期限制；不要点击重诊/提问后将其结果作为本轮摄入压测的一部分。

生产接入、真实 LLM/通知配置及公网访问均不在本次部署验收范围内。
