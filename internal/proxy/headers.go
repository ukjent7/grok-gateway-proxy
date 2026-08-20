package proxy

import (
	"encoding/json"
	"net/http"
	"strings"

	"grok-gateway-proxy/internal/redact"
)

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
	"content-encoding":    true,
	"content-range":       true,
}

var defaultForwardHeaders = []string{
	"Authorization",
	"Content-Type",
	"Accept",
	"User-Agent",
}

func copyForwardHeaders(dst http.Header, src http.Header, allowlist []string) {
	allowed := make(map[string]bool, len(allowlist))
	for _, name := range allowlist {
		allowed[strings.ToLower(name)] = true
	}
	for name, values := range src {
		lower := strings.ToLower(name)
		if hopByHopHeaders[lower] || strings.HasPrefix(lower, "x-grok-") || !allowed[lower] {
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
