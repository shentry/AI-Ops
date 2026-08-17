package ingest

import (
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
)

// Fingerprint returns a deterministic SHA-256 fingerprint for the selected
// labels. An empty fields list selects every label. Keys are sorted and each
// key/value pair is NUL-terminated so label-map iteration order cannot affect
// the result.
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

// FullHash returns the MD5 hash used for full deduplication. Timestamp fields
// are deliberately absent from the canonical payload so repeated deliveries
// of one alert remain full duplicates as their received times change.
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

func canonicalLabels(labels map[string]string) map[string]string {
	if labels == nil {
		return map[string]string{}
	}
	return labels
}
