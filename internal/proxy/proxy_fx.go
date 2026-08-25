package proxy

import (
	"encoding/json"
	"net/http"
	"strings"
)

// fxSessionID derives a stable session identifier for Vercel AI Gateway FX
// mode. Vercel uses X-Session-Id / X-Session-Affinity for cache affinity: it
// routes the request to a backend node that holds the cached prompt prefix. A
// random per-request ID defeats this, so the prompt is re-tokenized on every
// call and cache hits collapse.
//
// The real fx client generates one session ID per conversation at startup and
// reuses it for every turn. The proxy is stateless, so it derives a stable ID
// from the request body's prompt_cache_key (set by the client to the
// conversation ID and kept constant across turns). When that field is absent,
// it falls back to x-session-id / x-session-affinity / x-client-request-id —
// standard gateway headers the original fx client also honors — and only
// generates a random ID as a last resort.
func fxSessionID(r *http.Request, requestBody []byte) string {
	var root struct {
		PromptCacheKey string `json:"prompt_cache_key"`
	}
	if json.Unmarshal(requestBody, &root) == nil {
		if key := strings.TrimSpace(root.PromptCacheKey); key != "" {
			return key
		}
	}
	for _, h := range []string{"x-session-id", "x-session-affinity", "x-client-request-id"} {
		if v := r.Header.Get(h); v != "" {
			return v
		}
	}
	return "pi-" + fxHex(8)
}
