package incident

import "strings"

// 重诊审计文本。准入检查只在 store.RequestRun 的父 Incident 行锁内执行。

// 重诊事件文本的长度上限。原因进 payload 给的余量大些，
// 摘要要塞进 event.summary 列，收得更紧。
const (
	MaxRetryReasonRunes  = 1024
	MaxRetrySummaryRunes = 512
)

// NormalizeRetryReason 兜底并截断失败原因。空原因给一个确定的默认值，
// 而不是留空 —— 事后回放时「没写原因」和「原因是空字符串」无法区分。
// 调用方负责先完成脱敏，这里只管长度。
func NormalizeRetryReason(reason string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "verification failed"
	}
	return TruncateRunes(reason, MaxRetryReasonRunes)
}

// RetryQueuedSummary / RetryScheduledSummary 是重诊落库时同批写入的两条
// 事件摘要。放在一起是为了让两条文本的措辞不会各自漂移。
func RetryQueuedSummary(reason string) string {
	return TruncateRunes("retry run queued: "+reason, MaxRetrySummaryRunes)
}

func RetryScheduledSummary(reason string) string {
	return TruncateRunes("retry scheduled: "+reason, MaxRetrySummaryRunes)
}

// TruncateRunes 按 rune 截断，不在多字节字符中间切开，并留下
// 显式的截断标记 —— 事后回放时能看出「这里被截过」，
// 而不是把半句话当成完整原因。标记本身放不下时退化为硬截。
func TruncateRunes(text string, maxRunes int) string {
	if maxRunes < 1 {
		return ""
	}
	runes := []rune(text)
	if len(runes) <= maxRunes {
		return text
	}
	const suffix = "…[truncated]"
	suffixRunes := []rune(suffix)
	if len(suffixRunes) >= maxRunes {
		return string(runes[:maxRunes])
	}
	return string(runes[:maxRunes-len(suffixRunes)]) + suffix
}
