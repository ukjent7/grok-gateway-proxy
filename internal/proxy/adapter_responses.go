package proxy

import (
	"fmt"

	"grok-gateway-proxy/internal/config"
)

const responsesEndpoint = "/responses"

// baseResponsesAdapter holds the shared Responses-protocol plumbing.
// DeepSeek and Standard adapters differ only in request sanitization;
// all other protocol details (path, validation, SSE filtering) are identical.
type baseResponsesAdapter struct {
	name string
}

func (b baseResponsesAdapter) Protocol() config.Protocol { return config.ProtocolResponses }
func (b baseResponsesAdapter) EndpointPath() string      { return responsesEndpoint }
func (b baseResponsesAdapter) AcceptsPath(path string) bool {
	return path == responsesEndpoint
}
func (b baseResponsesAdapter) RejectMessage(path string) string {
	return fmt.Sprintf("%s accepts only %s, got %s", b.name, responsesEndpoint, path)
}
func (b baseResponsesAdapter) ValidateRequest(body []byte) error {
	return validateJSONRequest(body, b.name)
}
func (b baseResponsesAdapter) NormalizeError(status int, body []byte) []byte {
	return normalizeUpstreamError(status, body)
}
