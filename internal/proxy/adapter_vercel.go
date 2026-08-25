package proxy

import (
	"bytes"
	"fmt"
	"io"
	"net/http"

	"grok-gateway-proxy/internal/config"
	"grok-gateway-proxy/internal/store"
)

// VercelResponsesAdapter handles the Vercel AI Gateway (Responses protocol).
// Vercel adapts third-party providers to the Responses API, but two behaviors
// break strict clients (e.g. Grok Build's async-openai based parser):
//   - it injects keepalive ping events (data: {"type":"ping"})
//   - for reasoning models, it emits legacy event names
//     response.reasoning.delta / response.reasoning.done instead of
//     response.reasoning_text.delta / response.reasoning_text.done
//
// The SSE reader below drops the pings and renames the legacy reasoning
// events (their payloads are field-for-field identical); everything else is
// passed through untouched.
type VercelResponsesAdapter struct{}

func (VercelResponsesAdapter) ID() string                { return "VercelResponsesAdapter" }
func (VercelResponsesAdapter) Protocol() config.Protocol { return config.ProtocolResponses }
func (VercelResponsesAdapter) EndpointPath() string      { return "/responses" }
func (a VercelResponsesAdapter) AcceptsPath(path string) bool {
	return path == a.EndpointPath()
}
func (a VercelResponsesAdapter) RejectMessage(path string) string {
	return fmt.Sprintf("%s accepts only %s, got %s", a.ID(), a.EndpointPath(), path)
}
func (VercelResponsesAdapter) ValidateRequest(body []byte) error {
	return validateJSONRequest(body, "Vercel Responses")
}
func (VercelResponsesAdapter) NormalizeError(status int, body []byte) []byte {
	return normalizeUpstreamError(status, body)
}

func (VercelResponsesAdapter) TransformSSE(reader io.Reader) io.Reader {
	return newVercelSSEReader(reader)
}

// GatewayUpstreamURL returns the v3 language-model endpoint when FX disguise
// is enabled, otherwise the standard /responses URL.
func (VercelResponsesAdapter) GatewayUpstreamURL(gateway config.GatewayConfig, subpath, rawQuery string) (string, error) {
	if gateway.FXDisguiseEnabled {
		return vercelFXUpstreamURL(gateway.BaseURL), nil
	}
	return joinUpstreamURL(gateway.BaseURL, subpath, rawQuery)
}

// GatewayExtraHeaders injects the FX disguise headers when enabled.
func (VercelResponsesAdapter) GatewayExtraHeaders(gateway config.GatewayConfig, r *http.Request, body []byte, model string) map[string]string {
	if !gateway.FXDisguiseEnabled {
		return nil
	}
	ua := gateway.FXDisguiseUserAgent
	if ua == "" {
		ua = "fx/0.0.3"
	}
	sessionID := fxSessionID(r, body)
	return fxDisguiseHeaders(ua, model, sessionID)
}

// TransformGatewayRequest rewrites the Responses body to v3 when FX is enabled.
func (VercelResponsesAdapter) TransformGatewayRequest(gateway config.GatewayConfig, model string, body []byte) ([]byte, error) {
	if !gateway.FXDisguiseEnabled {
		return body, nil
	}
	ua := gateway.FXDisguiseUserAgent
	if ua == "" {
		ua = "fx/0.0.3"
	}
	return convertResponsesToV3(body, ua)
}

// TransformGatewayResponse converts the v3 SSE payload back to Responses JSON
// for non-streaming FX requests. Streaming FX is handled via SSE.
func (VercelResponsesAdapter) TransformGatewayResponse(gateway config.GatewayConfig, model string, body []byte) ([]byte, error) {
	if !gateway.FXDisguiseEnabled {
		return body, nil
	}
	// The v3 endpoint always streams SSE; a non-streaming client expects a
	// JSON Responses object, so the buffered SSE must be assembled.
	converted, err := vercelFXSSEToResponses(model, bytes.NewReader(body))
	if err != nil {
		return body, err
	}
	return converted, nil
}

// TransformGatewaySSE returns the FX SSE reader when FX is enabled, otherwise
// the standard Vercel ping-fixing reader.
func (VercelResponsesAdapter) TransformGatewaySSE(gateway config.GatewayConfig, model string, reader io.Reader) io.Reader {
	if gateway.FXDisguiseEnabled {
		return newVercelFXSSEReader(reader, model)
	}
	return newVercelSSEReader(reader)
}

// ExtractGatewayUsage returns v3 usage for FX (from the raw upstream body),
// otherwise the standard Responses usage.
func (VercelResponsesAdapter) ExtractGatewayUsage(gateway config.GatewayConfig, upstreamBody, responseBody []byte) store.UsageMetrics {
	if gateway.FXDisguiseEnabled {
		return store.ExtractFXUsage(upstreamBody)
	}
	return ExtractUsage(responseBody, config.ProtocolResponses)
}

// GatewayResponseContentType overrides the downstream Content-Type for FX
// non-streaming responses (upstream is SSE, downstream must be JSON).
func (VercelResponsesAdapter) GatewayResponseContentType(gateway config.GatewayConfig, upstreamHeaders http.Header, stream bool) string {
	if gateway.FXDisguiseEnabled && !stream {
		return "application/json; charset=utf-8"
	}
	return ""
}

// IsGatewayEventStream mirrors the FX success-path decision: FX uses the
// client's stream flag, not the upstream Content-Type, because the v3
// endpoint always streams SSE.
func (VercelResponsesAdapter) IsGatewayEventStream(gateway config.GatewayConfig, upstreamHeaders http.Header, stream bool) bool {
	if gateway.FXDisguiseEnabled {
		return stream
	}
	return isEventStream(upstreamHeaders)
}
