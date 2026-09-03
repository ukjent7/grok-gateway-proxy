package proxy

import (
	"io"

	"grok-gateway-proxy/internal/config"
)

type OpenAICompatibleChatAdapter struct{}

func (OpenAICompatibleChatAdapter) Protocol() config.Protocol { return config.ProtocolOpenAICompatible }
func (OpenAICompatibleChatAdapter) EndpointPath() string      { return "/chat/completions" }
func (OpenAICompatibleChatAdapter) AcceptsPath(path string) bool {
	return path == "/chat/completions"
}
func (OpenAICompatibleChatAdapter) RejectMessage(path string) string {
	return "this gateway accepts /chat/completions, got " + path
}
func (OpenAICompatibleChatAdapter) ValidateRequest(body []byte) error {
	return validateJSONRequest(body, "OpenAI Compatible Chat")
}

func (OpenAICompatibleChatAdapter) TransformRequestBody(body []byte) ([]byte, error) {
	if len(body) == 0 {
		return body, nil
	}
	return sanitizeOpenAIChatRequest(body)
}

func (OpenAICompatibleChatAdapter) TransformResponseBody(body []byte) ([]byte, error) {
	return body, nil
}

func (OpenAICompatibleChatAdapter) TransformSSE(reader io.Reader) io.Reader {
	return reader
}

func sanitizeOpenAIChatRequest(body []byte) ([]byte, error) {
	return sanitizeSenseNovaToolCallHistory(body)
}
