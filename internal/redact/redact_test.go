package redact

import (
	"strings"
	"testing"
)

func TestSensitiveHeader(t *testing.T) {
	cases := map[string]bool{
		"Authorization":           true,
		"authorization":            true,
		"X-Api-Key":               true,
		"x-api-key":               true,
		"x-vendor-secret":         true,
		"X-Vendor-Secret":         true,
		"Cookie":                  true,
		"Set-Cookie":              true,
		"Bearer-Token":            true,
		"Content-Type":            false,
		"Accept":                  false,
		"X-Request-Id":            false,
	}
	for name, want := range cases {
		if got := SensitiveHeader(name); got != want {
			t.Fatalf("SensitiveHeader(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestRedactStoredHeaders(t *testing.T) {
	raw := `{"Accept":["application/json"],"Authorization":["Bearer sk-secret"],"Content-Type":["application/json"],"X-Api-Key":["abc123"]}`
	got := RedactStoredHeaders(raw)
	if !strings.Contains(got, `"Authorization":["[REDACTED]"]`) || !strings.Contains(got, `"X-Api-Key":["[REDACTED]"]`) {
		t.Fatalf("sensitive headers were not redacted: %s", got)
	}
	if strings.Contains(got, "sk-secret") || strings.Contains(got, "abc123") {
		t.Fatalf("credential leaked through: %s", got)
	}
	if !strings.Contains(got, `"Accept":["application/json"]`) || !strings.Contains(got, `"Content-Type":["application/json"]`) {
		t.Fatalf("non-sensitive headers were altered: %s", got)
	}
}

func TestRedactStoredHeadersEdgeCases(t *testing.T) {
	if got := RedactStoredHeaders(`{"Content-Type":["application/json"]}`); got != `{"Content-Type":["application/json"]}` {
		t.Fatalf("non-sensitive blob was rewritten: %s", got)
	}
	if got := RedactStoredHeaders(`not json`); got != `not json` {
		t.Fatalf("invalid JSON blob was altered: %s", got)
	}
	if got := RedactStoredHeaders(""); got != "" {
		t.Fatalf("empty blob was altered: %q", got)
	}
	already := `{"Authorization":["[REDACTED]"]}`
	if got := RedactStoredHeaders(already); got != already {
		t.Fatalf("already-redacted blob was rewritten: %s", got)
	}
	multi := `{"Authorization":["Bearer a","Bearer b"]}`
	if got := RedactStoredHeaders(multi); strings.Contains(got, "Bearer") {
		t.Fatalf("multiple Authorization values were not all redacted: %s", got)
	}
}
