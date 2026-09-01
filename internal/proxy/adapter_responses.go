package proxy

import (
	"fmt"
	"io"

	"grok-gateway-proxy/internal/config"
)

const responsesEndpoint = "/responses"

type baseResponsesAdapter struct{}

func (b baseResponsesAdapter) Protocol() config.Protocol { return config.ProtocolResponses }
func (b baseResponsesAdapter) EndpointPath() string      { return responsesEndpoint }
func (b baseResponsesAdapter) AcceptsPath(path string) bool {
	return path == responsesEndpoint
}
func (b baseResponsesAdapter) RejectMessage(path string) string {
	return fmt.Sprintf("this gateway accepts only %s, got %s", responsesEndpoint, path)
}

func (baseResponsesAdapter) TransformRequestBody(body []byte) ([]byte, error) {
	return body, nil
}

func (baseResponsesAdapter) TransformResponseBody(body []byte) ([]byte, error) {
	return body, nil
}

func (baseResponsesAdapter) TransformSSE(reader io.Reader) io.Reader {
	return reader
}
