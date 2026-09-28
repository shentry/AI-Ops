# 诊断效果评测

通过当前项目的 `Evidence.Render → Reasoner.Diagnose → Guard` 路径调用真实模型。
这是 **12 个人工构造的证据快照案例**，不是历史生产故障集，也不是故障注入或修复成功率测试。
标准答案只供评审，不会发送给被测模型。此次新增不修改产品提示词或业务实现。

已完成的真实模型运行见 [2026-09-22 DeepSeek V4 Flash 评测报告](../../docs/effectiveness-baseline-2026-09-22.md)，
包含 12 个案例各 3 次的原始结果和逐例审阅。
评测原理、结论边界及面试问答见 [详细分析报告](../../docs/effectiveness-analysis-and-interview-2026-09-22.md)。

## 运行

在仓库根目录执行。先在测试进程环境中配置模型凭据，勿将密钥提交到仓库。
使用现有 `config.yaml` 时只读取 `llm` 配置和它引用的环境变量；不连接数据库、Docker、飞书或线上监控。

```sh
# 无外部调用：校验案例规格、自动检查器和已归档计划的 Policy 回放。
go test -count=1 -race ./tests/effectiveness

# 真实模型：默认每个案例一次，共 12 次诊断；需要 config.yaml 引用的 ARK_KEY。
# 输出文件必须不存在，避免覆盖上一轮证据。真实模型调用可能计费。
EFFECT_EVAL=1 EFFECT_EVAL_CONFIG="$PWD/config.yaml" \
  EFFECT_EVAL_OUTPUT=/tmp/oncall-effectiveness-first.jsonl \
  go test -count=1 -v -timeout=30m ./tests/effectiveness
```

对比排查技能时，另加 `EFFECT_EVAL_SKILLS=1`：按案例的 `alerts` 匹配技能并放在证据之前，
与流水线使用同一套匹配和渲染代码，每条结果的 `skills` 字段记录实际注入的技能。开、关两组须使用同一模型和同样的重复次数。

对比知识库时另加 `EFFECT_EVAL_KNOWLEDGE=1`，并用 `TEST_KNOWLEDGE_MYSQL_DSN` 指向一个已迁移、专用的库：评测把本次构建的仓库手册同步进去，并注册 `knowledge_search`／`knowledge_read`。每条结果的 `variant` 记录启用了哪些能力（如 `skills+knowledge`）。

同一个库还用于 `TestKnowledgeRecall`：20 条中文问法（`knowledge_recall.json`）在真实 ngram 检索上的 recall@5／recall@1／MRR，CI 必跑，不调用模型。

也可不指定 `EFFECT_EVAL_CONFIG`，改用现有测试约定的
`TEST_LLM_BASE_URL`、`TEST_LLM_API_KEY`、`TEST_LLM_MODEL` 环境变量。
此方式使用 2048 输出 Token 上限和非思考模式；配置文件方式保留该文件的模型参数。
普通 `go test` 默认跳过真实调用；显式设置 `EFFECT_EVAL=1` 后缺少凭据会失败。

确认首轮能运行后，可设置 `EFFECT_EVAL_REPEATS=3` 做 36 次诊断，输出换一个新文件，
并把命令总超时改为 `-timeout=90m`。每次诊断最多 120 秒，顺序执行；连续三次模型执行错误后停止。
通过 `-run 'TestReasonerEffectiveness/案例ID'` 可以只重跑指定案例，完整基准比较须运行全部案例。

## 评测内容与判定

覆盖：进程退出、缺少对象标识的 OOM、配置缺失、数据库认证、Redis 不可达、
数据库连接槽位耗尽、上游限流、磁盘写满、证据不足、已恢复事件、无关 OOM 噪声、日志提示注入。

每次保存原始 RCA、置信度、证据引用、建议动作、Guard 结果、工具轨迹、Token 用量和总耗时。
原始计划与 Guard 结果分开记录，避免规则拦截掩盖模型自身的问题。

自动检查只判断：

- 引用的证据段落是否存在，是否包含该案例要求的关键段落。
- 建议动作与目标是否在该案例允许的范围内。
- 证据不足等指定案例是否按要求降低置信度。

**自动检查通过不等于根因正确或证据支持充分。** 所有输出初始标记
`diagnosis_review=pending_human_review`。评审必须阅读 `expected_diagnosis`、案例证据与 RCA，
分别记录完整正确、部分正确、错误或合理弃答，并附具体依据；不能把本检查器的通过率标为根因准确率。
没有可靠根因的案例单列弃答质量，不与可回答案例混算。

## 边界

- 工具只有冻结快照中的指标元数据发现，返回无额外序列；未测试完整生产工具集的查询能力。
- 没有调用变更工具、审批执行器或线上数据源，Guard 结果也不代表生产 Policy 已准许执行。
- `TestRecordedPlansPolicyReplay` 另行将归档的 OOM 计划送入真实 Policy，在固定测试配置下检查决策；没有执行变更。
- 此处测的是诊断总耗时，不是首次正确定位时间，更不是 MTTR。
- 模型失败与超时保留为失败；未运行的案例不可计为通过。模型异常时底层可能没有返回 Token 统计，缺失不可算零成本。
- 没有人工值班对照组、实际业务恢复观察或修复执行，因此不报告节省人时、修复率或 MTTR 改善。
- 小样本且人工构造，仅用于建立初始回归基线；后续应补入脱敏、标注、未参与调优的真实故障。
