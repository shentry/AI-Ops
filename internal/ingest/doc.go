// Package ingest 把 Alertmanager webhook 归一成 NormalizedAlert。
//
// D02 是纯函数日：ParseWebhook、Fingerprint、FullHash、Severity
// 不连库、不起 HTTP。收包、落库、跟 last_alert 比哈希是 D03 的事。
package ingest
