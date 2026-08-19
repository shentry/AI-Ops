# AI-Opus 14 天实施计划

> 项目名称：AI-Opus 智能告警自愈系统  
> V1 目标：对 Sub2API 测试环境中的 Prometheus 告警完成接入、去重、归并、诊断、审批、执行、验证和记忆闭环。  
> 计划基线：Day01-Day14 全部完成，v1.0 收口。

## 全局约束

以下约束适用于 Day01-Day14。它们是系统不变量，不因具体模块或开发阶段改变。

| ID | 约束 |
|---|---|
| GC-01 | Alertmanager 事件未成功写入 `raw_event` 前不得返回 HTTP 202 |
| GC-02 | MySQL 是 AI-Opus 唯一权威状态源；channel 只负责唤醒 worker |
| GC-03 | 所有数据库访问通过 `internal/store`；跨表状态变更必须在同一事务中提交 |
| GC-04 | 告警 `fingerprint` 只由规范化身份 labels 计算；时间、状态和 annotations 不参与 |
| GC-05 | 同一告警的 `firing` 与 `resolved` 必须得到相同 fingerprint |
| GC-06 | full duplicate 不新增告警历史、Incident 成员或 Agent Run，只刷新存活时间 |
| GC-07 | 摄入 worker 与诊断 worker 分离；LLM、审批和外部 API 不得阻塞告警摄入 |
| GC-08 | LLM 只读取脱敏证据并生成 RCA/Plan，不直接持有变更工具 |
| GC-09 | LLM 输出不是权限结论；所有 Plan 必须通过确定性的 Guard 与 Policy |
| GC-10 | L1 只读自动执行；L2 低风险动作满足护栏后自动执行；L3 必须审批；L4 永远禁止 |
| GC-11 | 执行 target 必须映射到真实运行对象，禁止对 LLM 编造的 target 执行 |
| GC-12 | 禁止任意 Shell；执行器只能调用 Tool Registry 中注册的结构化动作 |
| GC-13 | L3 审批绑定 Plan、target、参数和过期时间；执行前必须再次校验 |
| GC-14 | 所有变更动作必须幂等、限频、可超时，并具有明确的最大重试次数 |
| GC-15 | 命令执行成功不等于故障恢复；只有 Verify 通过才能认定修复成功 |
| GC-16 | 只有高置信度且验证成功的案例才能写入故障记忆 |
| GC-17 | Memory 命中不能绕过 Guard、权限判断、审批和 Verify |
| GC-18 | 每次处理都能沿 `raw_event_id -> incident_id -> run_id -> approval_id` 回放 |
| GC-19 | Token、Cookie、DSN、API Key 等敏感内容进入日志、数据库或 Prompt 前必须脱敏 |
| GC-20 | Sub2API 的 Redis 是被监控依赖；AI-Opus V1 自身不依赖 Redis、MQ 或向量数据库 |

每项代码变更的通用完成定义：

- [ ] 当日任务的验收项全部通过；
- [ ] `go test ./...` 通过；
- [ ] `go build ./...` 通过；
- [ ] `go vet ./...` 通过；
- [ ] 新增状态迁移、权限规则和错误分支都有测试；
- [ ] 文档中的“已完成”与仓库实际代码一致。

## 文件结构

以下结构包含当前目录和 Day05-Day14 计划新增文件；实际文件名可在不改变包边界的前提下微调。

```text
.
├── cmd/
│   ├── server/                 # 服务组装与生命周期
│   └── simulate/               # Alertmanager v4 故障造数
├── internal/
│   ├── api/                    # webhook、incident、approval HTTP API
│   ├── config/                 # YAML、环境变量和启动校验
│   ├── store/                  # 唯一数据库访问边界
│   ├── ingest/                 # 解析、指纹、去重、Correlator
│   ├── incident/               # Incident 生命周期和查询
│   ├── diagnose/               # Evidence、Pipeline、Guard、Verify
│   ├── llm/                    # Eino 模型工厂和 Reasoner
│   ├── tools/                  # Registry、Prometheus、Docker 与依赖工具
│   ├── approval/               # 审批、执行 worker 和过期处理
│   ├── memory/                 # 精确故障记忆和命令历史
│   └── notify/                 # 飞书/企业微信通知抽象
├── migrations/                # 手工 SQL migration
├── tests/                     # 后续端到端故障演练测试
├── docs/
│   ├── ai-opus-system-design.md
│   ├── day1-implementation.md  # 已实现内容的详细复盘
│   ├── day2-implementation.md
│   ├── day3-implementation.md
│   ├── day4-implementation.md
│   └── 14-day-plan/            # 本计划及逐日清单
├── config.example.yaml
├── docker-compose.dev.yml
├── prometheus.yml
└── alertmanager.yml
```

## 阶段 0：项目脚手架与基础设施

### 任务 1：Day01 仓库、配置、MySQL 与开发环境

