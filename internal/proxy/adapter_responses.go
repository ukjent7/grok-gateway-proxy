package proxy

import (
	"grok-gateway-proxy/internal/config"
)

const responsesEndpoint = "/responses"

// baseResponsesAdapter holds the shared Responses-protocol plumbing.
// DeepSeek and Standard adapters differ only in request sanitization;
// all other protocol details (path, validation, SSE filtering) are identical.
//
// Name-bearing methods (RejectMessage, ValidateRequest) deliberately live on
// the concrete adapters: they embed this base as a zero value, so a `name`
// field here would always be empty.
type baseResponsesAdapter struct{}

func (b baseResponsesAdapter) Protocol() config.Protocol { return config.ProtocolResponses }
func (b baseResponsesAdapter) EndpointPath() string      { return responsesEndpoint }
func (b baseResponsesAdapter) AcceptsPath(path string) bool {
	return path == responsesEndpoint
}
