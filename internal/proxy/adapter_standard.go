package proxy

import (
	"fmt"
	"io"

	"grok-gateway-proxy/internal/config"
)

// StandardResponsesAdapter targets any upstream that speaks the standard
// OpenAI Responses protocol. Grok Build's xAI-only request extensions are
// stripped and the reply stream is filtered to the event vocabulary Grok
// Build's parser understands; conformant bodies pass through byte-for-byte.
// The gateway ships with an empty Base URL: point it at any Responses
// endpoint in the console.
type StandardResponsesAdapter struct{ baseResponsesAdapter }

func (StandardResponsesAdapter) ID() string { return "StandardResponsesAdapter" }
func (a StandardResponsesAdapter) Protocol() config.Protocol {
	return a.baseResponsesAdapter.Protocol()
}
func (a StandardResponsesAdapter) EndpointPath() string { return a.baseResponsesAdapter.EndpointPath() }
func (a StandardResponsesAdapter) AcceptsPath(path string) bool {
	return a.baseResponsesAdapter.AcceptsPath(path)
}
func (a StandardResponsesAdapter) RejectMessage(path string) string {
	return fmt.Sprintf("%s accepts only %s, got %s", a.ID(), a.EndpointPath(), path)
}
func (StandardResponsesAdapter) ValidateRequest(body []byte) error {
	return validateJSONRequest(body, "standard Responses")
}
func (StandardResponsesAdapter) NormalizeError(status int, body []byte) []byte {
	return normalizeUpstreamError(status, body)
}

// TransformRequestBody strips xAI-only request extensions so the upstream
// receives a standard Responses request.
func (StandardResponsesAdapter) TransformRequestBody(body []byte) ([]byte, error) {
	return sanitizeResponsesRequest(body)
}

// TransformResponseBody is a pass-through; responses need no sanitization
// beyond the SSE event filter.
func (StandardResponsesAdapter) TransformResponseBody(body []byte) ([]byte, error) {
	return body, nil
}

// TransformSSE translates the reply stream into the event vocabulary Grok
// Build's parser understands.
func (StandardResponsesAdapter) TransformSSE(reader io.Reader) io.Reader {
	return newResponsesSSEFilter(reader)
}
