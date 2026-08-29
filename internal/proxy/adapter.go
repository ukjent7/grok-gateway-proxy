package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"grok-gateway-proxy/internal/config"
)

// GatewayAdapter describes one upstream's native protocol. Adapters validate
// the fixed endpoint, and where the upstream dialect differs from what the
// client speaks, rewrite the payload on the way in and out.
type GatewayAdapter interface {
	ID() string
	Protocol() config.Protocol
	EndpointPath() string
	AcceptsPath(path string) bool
	RejectMessage(path string) string
	ValidateRequest(body []byte) error
	NormalizeError(status int, body []byte) []byte
}

// payloadTransformer is implemented by adapters that need to rewrite the JSON
// payload on the way in or out (Responses request sanitization, SenseNova
// tool-call normalization).
type payloadTransformer interface {
	TransformRequestBody([]byte) ([]byte, error)
	TransformResponseBody([]byte) ([]byte, error)
}

// streamTransformer is implemented by adapters that need line-level SSE
// transforms (Responses event filtering, SenseNova tool-call deltas).
type streamTransformer interface {
	TransformSSE(io.Reader) io.Reader
}

func transformRequestBody(adapter GatewayAdapter, body []byte) ([]byte, error) {
	if transformer, ok := adapter.(payloadTransformer); ok {
		return transformer.TransformRequestBody(body)
	}
	return body, nil
}

func transformResponseBody(adapter GatewayAdapter, body []byte) ([]byte, error) {
	if transformer, ok := adapter.(payloadTransformer); ok {
		return transformer.TransformResponseBody(body)
	}
	return body, nil
}

func transformSSE(adapter GatewayAdapter, reader io.Reader) io.Reader {
	if transformer, ok := adapter.(streamTransformer); ok {
		return transformer.TransformSSE(reader)
	}
	return reader
}

func validateJSONRequest(body []byte, protocolName string) error {
	if len(strings.TrimSpace(string(body))) == 0 {
		return fmt.Errorf("%s request body cannot be empty", protocolName)
	}
	if !json.Valid(body) {
		return fmt.Errorf("%s request body must be valid JSON", protocolName)
	}
	return nil
}

// normalizeUpstreamError converts an upstream error body into the proxy's
// stable error envelope while keeping the original body under "details" for
// debugging.
func normalizeUpstreamError(status int, body []byte) []byte {
	message := fmt.Sprintf("upstream returned HTTP %d", status)
	var upstream struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &upstream) == nil && strings.TrimSpace(upstream.Error.Message) != "" {
		message = upstream.Error.Message
	}
	errorBody := map[string]any{
		"type":    "upstream_error",
		"message": message,
		"code":    strconv.Itoa(status),
	}
	if len(body) > 0 {
		if json.Valid(body) {
			errorBody["details"] = json.RawMessage(body)
		} else {
			errorBody["details"] = string(body)
		}
	}
	result, err := json.Marshal(map[string]any{"error": errorBody})
	if err != nil {
		return []byte(`{"error":{"type":"upstream_error","message":"upstream request failed"}}`)
	}
	return result
}

// gatewayAdapters maps a gateway ID to its protocol adapter. Adapters are
// stateless singletons, so one instance per gateway is sufficient.
var gatewayAdapters = map[string]GatewayAdapter{
	"ds":  DeepSeekResponsesAdapter{},
	"st":  SenseNovaChatAdapter{},
	"std": StandardResponsesAdapter{},
}

func adapterFor(id string) (GatewayAdapter, bool) {
	adapter, ok := gatewayAdapters[id]
	return adapter, ok
}