- 状态：已完成，待统一回归。
- 清单：[Day01](day01-foundation.md)
- 产出：可编译 Go 工程、十表 schema、配置加载、MySQL 连接和 Docker Compose 开发环境。

## 阶段 1：P0 告警核心

### 任务 2：Day02 告警归一化、指纹和严重度

- 状态：已完成，待统一回归。
- 清单：[Day02](day02-normalization.md)

### 任务 3：Day03 Webhook、可靠队列和两级去重

- 状态：已完成，待统一回归。
- 清单：[Day03](day03-ingest-dedup.md)

### 任务 4：Day04 Correlator 与 Incident 归并

- 状态：已完成。
- 清单：[Day04](day04-correlation.md)

### 任务 5：Day05 Incident 生命周期、查询 API 和诊断分流

- 状态：已完成。
- 清单：[Day05](day05-incident-lifecycle.md)

## 阶段 2：P1 Agent 诊断与报告

### 任务 6：Day06 Tool Registry 与 Prometheus 工具

- 状态：已完成。
- 清单：[Day06](day06-tool-registry.md)

### 任务 7：Day07 Sub2API Evidence Collector

- 状态：已完成。
- 清单：[Day07](day07-evidence.md)

### 任务 8：Day08 Eino Reasoner 与结构化 Plan

- 状态：已完成。
- 清单：[Day08](day08-reasoner.md)

### 任务 9：Day09 诊断 Pipeline、Guard 和通知

- 状态：已完成。
- 清单：[Day09](day09-pipeline.md)

## 阶段 3：P2 权限审批、执行与验证

### 任务 10：Day10 Policy 与审批生命周期

- 状态：已完成。
- 清单：[Day10](day10-approval.md)

### 任务 11：Day11 Docker Runtime Adapter、L2 执行和 Verify

- 状态：已完成。
- 清单：[Day11](day11-execute-verify.md)

### 任务 12：Day12 验证失败重诊与安全闭环

- 状态：已完成。
- 清单：[Day12](day12-retry-safety.md)

## 阶段 4：P3 故障记忆与项目收口

### 任务 13：Day13 精确故障记忆与命令历史

- 状态：已完成。
- 清单：[Day13](day13-memory.md)

### 任务 14：Day14 Sub2API 故障演练、可观测性与发布

- 状态：已完成。
- 清单：[Day14](day14-release.md)

## 14 天任务检索表

| Day | 核心结果 | 依赖 | 状态 |
|---|---|---|---|
| [01](day01-foundation.md) | 工程骨架、配置、十表、开发依赖 | 无 | 已完成，待回归 |
| [02](day02-normalization.md) | 归一化、fingerprint、hash、severity | Day01 | 已完成，待回归 |
| [03](day03-ingest-dedup.md) | Webhook、raw_event、worker、两级去重 | Day02 | 已完成，待回归 |
| [04](day04-correlation.md) | group key、Incident 归并、阈值促发 | Day03 | 已完成 |
| [05](day05-incident-lifecycle.md) | resolved、查询 API、agent_run 分流 | Day04 | 已完成 |
| [06](day06-tool-registry.md) | 安全工具注册表和 Prometheus 查询 | Day05 | 已完成 |
| [07](day07-evidence.md) | Sub2API、数据库、Redis、主机证据 | Day06 | 已完成 |
| [08](day08-reasoner.md) | Eino Reasoner、RCA 和 Plan JSON | Day07 | 已完成 |
| [09](day09-pipeline.md) | 独立诊断 worker、Guard、报告通知 | Day08 | 已完成 |
| [10](day10-approval.md) | L1-L4 Policy 和 L3 审批 | Day09 | 已完成 |
| [11](day11-execute-verify.md) | Docker 受控执行和恢复验证 | Day10 | 已完成 |
| [12](day12-retry-safety.md) | 有限重诊、人工升级和安全测试 | Day11 | 已完成 |
| [13](day13-memory.md) | 精确记忆、降级和命令历史 | Day12 | 已完成 |
| [14](day14-release.md) | 故障演练、指标、文档和 v1.0 | Day13 | 已完成 |

## 自检清单

- [x] Day01-Day03 在当前工作区重新执行测试、构建和静态检查；
- [x] Day04 验收通过后才将状态改为已完成；
- [x] 每日只实现对应文档列出的范围，不提前混入后续能力；
- [x] 每个阶段结束执行一次从 Alertmanager 到数据库/通知/执行面的阶段性联调；
- [x] 自动动作在测试环境先以 `dry_run=true` 演练（config.example 默认 dry_run=true、auto_execute_l2=false）；
- [x] L3 未审批和 L4 动作均有明确拒绝测试；
- [x] Day14 前至少完成 3 个自动修复、2 个审批和 1 个禁止动作场景（本地等价场景，见 day14 文档 §2）；
- [x] README、配置样例、migration 和实际代码保持一致。
