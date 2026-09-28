package diagnose

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"oncall-agent/internal/tools"
)

// promReplayWindow 是 generatorURL 回放的理想半窗口：告警时刻前后各 15 分钟。
// 实际半窗口会被 range_minutes 上限折半钳制 —— prom_range_query 会把
// 超过上限的窗口截回，不对称的窗口等于丢掉告警后的半段，所以宁可
// 对称收窄，也不让工具悄悄截断（截断后 LLM 看到的是误导性的半段曲线）。
const promReplayWindow = 15 * time.Minute

// promReplayCollector 解析成员告警 generatorURL 里的 PromQL（g0.expr），
// 在 incident 开始时刻前后窗口做 range 回放，让 LLM 看到触发表达式当时的曲线。
type promReplayCollector struct {
	registry   *tools.Registry
	halfWindow time.Duration
}

func NewPromReplayCollector(registry *tools.Registry, rangeMinutes int) Collector {
	halfWindow := promReplayWindow
	if cap := time.Duration(rangeMinutes) * time.Minute / 2; cap < halfWindow {
		halfWindow = cap
	}
	if halfWindow < time.Minute {
		halfWindow = time.Minute
	}
	return promReplayCollector{registry: registry, halfWindow: halfWindow}
}

func (promReplayCollector) Name() string { return "prom_replay" }

// replayTarget 是一条待回放的表达式：来源告警名和该告警自己的 firing 时刻。
type replayTarget struct {
	alertName string
	firingAt  time.Time
}

func (c promReplayCollector) Collect(ctx context.Context, target Target) EvidenceItem {
	// 按 expr 去重，但窗口按每条告警自己的 firing 时刻算（D07 要求
	// "告警时刻前后各 15 分钟"）：incident 的 StartedAt 是首个成员的时间，
	// 晚 20 分钟才 firing 的成员用它算窗口会完全错过触发时刻的曲线。
	// 同一 expr 来自多条告警时取最早的 firing 时刻 —— 那是它第一次触发的现场。
	exprs := make(map[string]replayTarget)
	for _, alert := range target.Alerts {
		expr, err := ExtractGeneratorExpr(alert.GeneratorURL)
		if err != nil || expr == "" {
			continue
		}
		firingAt := alert.StartsAt.UTC()
		if firingAt.IsZero() {
			// 告警没带 startsAt（少见）时退回 incident 开始时刻，聊胜于无。
			firingAt = target.Incident.StartedAt.UTC()
		}
		if existing, seen := exprs[expr]; seen && !firingAt.Before(existing.firingAt) {
			continue
		}
		exprs[expr] = replayTarget{alertName: alert.Name, firingAt: firingAt}
	}
	if len(exprs) == 0 {
		return missingItem(c.Name(), "prometheus:query_range", "no generatorURL with parseable PromQL")
	}
	var body strings.Builder
	failed := 0
	// 排序后逐个回放：同一 incident 渲染出的证据文本稳定，可 diff。
	sortedExprs := make([]string, 0, len(exprs))
	for expr := range exprs {
		sortedExprs = append(sortedExprs, expr)
	}
	sort.Strings(sortedExprs)
	for _, expr := range sortedExprs {
		replay := exprs[expr]
		start := replay.firingAt.Add(-c.halfWindow)
		end := replay.firingAt.Add(c.halfWindow)
		args, _ := json.Marshal(map[string]string{
			"query": expr,
			"start": start.Format(time.RFC3339),
			"end":   end.Format(time.RFC3339),
		})
		out, err := c.registry.Execute(ctx, tools.ToolPromRangeQuery, args)
		fmt.Fprintf(&body, "expr (from %s, firing_at %s): %s\n",
			replay.alertName, replay.firingAt.Format(time.RFC3339), expr)
		if err != nil {
			fmt.Fprintf(&body, "  error: %s\n", err)
			failed++
			continue
		}
		body.WriteString(out)
		body.WriteString("\n")
	}
	return queriesItem(c.Name(), "prometheus:query_range", body.String(), failed, len(sortedExprs))
}

// ExtractGeneratorExpr 从 Prometheus generatorURL 安全解析 g0.expr。
// 只读查询参数，不执行任何内容；URL 不合法或没有 expr 都返回空。
func ExtractGeneratorExpr(generatorURL string) (string, error) {
	if strings.TrimSpace(generatorURL) == "" {
		return "", nil
	}
	parsed, err := url.Parse(generatorURL)
	if err != nil {
		return "", fmt.Errorf("parse generatorURL: %w", err)
	}
	expr := parsed.Query().Get("g0.expr")
	if strings.TrimSpace(expr) == "" {
		return "", errors.New("generatorURL has no g0.expr")
	}
	return expr, nil
}
