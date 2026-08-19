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

func (c promReplayCollector) Collect(ctx context.Context, target Target) EvidenceItem {
	exprs := make(map[string]string) // expr -> 来源 alert，去重
	for _, alert := range target.Alerts {
		expr, err := ExtractGeneratorExpr(alert.GeneratorURL)
		if err == nil && expr != "" {
			exprs[expr] = alert.Name
		}
	}
	if len(exprs) == 0 {
		return missingItem(c.Name(), "prometheus:query_range", "no generatorURL with parseable PromQL")
	}
	end := target.Incident.StartedAt.UTC().Add(c.halfWindow)
	start := target.Incident.StartedAt.UTC().Add(-c.halfWindow)
	var body strings.Builder
	var firstErr error
	// 排序后逐个回放：同一 incident 渲染出的证据文本稳定，可 diff。
	sortedExprs := make([]string, 0, len(exprs))
	for expr := range exprs {
		sortedExprs = append(sortedExprs, expr)
	}
	sort.Strings(sortedExprs)
	for _, expr := range sortedExprs {
		alertName := exprs[expr]
		args, _ := json.Marshal(map[string]string{
			"query": expr,
			"start": start.Format(time.RFC3339),
			"end":   end.Format(time.RFC3339),
		})
		out, err := c.registry.Execute(ctx, tools.ToolPromRangeQuery, args)
		fmt.Fprintf(&body, "expr (from %s): %s\n", alertName, expr)
		if err != nil {
			fmt.Fprintf(&body, "  error: %s\n", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		body.WriteString(out)
		body.WriteString("\n")
	}
	// 全部 expr 都失败才算 collector 失败；部分失败已在正文留痕。
	if firstErr != nil && len(exprs) == 1 {
		return finishItem(c.Name(), "prometheus:query_range", "", firstErr)
	}
	return finishItem(c.Name(), "prometheus:query_range", body.String(), nil)
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
