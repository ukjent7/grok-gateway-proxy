package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// OpenCodeResponsesAdapter handles the OpenCode Zen gateway (Responses
// protocol). The protocol itself passes through unchanged; the only
// deviation is a minimal per-model-family fix for Muse models (see
// isMuseSparkModel below).
type OpenCodeResponsesAdapter struct{}

func (OpenCodeResponsesAdapter) ID() string           { return "OpenCodeResponsesAdapter" }
func (OpenCodeResponsesAdapter) Protocol() Protocol   { return ProtocolResponses }
func (OpenCodeResponsesAdapter) EndpointPath() string { return "/responses" }
func (a OpenCodeResponsesAdapter) AcceptsPath(path string) bool {
	return path == a.EndpointPath()
}
func (a OpenCodeResponsesAdapter) RejectMessage(path string) string {
	return fmt.Sprintf("%s accepts only %s, got %s", a.ID(), a.EndpointPath(), path)
}
func (OpenCodeResponsesAdapter) ValidateRequest(body []byte) error {
	return validateJSONRequest(body, "OpenCode Responses")
}
func (OpenCodeResponsesAdapter) NormalizeError(status int, body []byte) []byte {
	return normalizeUpstreamError(status, body)
}

// isMuseSparkModel reports whether the model belongs to the Muse family by
// name prefix, so upstream variants such as muse-spark-1.2-contributo match
// the "muse-spark" family just like muse-spark-1.2. "deepseek"-prefixed
// models currently pass through unchanged and get their own case here as
// soon as they need protocol deviations.
func isMuseSparkModel(model string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "muse-spark")
}

// TransformModelRequest removes the client-only stream_tool_calls option
// that Muse models reject. Requests without the field, and every non-Muse
// model, pass through byte-for-byte.
func (OpenCodeResponsesAdapter) TransformModelRequest(model string, body []byte) ([]byte, error) {
	if !isMuseSparkModel(model) {
		return body, nil
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return body, nil
	}
	if _, exists := payload["stream_tool_calls"]; !exists {
		return body, nil
	}
	delete(payload, "stream_tool_calls")
	return json.Marshal(payload)
}

// TransformModelResponse is a pass-through; Muse models need no response-side
// fix today.
func (OpenCodeResponsesAdapter) TransformModelResponse(_ string, body []byte) ([]byte, error) {
	return body, nil
}

// TransformModelSSE drops the ping keepalive events the gateway injects for
// Muse models. Non-Muse models pass through untouched.
func (OpenCodeResponsesAdapter) TransformModelSSE(model string, reader io.Reader) io.Reader {
	if !isMuseSparkModel(model) {
		return reader
	}
	return newMuseSSEReader(reader)
}
