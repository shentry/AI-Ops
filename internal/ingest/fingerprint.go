package ingest

import (
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
)

// Fingerprint 是 D02 的告警对象身份：选中 labels 的 SHA-256。
// 不算 status，所以 firing 和 resolved 共用指纹。
//
// fields 为空则用全部 labels。key 排序后拼接，每对用 NUL 结尾，
// map 遍历顺序不会影响结果。若配置字段在这条告警上全缺，
// 会对空串哈希——D03 不能让这种情况悄悄把无关告警并到一起。
func Fingerprint(labels map[string]string, fields []string) string {
	keys := fingerprintKeys(labels, fields)
	var input strings.Builder
	for _, key := range keys {
		input.WriteString(key)
		input.WriteByte('=')
		input.WriteString(labels[key])
		input.WriteByte(0)
	}

	digest := sha256.Sum256([]byte(input.String()))
	return hex.EncodeToString(digest[:])
}

// FullHash 是 D02 的内容身份，给全量去重用。
// 同指纹 + 同哈希 = 全量重复；同指纹 + 不同哈希 = 部分重复（状态/内容变了）。
// StartsAt、EndsAt、ReceivedAt、AlertHash 自身都不参与。
func FullHash(a NormalizedAlert) string {
	payload := struct {
		Source       string            `json:"source"`
		Name         string            `json:"name"`
		Status       string            `json:"status"`
		Severity     int               `json:"severity"`
		Labels       map[string]string `json:"labels"`
		Annotations  map[string]string `json:"annotations"`
		GeneratorURL string            `json:"generator_url"`
		Fingerprint  string            `json:"fingerprint"`
	}{
		Source:       a.Source,
		Name:         a.Name,
		Status:       a.Status,
		Severity:     a.Severity,
		Labels:       canonicalLabels(a.Labels),
		Annotations:  canonicalLabels(a.Annotations),
		GeneratorURL: a.GeneratorURL,
		Fingerprint:  a.Fingerprint,
	}

	encoded, _ := json.Marshal(payload)
	digest := md5.Sum(encoded)
	return hex.EncodeToString(digest[:])
}

// fingerprintKeys 同时接受 "service" 和 "labels.service"。重复字段只算一次。
// 配置了但告警上没有的字段直接跳过，不编成空值。
func fingerprintKeys(labels map[string]string, fields []string) []string {
	if len(fields) == 0 {
		keys := make([]string, 0, len(labels))
		for key := range labels {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		return keys
	}

	selected := make(map[string]struct{}, len(fields))
	for _, field := range fields {
		field = strings.TrimSpace(field)
		field = strings.TrimPrefix(field, "labels.")
		if field == "" {
			continue
		}
		if _, exists := labels[field]; exists {
			selected[field] = struct{}{}
		}
	}

	keys := make([]string, 0, len(selected))
	for key := range selected {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// canonicalLabels 把 nil 收成 {}，JSON 编码写成 {} 而不是 null。
func canonicalLabels(labels map[string]string) map[string]string {
	if labels == nil {
		return map[string]string{}
	}
	return labels
}
