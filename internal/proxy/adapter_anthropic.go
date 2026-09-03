package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"grok-gateway-proxy/internal/config"
)

type AnthropicMessagesAdapter struct{}

func (AnthropicMessagesAdapter) Protocol() config.Protocol { return config.ProtocolAnthropic }
func (AnthropicMessagesAdapter) EndpointPath() string      { return "/v1/messages" }
func (AnthropicMessagesAdapter) AcceptsPath(path string) bool {
	return path == "/v1/messages" || path == "/messages"
}
func (AnthropicMessagesAdapter) RejectMessage(path string) string {
	return fmt.Sprintf("this gateway accepts %s or %s, got %s", "/messages", "/v1/messages", path)
}
func (AnthropicMessagesAdapter) ValidateRequest(body []byte) error {
	return validateJSONRequest(body, "Anthropic Messages")
}

func (AnthropicMessagesAdapter) TransformRequestBody(body []byte) ([]byte, error) {
	if len(bytes.TrimSpace(body)) == 0 {
		return body, nil
	}
	return sanitizeAnthropicRequest(body)
}

func (AnthropicMessagesAdapter) TransformResponseBody(body []byte) ([]byte, error) {
	return body, nil
}

func (AnthropicMessagesAdapter) TransformSSE(reader io.Reader) io.Reader {
	return reader
}

func sanitizeAnthropicRequest(body []byte) ([]byte, error) {
	if !bytes.Contains(body, []byte(`"messages"`)) {
		return body, nil
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body, nil
	}
	changed := false
	if toolsRaw, ok := payload["tools"]; ok {
		if tools, ok := toolsRaw.([]any); ok {
			kept := make([]any, 0, len(tools))
			for _, t := range tools {
				if tm, ok := t.(map[string]any); ok {
					if _, ok := tm["name"].(string); ok {
						kept = append(kept, t)
					} else {
						changed = true
					}
				} else {
					kept = append(kept, t)
				}
			}
			if len(kept) != len(tools) {
				payload["tools"] = kept
				changed = true
			}
		}
	}
	if !changed {
		return body, nil
	}
	out, err := marshalJSONNoEscape(payload)
	if err != nil {
		return body, nil
	}
	return out, nil
}
