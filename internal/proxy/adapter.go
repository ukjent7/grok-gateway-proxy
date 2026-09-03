package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"grok-gateway-proxy/internal/config"
)

type GatewayAdapter interface {
	Protocol() config.Protocol
	EndpointPath() string
	AcceptsPath(path string) bool
	RejectMessage(path string) string
	ValidateRequest(body []byte) error
	TransformRequestBody([]byte) ([]byte, error)
	TransformResponseBody([]byte) ([]byte, error)
	TransformSSE(io.Reader) io.Reader
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

var gatewayAdapters = map[string]GatewayAdapter{
	"ds":   DeepSeekResponsesAdapter{},
	"st":   SenseNovaChatAdapter{},
	"std":  StandardResponsesAdapter{},
	"oaic": OpenAICompatibleChatAdapter{},
	"anth": AnthropicMessagesAdapter{},
}

func adapterFor(id string) (GatewayAdapter, bool) {
	adapter, ok := gatewayAdapters[id]
	return adapter, ok
}

func adapterForGateway(gateway config.GatewayConfig) (GatewayAdapter, bool) {
	if adapter, ok := adapterFor(gateway.ID); ok {
		return adapter, true
	}
	switch gateway.Protocol {
	case config.ProtocolResponses:
		return StandardResponsesAdapter{}, true
	case config.ProtocolChat:
		return SenseNovaChatAdapter{}, true
	case config.ProtocolOpenAICompatible:
		return OpenAICompatibleChatAdapter{}, true
	case config.ProtocolAnthropic:
		return AnthropicMessagesAdapter{}, true
	}
	return nil, false
}
