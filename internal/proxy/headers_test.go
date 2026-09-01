package proxy

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"grok-gateway-proxy/internal/config"
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
	// The proxy owns the upstream Content-Type: it builds the body, so the
	// client's declaration is never copied.
	if got := dst.Get("Content-Type"); got != "" {
		t.Fatalf("client Content-Type was copied: %q", got)
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

// The session dialects are alternatives, not additions: an upstream that
// routes on `x-session-id` must not also be told a `session_id`, and the
// OpenAI spelling must not be sent alongside the OpenRouter one.
func TestApplyGrokHeaderTranslationsSessionDialects(t *testing.T) {
	src := http.Header{}
	src.Set("X-Grok-Session-Id", "sess-123")

	cases := []struct {
		mode            string
		wantSessionID   string
		wantClientReqID string
		wantXSessionID  string
	}{
		{config.SessionAffinityOpenAI, "sess-123", "sess-123", ""},
		{config.SessionAffinityOpenRouter, "", "", "sess-123"},
		{config.SessionAffinityOff, "", "", ""},
		// An unset or unknown mode resolves to the OpenAI dialect.
		{"", "sess-123", "sess-123", ""},
		{"nonsense", "sess-123", "sess-123", ""},
	}
	for _, tc := range cases {
		dst := http.Header{}
		applyGrokHeaderTranslations(dst, src, config.GatewayConfig{SessionAffinity: tc.mode}, "", false)
		if got := dst.Get("session_id"); got != tc.wantSessionID {
			t.Fatalf("mode %q: session_id = %q, want %q", tc.mode, got, tc.wantSessionID)
		}
		if got := dst.Get("X-Client-Request-Id"); got != tc.wantClientReqID {
			t.Fatalf("mode %q: X-Client-Request-Id = %q, want %q", tc.mode, got, tc.wantClientReqID)
		}
		if got := dst.Get("X-Session-Id"); got != tc.wantXSessionID {
			t.Fatalf("mode %q: X-Session-Id = %q, want %q", tc.mode, got, tc.wantXSessionID)
		}
	}
}

// The audit row's id is what the console shows, so it is what an upstream
// log has to name. grok's per-turn id goes out as X-Correlation-Id, which
// links the exchange back to the client instead.
func TestApplyGrokHeaderTranslationsRequestIDs(t *testing.T) {
	src := http.Header{}
	src.Set("X-Grok-Req-Id", "req-456")
	dst := http.Header{}
	applyGrokHeaderTranslations(dst, src, config.GatewayConfig{}, "req-audit-1", false)

	if got := dst.Get("X-Request-Id"); got != "req-audit-1" {
		t.Fatalf("X-Request-Id = %q, want the audit id", got)
	}
	if got := dst.Get("X-Correlation-Id"); got != "req-456" {
		t.Fatalf("X-Correlation-Id = %q, want grok's per-turn id", got)
	}
	if dst.Get("X-Grok-Req-Id") != "" {
		t.Fatal("raw x-grok-req-id must not be forwarded")
	}
}

// A caller that already speaks the standard protocol keeps its own ids: the
// translation fills gaps, it does not overwrite.
func TestApplyGrokHeaderTranslationsPreservesExisting(t *testing.T) {
	src := http.Header{}
	src.Set("X-Grok-Session-Id", "new-sess")
	src.Set("X-Grok-Req-Id", "new-req")
	dst := http.Header{}
	dst.Set("session_id", "existing")
	dst.Set("X-Client-Request-Id", "existing-req")
	dst.Set("X-Request-Id", "existing-id")
	applyGrokHeaderTranslations(dst, src, config.GatewayConfig{}, "req-audit-1", false)

	if got := dst.Get("session_id"); got != "existing" {
		t.Fatalf("existing session_id was overwritten: %q", got)
	}
	if got := dst.Get("X-Client-Request-Id"); got != "existing-req" {
		t.Fatalf("existing X-Client-Request-Id was overwritten: %q", got)
	}
	if got := dst.Get("X-Request-Id"); got != "existing-id" {
		t.Fatalf("existing X-Request-Id was overwritten: %q", got)
	}
}

func TestApplyGrokHeaderTranslationsFallsBackToConvId(t *testing.T) {
	src := http.Header{}
	src.Set("X-Grok-Conv-Id", "conv-only")
	dst := http.Header{}
	applyGrokHeaderTranslations(dst, src, config.GatewayConfig{}, "", false)
	if got := dst.Get("session_id"); got != "conv-only" {
		t.Fatalf("fallback to conv-id failed: %q", got)
	}
}

func TestApplyGrokHeaderTranslationsSetsUserAgentAndAccept(t *testing.T) {
	src := http.Header{}
	src.Set("User-Agent", "grok-shell/1.2.3 (windows; x86_64)")
	dst := http.Header{}
	applyGrokHeaderTranslations(dst, src, config.GatewayConfig{}, "", false)
	if got := dst.Get("User-Agent"); got != "grok-shell/1.2.3 (windows; x86_64)" {
		t.Fatalf("User-Agent not preserved: %q", got)
	}
	if got := dst.Get("Accept"); got != "application/json" {
		t.Fatalf("Accept default not set: %q", got)
	}
	// Falls back to x-grok-client-* when no User-Agent.
	src2 := http.Header{}
	src2.Set("X-Grok-Client-Identifier", "grok-shell")
	src2.Set("X-Grok-Client-Version", "9.9.9")
	dst2 := http.Header{}
	applyGrokHeaderTranslations(dst2, src2, config.GatewayConfig{}, "", false)
	if got := dst2.Get("User-Agent"); got != "grok-shell/9.9.9" {
		t.Fatalf("User-Agent from x-grok-client-* failed: %q", got)
	}
}

// Accept has to match the representation the upstream is being asked for: a
// streaming call is answered with SSE, and announcing JSON lets an upstream
// pick the buffered form.
func TestApplyGrokHeaderTranslationsAcceptTracksStream(t *testing.T) {
	streaming := http.Header{}
	applyGrokHeaderTranslations(streaming, http.Header{}, config.GatewayConfig{}, "", true)
	if got := streaming.Get("Accept"); got != "text/event-stream" {
		t.Fatalf("streaming Accept = %q, want text/event-stream", got)
	}

	buffered := http.Header{}
	applyGrokHeaderTranslations(buffered, http.Header{}, config.GatewayConfig{}, "", false)
	if got := buffered.Get("Accept"); got != "application/json" {
		t.Fatalf("non-streaming Accept = %q, want application/json", got)
	}
}

func TestApplyGrokHeaderTranslationsUserAgentFallbackNamesVersion(t *testing.T) {
	dst := http.Header{}
	applyGrokHeaderTranslations(dst, http.Header{}, config.GatewayConfig{}, "", false)
	if got := dst.Get("User-Agent"); got != config.DefaultUserAgentOverride {
		t.Fatalf("User-Agent fallback = %q, want %q", got, config.DefaultUserAgentOverride)
	}
}

func TestInjectPromptCacheKey(t *testing.T) {
	body := []byte(`{"model":"m","input":"hi"}`)
	out, changed := injectPromptCacheKey(body, "sess-abc")
	if !changed {
		t.Fatal("expected injection to change body")
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	var got string
	if err := json.Unmarshal(m["prompt_cache_key"], &got); err != nil || got != "sess-abc" {
		t.Fatalf("injected value wrong: %s", out)
	}
}

func TestInjectPromptCacheKeyPreservesExisting(t *testing.T) {
	body := []byte(`{"model":"m","prompt_cache_key":"keep","input":"hi"}`)
	out, changed := injectPromptCacheKey(body, "sess-abc")
	if changed {
		t.Fatalf("should not overwrite existing prompt_cache_key: %s", out)
	}
	if string(out) != string(body) {
		t.Fatalf("body was altered: %s", out)
	}
}

func TestInjectPromptCacheKeyClamps(t *testing.T) {
	longID := strings.Repeat("x", 80)
	body := []byte(`{"model":"m"}`)
	out, changed := injectPromptCacheKey(body, longID)
	if !changed {
		t.Fatal("expected clamped injection")
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	var got string
	if err := json.Unmarshal(m["prompt_cache_key"], &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len([]rune(got)) != 64 || got != strings.Repeat("x", 64) {
		t.Fatalf("clamping failed: got %q len %d", got, len([]rune(got)))
	}
}
