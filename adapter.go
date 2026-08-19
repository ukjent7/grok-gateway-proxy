package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// GatewayAdapter describes one upstream's native protocol. Adapters deliberately
// do not translate Responses and Chat Completions; they validate the fixed
// endpoint and preserve the provider payload, including reasoning and tool data.
type GatewayAdapter interface {
	ID() string
	Protocol() Protocol
	EndpointPath() string
	AcceptsPath(path string) bool
	RejectMessage(path string) string
	ValidateRequest(body []byte) error
	NormalizeError(status int, body []byte) []byte
}

// payloadTransformer is implemented by adapters that need to rewrite the JSON
// payload on the way in or out (SenseNova tool-call normalization).
type payloadTransformer interface {
	TransformRequestBody([]byte) ([]byte, error)
	TransformResponseBody([]byte) ([]byte, error)
}

// streamTransformer is implemented by adapters that need line-level SSE
// transforms (SenseNova tool-call deltas, Vercel ping/reasoning events).
type streamTransformer interface {
	TransformSSE(io.Reader) io.Reader
}

// modelAwareTransformer is implemented by adapters that apply
// model-specific (per model-family) protocol deviations, e.g. OpenCode's
// Muse-family rules. Models that match no rule pass through untouched.
type modelAwareTransformer interface {
	TransformModelRequest(model string, body []byte) ([]byte, error)
	TransformModelResponse(model string, body []byte) ([]byte, error)
	TransformModelSSE(model string, reader io.Reader) io.Reader
}

func transformRequestBody(adapter GatewayAdapter, model string, body []byte) ([]byte, error) {
	transformed := body
	if transformer, ok := adapter.(payloadTransformer); ok {
		var err error
		transformed, err = transformer.TransformRequestBody(transformed)
		if err != nil {
			return nil, err
		}
	}
	if mt, ok := adapter.(modelAwareTransformer); ok {
		return mt.TransformModelRequest(model, transformed)
	}
	return transformed, nil
}

func transformResponseBody(adapter GatewayAdapter, model string, body []byte) ([]byte, error) {
	transformed := body
	if transformer, ok := adapter.(payloadTransformer); ok {
		var err error
		transformed, err = transformer.TransformResponseBody(transformed)
		if err != nil {
			return nil, err
		}
	}
	if mt, ok := adapter.(modelAwareTransformer); ok {
		return mt.TransformModelResponse(model, transformed)
	}
	return transformed, nil
}

func transformSSE(adapter GatewayAdapter, model string, reader io.Reader) io.Reader {
	if transformer, ok := adapter.(streamTransformer); ok {
		reader = transformer.TransformSSE(reader)
	}
	if mt, ok := adapter.(modelAwareTransformer); ok {
		reader = mt.TransformModelSSE(model, reader)
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
	"oc": OpenCodeResponsesAdapter{},
	"st": SenseNovaChatAdapter{},
	"ve": VercelResponsesAdapter{},
}

func adapterFor(id string) (GatewayAdapter, bool) {
	adapter, ok := gatewayAdapters[id]
	return adapter, ok
}
