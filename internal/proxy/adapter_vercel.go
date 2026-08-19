package proxy

import (
	"fmt"
	"io"

	"grok-gateway-proxy/internal/config"
)

// VercelResponsesAdapter handles the Vercel AI Gateway (Responses protocol).
// Vercel adapts third-party providers to the Responses API, but two behaviors
// break strict clients (e.g. Grok Build's async-openai based parser):
//   - it injects keepalive ping events (data: {"type":"ping"})
//   - for reasoning models, it emits legacy event names
//     response.reasoning.delta / response.reasoning.done instead of
//     response.reasoning_text.delta / response.reasoning_text.done
//
// The SSE reader below drops the pings and renames the legacy reasoning
// events (their payloads are field-for-field identical); everything else is
// passed through untouched.
type VercelResponsesAdapter struct{}

func (VercelResponsesAdapter) ID() string           { return "VercelResponsesAdapter" }
func (VercelResponsesAdapter) Protocol() config.Protocol { return config.ProtocolResponses }
func (VercelResponsesAdapter) EndpointPath() string { return "/responses" }
func (a VercelResponsesAdapter) AcceptsPath(path string) bool {
	return path == a.EndpointPath()
}
func (a VercelResponsesAdapter) RejectMessage(path string) string {
	return fmt.Sprintf("%s accepts only %s, got %s", a.ID(), a.EndpointPath(), path)
}
func (VercelResponsesAdapter) ValidateRequest(body []byte) error {
	return validateJSONRequest(body, "Vercel Responses")
}
func (VercelResponsesAdapter) NormalizeError(status int, body []byte) []byte {
	return normalizeUpstreamError(status, body)
}

func (VercelResponsesAdapter) TransformSSE(reader io.Reader) io.Reader {
	return newVercelSSEReader(reader)
}
