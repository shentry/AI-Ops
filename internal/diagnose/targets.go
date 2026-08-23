package diagnose

import (
	"encoding/json"
	"sort"
	"strings"

	"oncall-agent/internal/store"
)

// targetLabelKeys 是"真实运行对象"可能出现的告警标签键。L2 自动路径要求
// plan.Target.Name 来自告警标签、服务清单或运行时查询（GC-11），这里提供
// 第一类来源：只有 incident 自己的告警指到过的对象才算可信。
// 键是白名单而不是"全部标签"：alertname/severity/team 这类标签不是运行对象，
// 拿它们做 target 会让护栏形同虚设。
var targetLabelKeys = []string{
	"container", "container_name", "daemonset", "deployment", "instance",
	"job", "node", "pod", "service", "statefulset", "target",
}

// KnownTargets 汇总 incident 告警标签里出现过的运行对象名，排序去重。
// 返回空表示"没有可信 target 来源" —— 调用方（Policy）应据此拒绝自动执行，
// 而不是放行。
func KnownTargets(target Target) []string {
	seen := make(map[string]bool)
	for _, alert := range target.Alerts {
		for _, value := range labelValues(alert, targetLabelKeys) {
			seen[value] = true
		}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// labelValues 取一条告警里指定标签键的非空值。labels 不是合法 JSON 时
// 返回空（证据层已经容忍脏数据，这里不因为一条坏标签让整条链失败）。
func labelValues(alert store.Alert, keys []string) []string {
	if len(alert.Labels) == 0 {
		return nil
	}
	var labels map[string]string
	if err := json.Unmarshal(alert.Labels, &labels); err != nil {
		return nil
	}
	values := make([]string, 0, len(keys))
	for _, key := range keys {
		if value := strings.TrimSpace(labels[key]); value != "" {
			values = append(values, value)
		}
	}
	return values
}
