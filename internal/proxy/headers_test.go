package proxy

import (
	"net/http"
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
