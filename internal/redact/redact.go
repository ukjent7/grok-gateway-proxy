package redact

import (
	"encoding/json"
	"strings"
)

func SensitiveHeader(name string) bool {
	lower := strings.ToLower(name)
	return strings.Contains(lower, "authorization") ||
		strings.Contains(lower, "api-key") ||
		strings.Contains(lower, "apikey") ||
		strings.Contains(lower, "token") ||
		strings.Contains(lower, "secret") ||
		strings.Contains(lower, "cookie")
}

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
