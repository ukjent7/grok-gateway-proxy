package main

import (
	"fmt"
	"io"
)

// SenseNovaChatAdapter handles the SenseNova gateway (Chat Completions protocol).
// It normalizes tool-call types between the client-facing Chat Completions
// format (function) and SenseNova's internal representation (function_call).
type SenseNovaChatAdapter struct{}

func (SenseNovaChatAdapter) ID() string           { return "SenseNovaChatAdapter" }
func (SenseNovaChatAdapter) Protocol() Protocol   { return ProtocolChat }
func (SenseNovaChatAdapter) EndpointPath() string { return "/chat/completions" }
func (a SenseNovaChatAdapter) AcceptsPath(path string) bool {
	return path == a.EndpointPath()
}
func (a SenseNovaChatAdapter) RejectMessage(path string) string {
	return fmt.Sprintf("%s accepts only %s, got %s", a.ID(), a.EndpointPath(), path)
}
func (SenseNovaChatAdapter) ValidateRequest(body []byte) error {
	return validateJSONRequest(body, "SenseNova Chat Completions")
}
func (SenseNovaChatAdapter) NormalizeError(status int, body []byte) []byte {
	return normalizeUpstreamError(status, body)
}

// SenseNova's tool-call history decoder uses function_call for the tool call
// variant, while the client-facing Chat Completions format uses function.
// Keep the conversion scoped to tool_calls so tools[].type remains function.
func (SenseNovaChatAdapter) TransformRequestBody(body []byte) ([]byte, error) {
	transformed, err := transformToolCallType(body, "function", "function_call")
	if err != nil {
		return nil, err
	}
	return sanitizeSenseNovaToolCallHistory(transformed)
}

func (SenseNovaChatAdapter) TransformResponseBody(body []byte) ([]byte, error) {
	return transformSenseNovaResponseBody(body)
}

func (SenseNovaChatAdapter) TransformSSE(reader io.Reader) io.Reader {
	return newSSELineTransformer(reader, transformSenseNovaSSELine, nil, nil)
}
