package diagnose

import (
	"regexp"
	"strings"
)

// 脱敏规则（GC-19）：凡可能携带凭据的位置一律打码。
// 注意不碰 64 位十六进制 fingerprint —— 那是诊断要用的身份，不是秘密。
var sanitizeRules = []struct {
	pattern *regexp.Regexp
	replace string
}{
	// Authorization 头（Bearer/Basic 等）。
	{regexp.MustCompile(`(?i)(authorization["'\s:=]+\s*(?:bearer|basic)\s+)[^\s"',}]+`), "${1}***"},
	// key=value 形态的敏感字段：token/api_key/password/secret/cookie/dsn。
	{regexp.MustCompile(`(?i)\b(api[_-]?key|access[_-]?token|auth[_-]?token|token|password|passwd|secret|cookie|dsn)(["'\s]*[:=]["'\s]*)([^\s"',;}]+)`), "${1}${2}***"},
	// URL userinfo：scheme://user:password@host → scheme://user:***@host。
	{regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://[^:/\s"']+:)[^@\s"']+(@)`), "${1}***${2}"},
	// MySQL DSN 形态：user:pass@tcp(host)/db → user:***@tcp(host)/db。
	{regexp.MustCompile(`([\w.-]+):[^@\s]+@(tcp|unix|http|https)\(`), "${1}:***@${2}("},
}

// Sanitize 对任意外部文本脱敏。所有进入日志、数据库、prompt 的
// 外部内容都必须过这一关。
func Sanitize(input string) string {
	out := input
	for _, rule := range sanitizeRules {
		out = rule.pattern.ReplaceAllString(out, rule.replace)
	}
	return out
}

// ToSafeText 把外部内容转成可安全渲染的文本：
// 非法 UTF-8 和二进制字节替换为 U+FFFD，控制字符（除换行/制表）剔除，
// 避免日志注入和终端转义序列污染。
func ToSafeText(input string) string {
	valid := strings.ToValidUTF8(input, "")
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || r == '\r' {
			return r
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, valid)
}
