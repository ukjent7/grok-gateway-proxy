package proxy

import (
	"encoding/json"
	"net/http"
	"strings"

	"grok-gateway-proxy/internal/redact"
)

// hopByHopHeaders lists the headers that belong to a single connection and
// must never be forwarded in either direction. Content-Length is included even
// though it is an end-to-end header: the proxy re-frames bodies, so the
// original length no longer describes what travels on the next hop.
//
// Content-Encoding and Content-Range are deliberately absent — they are
// end-to-end representation headers. Dropping them would strand a compressed
// or partial body on the client with no way to decode it (reachable when a
// gateway's ForwardHeaders opts into Accept-Encoding, which disables the Go
// transport's transparent gzip handling).
var hopByHopHeaders = map[string]bool{
	"connection":          true,
	"keep-alive":          true,
	"proxy-authenticate":  true,
	"proxy-authorization": true,
	"te":                  true,
	"trailer":             true,
	"transfer-encoding":   true,
	"upgrade":             true,
	"host":                true,
	"content-length":      true,
}

// defaultForwardHeaders is the allowlist applied when a gateway configures
// none of its own. It deliberately omits Grok Build's `x-grok-*` headers:
// they are routing and telemetry for an xAI backend, and several carry
// identifiers (x-grok-user-id, x-grok-session-id) that should not reach a
// third-party upstream unbidden.
var defaultForwardHeaders = []string{
	"Authorization",
	"Content-Type",
	"Accept",
	"User-Agent",
}

// copyForwardHeaders copies the allowlisted request headers onto the upstream
// request and drops everything else. That is the whole mechanism keeping Grok
// Build's internal `x-grok-*` headers off a third-party upstream: they are
// simply not on the list.
//
// Naming one of them in a gateway's ForwardHeaders opts in explicitly. A
// blanket prefix block used to override the allowlist here, which meant the
// configuration could name an x-grok-* header and still have it stripped —
// default-deny had quietly become a lockout, and there was no way to route the
// xAI-only opt-ins (x-grok-doom-loop-check, x-grok-exact-repetition-check) to
// a backend that understands them.
func copyForwardHeaders(dst http.Header, src http.Header, allowlist []string) {
	allowed := make(map[string]bool, len(allowlist))
	for _, name := range allowlist {
		allowed[strings.ToLower(name)] = true
	}
	for name, values := range src {
		lower := strings.ToLower(name)
		if hopByHopHeaders[lower] || !allowed[lower] {
			continue
		}
		for _, value := range values {
			dst.Add(name, value)
		}
	}
}

func sanitizeHeaders(src http.Header) http.Header {
	dst := make(http.Header, len(src))
	for name, values := range src {
		for _, value := range values {
			if redact.SensitiveHeader(name) {
				dst.Add(name, "[REDACTED]")
			} else {
				dst.Add(name, value)
			}
		}
	}
	return dst
}

func headersJSON(headers http.Header) string {
	return marshalJSON(sanitizeHeaders(headers))
}

func marshalJSON(value any) string {
	b, err := json.Marshal(value)
	if err != nil {
		return "{}"
	}
	return string(b)
}
