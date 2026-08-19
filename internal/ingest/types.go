package ingest

import "time"

const (
	// SourceAlertmanager 是 D02 唯一来源。D03 的 simulate 走同一套格式。
	SourceAlertmanager = "alertmanager"

	// DefaultSeverityLabel 是 ParseWebhook 用的标签名。D03 按配置重算。
	DefaultSeverityLabel = "severity"
)

// NormalizedAlert 是 D02 归一化后的一条 Alertmanager 告警。
//
// payload 字段：Status、Labels、Annotations、StartsAt、EndsAt、GeneratorURL。
// 派生字段：Source、Name、Severity、Fingerprint、AlertHash、ReceivedAt。
type NormalizedAlert struct {
	Source string
	Name   string // 小写化后的 labels["alertname"]
	Status string // firing | resolved；未知值直接拒绝
	// Labels / Annotations 的 key 转小写，value 保持原样。
	// 缺失时变成空 map，不是 nil。
	Labels       map[string]string
	Annotations  map[string]string
	StartsAt     time.Time // RFC3339 → UTC；空串 / 0001-01-01 保持零值
	EndsAt       time.Time
	GeneratorURL string    // 留给 D07 回放 PromQL；这里不校验
	Severity     int       // 5..1；解析器先用 DefaultSeverityLabel
	Fingerprint  string    // labels 的 SHA-256；不含 status，所以 firing/resolved 同指纹
	AlertHash    string    // MD5 FullHash；不含时间字段
	ReceivedAt   time.Time // D03 worker 填写，ParseWebhook 不填
}
