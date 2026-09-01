package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"

	"grok-gateway-proxy/internal/config"
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
}

var defaultForwardHeaders = []string{
	"Authorization",
	"X-Api-Key",
	"Accept",
	"User-Agent",
	"X-Request-Id",
	"X-Correlation-Id",
	"X-Client-Request-Id",
	"X-Session-Id",
	"session_id",
	"Anthropic-Version",
	"Anthropic-Beta",
}

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

func grokSessionID(src http.Header) string {
	if v := strings.TrimSpace(src.Get("X-Grok-Session-Id")); v != "" {
		return v
	}
	return strings.TrimSpace(src.Get("X-Grok-Conv-Id"))
}

func grokRequestID(src http.Header) string {
	return strings.TrimSpace(src.Get("X-Grok-Req-Id"))
}

func clampPromptCacheKey(s string) string {
	if s == "" {
		return s
	}
	runes := []rune(s)
	if len(runes) <= 64 {
		return s
	}
	return string(runes[:64])
}

func injectPromptCacheKey(body []byte, sessionID string) ([]byte, bool) {
	sessionID = clampPromptCacheKey(strings.TrimSpace(sessionID))
	if sessionID == "" {
		return body, false
	}
	if len(bytes.TrimSpace(body)) == 0 || !bytes.Contains(body, []byte("{")) {
		return body, false
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return body, false
	}
	if raw, ok := payload["prompt_cache_key"]; ok {
		var existing string
		if err := json.Unmarshal(raw, &existing); err == nil && strings.TrimSpace(existing) != "" {
			return body, false
		}
	}
	encoded, err := json.Marshal(sessionID)
	if err != nil {
		return body, false
	}
	payload["prompt_cache_key"] = encoded
	out, err := marshalJSONNoEscape(payload)
	if err != nil {
		return body, false
	}
	return out, true
}

func buildUpstreamHeaders(dst http.Header, src http.Header, gateway config.GatewayConfig, requestID string, stream bool) {
	allowlist := gateway.ForwardHeaders
	if len(allowlist) == 0 {
		allowlist = defaultForwardHeaders
	}
	copyForwardHeaders(dst, src, allowlist)
	applyGrokHeaderTranslations(dst, src, gateway, requestID, stream)
}

func applyGrokHeaderTranslations(dst http.Header, src http.Header, gateway config.GatewayConfig, requestID string, stream bool) {
	// Single-entry header construction is buildUpstreamHeaders; the two steps
	// below are the internal phases. Order matters: copyForwardHeaders runs
	// first and may already have copied User-Agent / Accept from the allowlist.
	// The checks below only set a fallback when the header is still empty, so
	// the allowlist decision wins and each header is decided exactly once.
	applySessionAffinity(dst, gateway.EffectiveSessionAffinity(), grokSessionID(src))

	if reqID := grokRequestID(src); reqID != "" && dst.Get("X-Correlation-Id") == "" {
		dst.Set("X-Correlation-Id", reqID)
	}
	if requestID != "" && dst.Get("X-Request-Id") == "" {
		dst.Set("X-Request-Id", requestID)
	}

	if dst.Get("Authorization") == "" {
		if v := strings.TrimSpace(src.Get("Authorization")); v != "" {
			dst.Set("Authorization", v)
		}
	}
	if dst.Get("X-Api-Key") == "" {
		if v := strings.TrimSpace(src.Get("X-Api-Key")); v != "" {
			dst.Set("X-Api-Key", v)
		}
	}

	if dst.Get("User-Agent") == "" {
		if v := strings.TrimSpace(src.Get("User-Agent")); v != "" {
			dst.Set("User-Agent", v)
		} else if v := strings.TrimSpace(src.Get("X-Grok-Client-Identifier")); v != "" {
			if ver := strings.TrimSpace(src.Get("X-Grok-Client-Version")); ver != "" {
				dst.Set("User-Agent", v+"/"+ver)
			} else {
				dst.Set("User-Agent", v)
			}
		} else {
			dst.Set("User-Agent", config.DefaultUserAgentOverride)
		}
	}
	if dst.Get("Accept") == "" {
		if stream {
			dst.Set("Accept", "text/event-stream")
		} else {
			dst.Set("Accept", "application/json")
		}
	}
}

func applySessionAffinity(dst http.Header, mode, sessionID string) {
	if sessionID == "" {
		return
	}
	switch mode {
	case config.SessionAffinityOpenRouter:
		if dst.Get("X-Session-Id") == "" {
			dst.Set("X-Session-Id", sessionID)
		}
	case config.SessionAffinityOff:
		return
	case config.SessionAffinityOpenAI:
		fallthrough
	default:
		if dst.Get("session_id") == "" {
			dst.Set("session_id", sessionID)
		}
		if dst.Get("X-Client-Request-Id") == "" {
			dst.Set("X-Client-Request-Id", sessionID)
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
