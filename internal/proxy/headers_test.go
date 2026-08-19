package proxy

import (
	"net/http"
	"strings"
	"testing"
)

func TestCopyForwardHeadersDropsHopByHopAndInternal(t *testing.T) {
	src := http.Header{}
	src.Set("Authorization", "Bearer secret")
	src.Set("Content-Type", "application/json")
	src.Set("Accept", "application/json")
	src.Set("Connection", "keep-alive")
	src.Set("X-Forwarded-For", "1.2.3.4")
	src.Set("X-Grok-Trace", "abc")

	dst := http.Header{}
	copyForwardHeaders(dst, src, defaultForwardHeaders)

	if got := dst.Get("Authorization"); got != "Bearer secret" {
		t.Fatalf("Authorization not forwarded: %q", got)
	}
	if got := dst.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type not forwarded: %q", got)
	}
	if got := dst.Get("Accept"); got != "application/json" {
		t.Fatalf("Accept not forwarded: %q", got)
	}
	if dst.Get("Connection") != "" {
		t.Fatal("hop-by-hop Connection header should have been dropped")
	}
	if dst.Get("X-Forwarded-For") != "" {
		t.Fatal("hop-by-hop X-Forwarded-For should have been dropped")
	}
	if dst.Get("X-Grok-Trace") != "" {
		t.Fatal("internal X-Grok-* header should have been dropped")
	}
}

func TestCopyForwardHeadersRespectsAllowlist(t *testing.T) {
	src := http.Header{}
	src.Set("Authorization", "Bearer secret")
	src.Set("X-Custom", "value")

	dst := http.Header{}
	copyForwardHeaders(dst, src, []string{"Authorization"})
	if dst.Get("Authorization") != "Bearer secret" {
		t.Fatalf("allowlisted Authorization was not forwarded: %q", dst.Get("Authorization"))
	}
	if dst.Get("X-Custom") != "" {
		t.Fatalf("non-allowlisted header was forwarded: %q", dst.Get("X-Custom"))
	}
}

func TestSanitizeHeadersRedactsSensitive(t *testing.T) {
	src := http.Header{}
	src.Set("Authorization", "Bearer secret")
	src.Set("X-Api-Key", "abc123")
	src.Set("X-Request-Id", "abc")
	got := sanitizeHeaders(src)
	if got.Get("Authorization") != "[REDACTED]" {
		t.Fatalf("Authorization was not redacted: %q", got.Get("Authorization"))
	}
	if got.Get("X-Api-Key") != "[REDACTED]" {
		t.Fatalf("X-Api-Key was not redacted: %q", got.Get("X-Api-Key"))
	}
	if got.Get("X-Request-Id") != "abc" {
		t.Fatalf("non-sensitive header was altered: %q", got.Get("X-Request-Id"))
	}
}

func TestRedactStoredHeaders(t *testing.T) {
	raw := `{"Accept":["application/json"],"Authorization":["Bearer sk-secret"],"Content-Type":["application/json"],"X-Api-Key":["abc123"]}`
	got := redactStoredHeaders(raw)
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
	if got := redactStoredHeaders(`{"Content-Type":["application/json"]}`); got != `{"Content-Type":["application/json"]}` {
		t.Fatalf("non-sensitive blob was rewritten: %s", got)
	}
	if got := redactStoredHeaders(`not json`); got != `not json` {
		t.Fatalf("invalid JSON blob was altered: %s", got)
	}
	if got := redactStoredHeaders(""); got != "" {
		t.Fatalf("empty blob was altered: %q", got)
	}
	already := `{"Authorization":["[REDACTED]"]}`
	if got := redactStoredHeaders(already); got != already {
		t.Fatalf("already-redacted blob was rewritten: %s", got)
	}
	multi := `{"Authorization":["Bearer a","Bearer b"]}`
	if got := redactStoredHeaders(multi); strings.Contains(got, "Bearer") {
		t.Fatalf("multiple Authorization values were not all redacted: %s", got)
	}
}
