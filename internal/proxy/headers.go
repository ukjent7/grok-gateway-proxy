package proxy

import (
	"encoding/json"
	"net/http"
	"strings"
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
			if sensitiveHeader(name) {
				dst.Add(name, "[REDACTED]")
			} else {
				dst.Add(name, value)
			}
		}
	}
	return dst
}

func sensitiveHeader(name string) bool {
	lower := strings.ToLower(name)
	return strings.Contains(lower, "authorization") ||
		strings.Contains(lower, "api-key") ||
		strings.Contains(lower, "apikey") ||
		strings.Contains(lower, "token") ||
		strings.Contains(lower, "secret") ||
		strings.Contains(lower, "cookie")
}

// redactStoredHeaders rewrites a stored header JSON blob (audit columns,
// including the legacy *_actual ones that predate write-time sanitization) so
// sensitive header values become "[REDACTED]". Blobs that are not valid JSON
// header maps — or that contain no sensitive values — are returned unchanged.
func redactStoredHeaders(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return raw
	}
	var headers map[string][]string
	if err := json.Unmarshal([]byte(raw), &headers); err != nil {
		return raw
	}
	changed := false
	for name, values := range headers {
		if !sensitiveHeader(name) {
			continue
		}
		for i, value := range values {
			if value != "[REDACTED]" {
				values[i] = "[REDACTED]"
				changed = true
			}
		}
	}
	if !changed {
		return raw
	}
	redacted, err := json.Marshal(headers)
	if err != nil {
		return raw
	}
	return string(redacted)
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
