// Package redact centralizes the rules for recognizing credential-bearing
// HTTP headers and rewriting header blobs so that sensitive values are masked.
// Both the proxy (forwarding/logging) and store (persistence) layers share
// these rules to keep forward-time and at-rest redaction consistent.
package redact

import (
	"encoding/json"
	"strings"
)

// SensitiveHeader reports whether a header name may carry credentials.
// The match is case-insensitive and substring-based so vendor-specific
// spellings (e.g. "x-vendor-secret", "x-api-key") are caught naturally.
func SensitiveHeader(name string) bool {
	lower := strings.ToLower(name)
	return strings.Contains(lower, "authorization") ||
		strings.Contains(lower, "api-key") ||
		strings.Contains(lower, "apikey") ||
		strings.Contains(lower, "token") ||
		strings.Contains(lower, "secret") ||
		strings.Contains(lower, "cookie")
}

// RedactStoredHeaders rewrites a stored header JSON blob (audit columns,
// including the legacy *_actual ones that predate write-time sanitization) so
// sensitive header values become "[REDACTED]". Blobs that are not valid JSON
// header maps — or that contain no sensitive values — are returned unchanged.
func RedactStoredHeaders(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return raw
	}
	var headers map[string][]string
	if err := json.Unmarshal([]byte(raw), &headers); err != nil {
		return raw
	}
	changed := false
	for name, values := range headers {
		if !SensitiveHeader(name) {
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
