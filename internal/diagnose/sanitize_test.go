package diagnose

import (
	"strings"
	"testing"
)

func TestSanitize(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		contains string
		excludes []string
	}{
		{
			name:     "bearer token",
			input:    `Authorization: Bearer abc123.def456`,
			contains: "Bearer ***",
			excludes: []string{"abc123"},
		},
		{
			name:     "api key pair",
			input:    `api_key: "sk-live-12345"`,
			contains: "api_key",
			excludes: []string{"sk-live-12345"},
		},
		{
			name:     "dsn key value",
			input:    `dsn: postgres://app:s3cret@db.internal:5432/sub2api`,
			contains: "dsn: ***",
			excludes: []string{"s3cret"},
		},
		{
			name:     "url userinfo in text",
			input:    `dial postgres://app:s3cret@db.internal:5432/sub2api failed`,
			contains: "postgres://app:***@db.internal",
			excludes: []string{"s3cret"},
		},
		{
			name:     "mysql dsn",
			input:    `open oncall:passw0rd@tcp(10.0.0.8:3306)/oncall failed`,
			contains: "oncall:***@tcp(10.0.0.8:3306)",
			excludes: []string{"passw0rd"},
		},
		{
			name:     "cookie",
			input:    `Cookie: session=deadbeef`,
			contains: "Cookie: ***",
			excludes: []string{"deadbeef"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := Sanitize(test.input)
			if !strings.Contains(got, test.contains) {
				t.Fatalf("Sanitize() = %q, want containing %q", got, test.contains)
			}
			for _, excluded := range test.excludes {
				if strings.Contains(got, excluded) {
					t.Fatalf("Sanitize() = %q leaks %q", got, excluded)
				}
			}
		})
	}
}

func TestSanitizeKeepsFingerprint(t *testing.T) {
	// 64 位十六进制 fingerprint 是诊断身份，不是秘密，不能被误伤。
	fingerprint := strings.Repeat("a1", 32)
	if got := Sanitize("fingerprint=" + fingerprint); !strings.Contains(got, fingerprint) {
		t.Fatalf("Sanitize() mangled fingerprint: %q", got)
	}
}

func TestToSafeText(t *testing.T) {
	// 非法 UTF-8 与控制字符被清理，换行保留。
	input := "ok\n" + string([]byte{0xff, 0x00, 0x1b}) + "tail"
	got := ToSafeText(input)
	if !strings.HasPrefix(got, "ok\n") || !strings.HasSuffix(got, "tail") {
		t.Fatalf("ToSafeText() = %q", got)
	}
	if strings.ContainsRune(got, 0x1b) || strings.ContainsRune(got, 0x00) {
		t.Fatalf("ToSafeText() kept control chars: %q", got)
	}
}
