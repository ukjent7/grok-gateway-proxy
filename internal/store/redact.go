package store

import (
	"encoding/json"
	"strings"
)

// sensitiveHeader reports whether a header name may carry credentials.
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
