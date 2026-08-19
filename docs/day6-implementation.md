# Day6 实现文档：Tool Registry 与 Prometheus 只读工具

> 本文对应 `oncall-agent-开发SPEC.md` 的 D06 和 `docs/14-day-plan/day06-tool-registry.md`。目标：建立工具安全边界（SafetyLevel + Registry），实现三个 Prometheus 只读查询工具，为 D07 Evidence Collector 和 D08 Reasoner 提供稳定接口。
>
> D06 不接 LLM（Eino 适配属于 D08），不实现任何变更类工具（L2/L3 动作属于 D10/D11）。

## 1. Day6 做了什么

- `ToolSpec{Name, Description, Level, Timeout, MaxOutput, Handler}`：没有注册、没有安全等级的动作在系统里不存在；
- `Registry.Register/Get/ForLLM/Execute`：注册期校验契约完整性；`ForLLM()` 只导出 L1 只读工具（GC-08）；`Execute` 是所有调用的统一入口，统一套超时和输出截断；
- 三个 L1 工具：`prom_instant_query`、`prom_range_query`、`prom_series_meta`；
- HTTP client 进程级复用，工具调用不新建连接池；
- 错误摘要只带 Prometheus 的 errorType/error 文本和 HTTP 状态码，URL（可能含凭据）不进错误（GC-19）。

## 2. 安全分级

```go
L1ReadOnly  // 只读，自动执行
L2LowRisk   // 低风险，满足护栏自动执行（D11）
L3Approval  // 必须审批（D10）
L4Forbidden // 永远禁止
```

`ForLLM()` 是 LLM 能看到的全部工具面 —— LLM 编造 L2+ 工具名也调不到，因为 Execute 对未注册/未导出的名字一律 `ErrToolNotRegistered`（不在名单即拒绝）。D08 的 Reasoner 只挂 `ForLLM()`；L2/L3 动作由确定性执行路径按名 `Get` 后走 Policy/审批，不经过 LLM 工具循环。

## 3. 统一纪律：超时与截断

所有调用走 `Registry.Execute`：

```text
context.WithTimeout(spec.Timeout)
    -> handler(ctx, args)
    -> 错误：区分超时（"timed out after Ns"）与 handler 错误
    -> 输出：按 rune 截断到 MaxOutput，追加 …[truncated] 标记
```

截断按 rune 不按 byte，不切半个 UTF-8 字符。MaxOutput 缺省 4096，防止工具漏配。

这套纪律在注册表层只做一次，不在 prompt 层重复（参考实现把截断散在 prompt 拼接里，那是漏管之源）。

## 4. Prometheus 工具

`PrometheusClient` 由 `config.Tools.Prometheus` 构造：base URL 校验 scheme 并剥掉 userinfo；`range_minutes`、`max_points` 必须为正。共享进程级 `http.Client`。

响应体限量是独立的内存兜底（`maxResponseBytes = 4 MiB`，读 limit+1 显式判溢出报错），与给 LLM 的文本截断（MaxOutput）无关 —— 合法 matrix 响应可达数百 KB，绝不能把 body 切在 JSON 中间。最终给调用方的文本仍由 Registry 统一截断。

| 工具 | 参数 | 护栏 |
|---|---|---|
| `prom_instant_query` | `{query, time?}` | time 只接受 RFC3339，拒绝任意字符串透传进 URL |
| `prom_range_query` | `{query, start, end}` | 窗口封顶 range_minutes；step 自适应保证点数 ≤ max_points |
| `prom_series_meta` | `{match[], limit?}` | match 非空逐个校验；limit 客户端裁剪（series API 无 limit 参数），默认 50 |

step 自适应的取整：Prometheus 在 `start, start+step, … ≤ end` 采样，点数含两端即 `1 + floor(range/step)`。要保证点数 ≤ max_points，必须 `step = ceil(range / (maxPoints - 1))`（maxPoints=1 时只剩起点）。用 `range/maxPoints` 会在 inclusive 端点下多出一个点。

响应体先经 `io.LimitReader` 限量读（4×MaxOutput），对端发狂也有内存上限；最终截断仍由 Registry 统一执行。

## 5. 测试覆盖

表驱动单测覆盖：

- Registry：契约校验（空名/空描述/非法等级/无 handler/非正超时）、重名拒绝、`ForLLM` 只出 L1 且按名排序稳定、未注册拒绝、超时纪律、按 rune 截断、handler 错误包装；
- Prometheus：client 构造校验与 userinfo 剥离；instant 的参数校验与 RFC3339 转换；range 的窗口封顶（2h → 15m）与 step 自适应（900s/3 点 → 450s）；series 的 limit 裁剪与 truncated 标记；业务错误摘要不含 URL/凭据；三个工具全部注册为 L1。

真实验收：`TEST_PROMETHEUS_URL=http://127.0.0.1:9090 go test -run TestPrometheusToolsAgainstRealServer ./internal/tools/` 对本地 Prometheus（docker compose）的三个接口各完成一次真实查询，通过。

本次实际执行并通过：

```bash
go test ./...    # 含 TEST_MYSQL_DSN 集成测试
go build ./...
go vet ./...
```

## 6. 边界结论

1. **注册表是唯一的工具面**：D10 的 Policy、D11 的执行器都从同一个 Registry 取契约，不存在第二份工具清单。
2. **L1 与变更的边界**：Prometheus 三件套全是 GET 只读；任何写动作（Docker、数据库变更）从 D06 起就只能以 L2+ 身份注册，永远进不了 `ForLLM()`。
3. **凭据边界**：client 不存 userinfo、错误不含 URL；后续接需要认证的监控源时沿用同一约束。
