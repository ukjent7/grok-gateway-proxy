package proxy

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// vercelfx.go implements the Vercel FX disguise mode for the ve gateway. When
// enabled, upstream requests are rewritten into the official fx client's v3
// language-model protocol so the request hits Vercel AI Gateway's free
// promotional pool:
//
//   - HTTP headers impersonate the fx CLI (fx/ UA, HTTP-Referer, X-Title).
//   - The request body is converted from the Responses protocol to the v3
//     language-model format with a `headers: {user-agent, x-title}` object
//     injected (the promo trigger).
//   - The v3 SSE response (text-delta / reasoning-delta / tool-input-* /
//     finish) is converted back to the Responses protocol SSE the client
//     expects, and non-streaming responses are assembled into a Responses
//     JSON object.
const (
	vercelFXUpstreamPath = "/v3/ai/language-model"
	vercelFXReferer      = "https://github.com/vercel-labs/fx"
	vercelFXTitle        = "fx"
	vercelFXMaxOutput    = 128000
)

// vercelFXUpstreamURL derives the v3 language-model endpoint from the
// gateway's configured base URL (same scheme+host, fx pathway path).
func vercelFXUpstreamURL(baseURL string) string {
	parsed, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "https://ai-gateway.vercel.sh" + vercelFXUpstreamPath
	}
	return parsed.Scheme + "://" + parsed.Host + vercelFXUpstreamPath
}

// fxHex returns a random lowercase hex string of the given byte length.
func fxHex(bytesLen int) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])[:bytesLen*2]
}

// fxDisguiseHeaders returns the HTTP headers that make a request look like it
// came from the official fx CLI.
func fxDisguiseHeaders(userAgent, model, sessionID string) map[string]string {
	return map[string]string{
		"User-Agent":                  userAgent,
		"HTTP-Referer":                vercelFXReferer,
		"X-Title":                     vercelFXTitle,
		"ai-gateway-protocol-version": "0.0.1",
		"ai-language-model-specification-version": "4",
		"ai-language-model-id":                    model,
		"ai-language-model-streaming":             "true",
		"x-session-id":                            sessionID,
		"x-session-affinity":                      sessionID,
	}
}
