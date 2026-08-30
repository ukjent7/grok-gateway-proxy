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

// Content-Encoding and Content-Range are end-to-end representation headers,
// not hop-by-hop: dropping them would strand a compressed or partial body on
// the client with no way to decode it. Reachable when a gateway's
// ForwardHeaders opts into Accept-Encoding, which disables the Go transport's
// transparent gzip handling.
func TestCopyResponseHeadersKeepsContentEncoding(t *testing.T) {
	src := http.Header{}
	src.Set("Content-Encoding", "gzip")
	src.Set("Content-Range", "bytes 0-99/200")
	src.Set("Transfer-Encoding", "chunked")
	src.Set("Content-Length", "42")

	dst := http.Header{}
	copyResponseHeaders(dst, src)

	if got := dst.Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding was dropped: %q", got)
	}
	if got := dst.Get("Content-Range"); got != "bytes 0-99/200" {
		t.Fatalf("Content-Range was dropped: %q", got)
	}
	if dst.Get("Transfer-Encoding") != "" {
		t.Fatal("hop-by-hop Transfer-Encoding should have been dropped")
	}
	if dst.Get("Content-Length") != "" {
		t.Fatal("Content-Length should have been dropped: the proxy re-frames bodies")
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

// The headers grok-build actually sends must stay off a third-party upstream
// by default. These are the real names from
// crates/codegen/xai-grok-sampler/src/client.rs (GrokRequestHeaders::apply),
// not placeholders: several carry identifiers that should not leak unbidden.
func TestCopyForwardHeadersDropsRealGrokBuildHeadersByDefault(t *testing.T) {
	src := http.Header{}
	for _, name := range []string{
		"X-Grok-Conv-Id", "X-Grok-Req-Id", "X-Grok-Session-Id",
		"X-Grok-Agent-Id", "X-Grok-User-Id", "X-Grok-Deployment-Id",
		"X-Grok-Model-Override", "X-Grok-Turn-Idx", "X-Grok-Transient-Retry",
		"X-Grok-Client-Version", "X-Grok-Client-Identifier",
		"X-Grok-Doom-Loop-Check", "X-Grok-Exact-Repetition-Check",
	} {
		src.Set(name, "v")
	}

	dst := http.Header{}
	copyForwardHeaders(dst, src, defaultForwardHeaders)

	for name := range src {
		if dst.Get(name) != "" {
			t.Fatalf("default allowlist forwarded %s (value %q)", name, dst.Get(name))
		}
	}
}

// Default-deny must remain configurable: naming an x-grok-* header in a
// gateway's ForwardHeaders has to actually forward it. The blanket prefix
// block used to short-circuit before the allowlist was consulted, so this
// silently did nothing.
func TestCopyForwardHeadersAllowlistOptsInGrokHeaders(t *testing.T) {
	src := http.Header{}
	src.Set("X-Grok-Doom-Loop-Check", "64")
	src.Set("X-Grok-Exact-Repetition-Check", "64")
	src.Set("X-Grok-User-Id", "u-1")
	src.Set("Authorization", "Bearer secret")

	dst := http.Header{}
	copyForwardHeaders(dst, src, []string{"Authorization", "X-Grok-Doom-Loop-Check", "x-grok-exact-repetition-check"})

	if got := dst.Get("X-Grok-Doom-Loop-Check"); got != "64" {
		t.Fatalf("allowlisted doom-loop opt-in not forwarded: %q", got)
	}
	// The allowlist is case-insensitive on the config side too.
	if got := dst.Get("X-Grok-Exact-Repetition-Check"); got != "64" {
		t.Fatalf("allowlisted repetition opt-in not forwarded: %q", got)
	}
	if got := dst.Get("Authorization"); got != "Bearer secret" {
		t.Fatalf("Authorization not forwarded: %q", got)
	}
	// Opt-in is per header, not a blanket switch.
	if got := dst.Get("X-Grok-User-Id"); got != "" {
		t.Fatalf("non-allowlisted x-grok-user-id was forwarded: %q", got)
	}
}
