package main

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func TestSenseNovaTransformsToolCallTypesWithoutChangingToolDefinitions(t *testing.T) {
	body := []byte(`{"tools":[{"type":"function","function":{"name":"lookup"}}],"messages":[{"role":"assistant","tool_calls":[{"id":"call-1","type":"function","function":{"name":"lookup","arguments":"{}"}}]}]}`)
	adapter := SenseNovaChatAdapter{}
	upstreamBody, err := adapter.TransformRequestBody(body)
	if err != nil {
		t.Fatal(err)
	}
	var upstream map[string]any
	if err := json.Unmarshal(upstreamBody, &upstream); err != nil {
		t.Fatal(err)
	}
	tools := upstream["tools"].([]any)
	if tools[0].(map[string]any)["type"] != "function" {
		t.Fatal("tool definition type was unexpectedly changed")
	}
	messages := upstream["messages"].([]any)
	calls := messages[0].(map[string]any)["tool_calls"].([]any)
	if calls[0].(map[string]any)["type"] != "function_call" {
		t.Fatalf("SenseNova tool call type was not converted: %s", upstreamBody)
	}

	clientBody, err := adapter.TransformResponseBody([]byte(`{"choices":[{"message":{"tool_calls":[{"type":"function_call","function":{"name":"lookup","arguments":"{}"}}]}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(clientBody), `"type":"function_call"`) || !strings.Contains(string(clientBody), `"type":"function"`) {
		t.Fatalf("client tool call type was not converted back: %s", clientBody)
	}
}

func TestSenseNovaTransformsStreamingToolCalls(t *testing.T) {
	adapter := SenseNovaChatAdapter{}
	reader := adapter.TransformSSE(strings.NewReader("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"type\":\"function_call\"}]}}]}\n\ndata: [DONE]\n\n"))
	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), `"type":"function_call"`) || !strings.Contains(string(body), `"type":"function"`) {
		t.Fatalf("streaming tool call type was not converted: %s", body)
	}
}
