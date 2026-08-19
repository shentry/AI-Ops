package diagnose

import (
	"context"
	"fmt"
	"strings"
)

// snapshotCollector 把 incident 成员与当前告警快照落成文本证据：
// 成员列表（fingerprint/名称/当前状态/级别）+ 每条当前告警的
// labels 与 annotations。这是 LLM 理解"发生了什么"的第一手材料。
type snapshotCollector struct{}

func NewSnapshotCollector() Collector {
	return snapshotCollector{}
}

func (snapshotCollector) Name() string { return "alert_snapshot" }

func (c snapshotCollector) Collect(_ context.Context, target Target) EvidenceItem {
	var body strings.Builder
	fmt.Fprintf(&body, "incident: id=%d group_key=%s status=%s severity=%d alerts_count=%d\n",
		target.Incident.ID, target.Incident.GroupKey, target.Incident.Status, target.Incident.Severity, target.Incident.AlertsCount)
	fmt.Fprintf(&body, "started_at=%s last_seen_at=%s\n",
		target.Incident.StartedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		target.Incident.LastSeenAt.UTC().Format("2006-01-02T15:04:05Z07:00"))
	body.WriteString("\nmembers:\n")
	for _, member := range target.Members {
		fmt.Fprintf(&body, "- fingerprint=%s name=%s status=%s severity=%d linked_at=%s\n",
			member.Fingerprint, member.Name, member.Status, member.Severity,
			member.LinkedAt.UTC().Format("2006-01-02T15:04:05Z07:00"))
	}
	for _, alert := range target.Alerts {
		fmt.Fprintf(&body, "\nalert %s (%s):\n", alert.Name, alert.Fingerprint)
		fmt.Fprintf(&body, "  status=%s severity=%d starts_at=%s\n",
			alert.Status, alert.Severity, alert.StartsAt.UTC().Format("2006-01-02T15:04:05Z07:00"))
		writeMap(&body, "labels", string(alert.Labels))
		writeMap(&body, "annotations", string(alert.Annotations))
		if alert.GeneratorURL != "" {
			fmt.Fprintf(&body, "  generator_url: %s\n", alert.GeneratorURL)
		}
	}
	return finishItem(c.Name(), "mysql:incident/last_alert/alert", body.String(), nil)
}

// writeMap 把 JSON map 原文转成 key=value 行。JSON 已经在 store 层验证过合法，
// 这里不再解析 —— 原文渲染，键值内容统一走 finishItem 的脱敏。
func writeMap(body *strings.Builder, title, raw string) {
	fmt.Fprintf(body, "  %s: %s\n", title, raw)
}
