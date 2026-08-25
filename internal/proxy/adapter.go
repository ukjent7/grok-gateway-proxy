package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"grok-gateway-proxy/internal/config"
	"grok-gateway-proxy/internal/store"
)

// GatewayAdapter describes one upstream's native protocol. Adapters deliberately
// do not translate Responses and Chat Completions; they validate the fixed
// endpoint and preserve the provider payload, including reasoning and tool data.
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

// gatewayURLProvider is implemented by adapters that need gateway-aware URL
// construction (Vercel FX v3 endpoint).
type gatewayURLProvider interface {
	GatewayUpstreamURL(gateway config.GatewayConfig, subpath, rawQuery string) (string, error)
}

// gatewayHeaderProvider is implemented by adapters that need to inject
// gateway-specific headers (FX disguise headers).
type gatewayHeaderProvider interface {
	GatewayExtraHeaders(gateway config.GatewayConfig, r *http.Request, body []byte, model string) map[string]string
}

// gatewayRequestTransformer is implemented by adapters that need gateway-aware
// request body rewriting (FX Responses → v3).
type gatewayRequestTransformer interface {
	TransformGatewayRequest(gateway config.GatewayConfig, model string, body []byte) ([]byte, error)
}

// gatewayResponseTransformer is implemented by adapters that need gateway-aware
// non-streaming response conversion (FX SSE → Responses JSON).
type gatewayResponseTransformer interface {
	TransformGatewayResponse(gateway config.GatewayConfig, model string, body []byte) ([]byte, error)
}

// gatewaySSETransformer is implemented by adapters that need gateway-aware SSE
// handling (FX v3 → Responses).
type gatewaySSETransformer interface {
	TransformGatewaySSE(gateway config.GatewayConfig, model string, reader io.Reader) io.Reader
}

// gatewayUsageExtractor is implemented by adapters that need gateway-aware
// usage extraction (FX v3 usage vs Responses usage).
type gatewayUsageExtractor interface {
	ExtractGatewayUsage(gateway config.GatewayConfig, upstreamBody, responseBody []byte) store.UsageMetrics
}

// gatewayResponseContentTypeProvider is implemented by adapters that need to
// override the downstream Content-Type (FX non-streaming JSON).
type gatewayResponseContentTypeProvider interface {
	GatewayResponseContentType(gateway config.GatewayConfig, upstreamHeaders http.Header, stream bool) string
}

// gatewayEventStreamProvider is implemented by adapters that need to
// override event-stream detection (FX uses client stream flag, not upstream
// Content-Type).
type gatewayEventStreamProvider interface {
	IsGatewayEventStream(gateway config.GatewayConfig, upstreamHeaders http.Header, stream bool) bool
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

// gatewayUpstreamURL returns the upstream URL, delegating to the adapter when
// it implements gatewayURLProvider (Vercel FX), otherwise using the default
// join logic.
func gatewayUpstreamURL(adapter GatewayAdapter, gateway config.GatewayConfig, subpath, rawQuery string) (string, error) {
	if p, ok := adapter.(gatewayURLProvider); ok {
		return p.GatewayUpstreamURL(gateway, subpath, rawQuery)
	}
	return joinUpstreamURL(gateway.BaseURL, subpath, rawQuery)
}

func gatewayExtraHeaders(adapter GatewayAdapter, gateway config.GatewayConfig, r *http.Request, body []byte, model string) map[string]string {
	if p, ok := adapter.(gatewayHeaderProvider); ok {
		return p.GatewayExtraHeaders(gateway, r, body, model)
	}
	return nil
}

func gatewayTransformRequest(adapter GatewayAdapter, gateway config.GatewayConfig, model string, body []byte) ([]byte, error) {
	if p, ok := adapter.(gatewayRequestTransformer); ok {
		return p.TransformGatewayRequest(gateway, model, body)
	}
	return transformRequestBody(adapter, model, body)
}

func gatewayTransformResponse(adapter GatewayAdapter, gateway config.GatewayConfig, model string, body []byte) ([]byte, error) {
	if p, ok := adapter.(gatewayResponseTransformer); ok {
		return p.TransformGatewayResponse(gateway, model, body)
	}
	return transformResponseBody(adapter, model, body)
}

func gatewayTransformSSE(adapter GatewayAdapter, gateway config.GatewayConfig, model string, reader io.Reader) io.Reader {
	if p, ok := adapter.(gatewaySSETransformer); ok {
		return p.TransformGatewaySSE(gateway, model, reader)
	}
	return transformSSE(adapter, model, reader)
}

func gatewayExtractUsage(adapter GatewayAdapter, gateway config.GatewayConfig, upstreamBody, responseBody []byte) store.UsageMetrics {
	if p, ok := adapter.(gatewayUsageExtractor); ok {
		return p.ExtractGatewayUsage(gateway, upstreamBody, responseBody)
	}
	return ExtractUsage(responseBody, gateway.Protocol)
}

func gatewayResponseContentType(adapter GatewayAdapter, gateway config.GatewayConfig, upstreamHeaders http.Header, stream bool) string {
	if p, ok := adapter.(gatewayResponseContentTypeProvider); ok {
		return p.GatewayResponseContentType(gateway, upstreamHeaders, stream)
	}
	return ""
}

func gatewayIsEventStream(adapter GatewayAdapter, gateway config.GatewayConfig, upstreamHeaders http.Header, stream bool) bool {
	if p, ok := adapter.(gatewayEventStreamProvider); ok {
		return p.IsGatewayEventStream(gateway, upstreamHeaders, stream)
	}
	return isEventStream(upstreamHeaders)
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
