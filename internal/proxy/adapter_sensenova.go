package proxy

import (
	"fmt"
	"io"

	"grok-gateway-proxy/internal/config"
)

type SenseNovaChatAdapter struct{}

func (SenseNovaChatAdapter) Protocol() config.Protocol { return config.ProtocolChat }
func (SenseNovaChatAdapter) EndpointPath() string      { return "/chat/completions" }
func (a SenseNovaChatAdapter) AcceptsPath(path string) bool {
	return path == a.EndpointPath()
}
func (a SenseNovaChatAdapter) RejectMessage(path string) string {
	return fmt.Sprintf("this gateway accepts only %s, got %s", a.EndpointPath(), path)
}
func (SenseNovaChatAdapter) ValidateRequest(body []byte) error {
	return validateJSONRequest(body, "SenseNova Chat Completions")
}

func (SenseNovaChatAdapter) TransformRequestBody(body []byte) ([]byte, error) {
	return sanitizeSenseNovaToolCallHistory(transformToolCallType(body, "function", "function_call"))
}

func (SenseNovaChatAdapter) TransformResponseBody(body []byte) ([]byte, error) {
	return transformSenseNovaResponseBody(body), nil
}

func (SenseNovaChatAdapter) TransformSSE(reader io.Reader) io.Reader {
	return newSSELineTransformer(reader, transformSenseNovaSSELine, nil, nil, nil)
}
