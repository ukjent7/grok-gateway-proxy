package proxy

import (
	"encoding/json"
	"fmt"
	"io"

	"grok-gateway-proxy/internal/config"
)

// DeepSeekResponsesAdapter targets DeepSeek's Responses API
// (https://api.deepseek.com, per the official "使用 Responses API" and
// "思考模式" docs).
//
// DeepSeek's Responses dialect is close to the standard: the stream event
// names (response.created, response.reasoning_text.delta,
// response.output_text.delta, response.function_call_arguments.delta, ...)
// and terminal events all fall inside Grok Build's typed event vocabulary,
// the stream carries no `data: [DONE]` sentinel (the client terminates on
// EOF), and unsupported top-level parameters are silently ignored. The
// request direction therefore only needs targeted cleanups on top of the
// shared standard-protocol sanitization:
//
//   - `include` is dropped entirely: DeepSeek does not support any include
//     value (reasoning content is always returned as plaintext).
//   - input `reasoning` items keep their plaintext `content` (what DeepSeek
//     merges back into the adjacent assistant message and what its tool-call
//     loop requires: a request carrying tools must replay the reasoning
//     content verbatim or the API returns 400), while `summary` and
//     `encrypted_content`, which DeepSeek does not support, are stripped.
//   - `reasoning.effort` passes through untouched.
type DeepSeekResponsesAdapter struct{ baseResponsesAdapter }

func (DeepSeekResponsesAdapter) ID() string { return "DeepSeekResponsesAdapter" }
func (a DeepSeekResponsesAdapter) Protocol() config.Protocol { return a.baseResponsesAdapter.Protocol() }
func (a DeepSeekResponsesAdapter) EndpointPath() string      { return a.baseResponsesAdapter.EndpointPath() }
func (a DeepSeekResponsesAdapter) AcceptsPath(path string) bool {
	return a.baseResponsesAdapter.AcceptsPath(path)
}
func (a DeepSeekResponsesAdapter) RejectMessage(path string) string {
	return fmt.Sprintf("%s accepts only %s, got %s", a.ID(), a.EndpointPath(), path)
}
func (DeepSeekResponsesAdapter) ValidateRequest(body []byte) error {
	return validateJSONRequest(body, "DeepSeek Responses")
}
func (DeepSeekResponsesAdapter) NormalizeError(status int, body []byte) []byte {
	return normalizeUpstreamError(status, body)
}

// TransformRequestBody sanitizes the request to the standard Responses
// vocabulary and then applies the DeepSeek-specific cleanups.
func (DeepSeekResponsesAdapter) TransformRequestBody(body []byte) ([]byte, error) {
	sanitized, err := sanitizeResponsesRequest(body)
	if err != nil {
		return nil, err
	}
	return adaptResponsesRequestForDeepSeek(sanitized)
}

// TransformResponseBody is a pass-through; the response object matches the
// OpenAI Responses structure.
func (DeepSeekResponsesAdapter) TransformResponseBody(body []byte) ([]byte, error) {
	return body, nil
}

// TransformSSE translates the reply stream into the event vocabulary Grok
// Build's parser understands (DeepSeek's event names are already inside it;
// the filter guards against pings and future event types).
func (DeepSeekResponsesAdapter) TransformSSE(reader io.Reader) io.Reader {
	return newResponsesSSEFilter(reader)
}

// adaptResponsesRequestForDeepSeek applies the DeepSeek-specific request
// cleanups. Bodies with none of the affected fields are returned
// byte-for-byte.
func adaptResponsesRequestForDeepSeek(body []byte) ([]byte, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return body, nil
	}
	changed := false
	if _, ok := payload["include"]; ok {
		delete(payload, "include")
		changed = true
	}
	if raw, ok := payload["input"]; ok {
		if cleaned, inputChanged := stripUnsupportedReasoningFields(raw); inputChanged {
			payload["input"] = cleaned
			changed = true
		}
	}
	if !changed {
		return body, nil
	}
	return json.Marshal(payload)
}

// stripUnsupportedReasoningFields removes `summary` and `encrypted_content`
// from input reasoning items, keeping the plaintext `content` that DeepSeek
// merges into the adjacent assistant message.
func stripUnsupportedReasoningFields(raw json.RawMessage) (json.RawMessage, bool) {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return raw, false
	}
	kept := make([]json.RawMessage, 0, len(items))
	changed := false
	for _, item := range items {
		var probe struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(item, &probe); err != nil || probe.Type != "reasoning" {
			kept = append(kept, item)
			continue
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(item, &fields); err != nil {
			kept = append(kept, item)
			continue
		}
		itemChanged := false
		if _, ok := fields["summary"]; ok {
			delete(fields, "summary")
			itemChanged = true
		}
		if _, ok := fields["encrypted_content"]; ok {
			delete(fields, "encrypted_content")
			itemChanged = true
		}
		if itemChanged {
			cleaned, err := json.Marshal(fields)
			if err != nil {
				kept = append(kept, item)
				continue
			}
			kept = append(kept, cleaned)
			changed = true
		} else {
			kept = append(kept, item)
		}
	}
	if !changed {
		return raw, false
	}
	out, err := json.Marshal(kept)
	if err != nil {
		return raw, false
	}
	return out, true
}
