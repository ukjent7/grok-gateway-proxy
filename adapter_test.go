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

	clientBody, err := adapter.TransformResponseBody([]byte(`{"choices":[{"message":{"tool_calls":[{"type":"function_call","function":{"name":"lookup","arguments":"{}"}}]},"finish_reason":""}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(clientBody), `"type":"function_call"`) || !strings.Contains(string(clientBody), `"type":"function"`) {
		t.Fatalf("client tool call type was not converted back: %s", clientBody)
	}
	if !strings.Contains(string(clientBody), `"finish_reason":null`) {
		t.Fatalf("empty finish_reason was not normalized to null: %s", clientBody)
	}
}

func TestVercelFiltersPingEventsFromResponsesStream(t *testing.T) {
	adapter := VercelResponsesAdapter{}
	input := "event: ping\ndata: {\"type\":\"ping\"}\n\n" +
		"event: response.created\ndata: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"r1\"}}\n\n" +
		"event: ping\ndata: {\"type\":\"ping\"}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\"}}\n\n" +
		"data: [DONE]\n\n"
	want := "event: response.created\ndata: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"r1\"}}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\"}}\n\n" +
		"data: [DONE]\n\n"
	reader := adapter.TransformSSE(strings.NewReader(input))
	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != want {
		t.Fatalf("ping events were not filtered:\nwant: %q\ngot:  %q", want, body)
	}
}

func TestVercelRenamesLegacyReasoningEvents(t *testing.T) {
	adapter := VercelResponsesAdapter{}
	input := "event: response.reasoning.delta\ndata: {\"type\":\"response.reasoning.delta\",\"sequence_number\":3,\"item_id\":\"rs_1\",\"output_index\":0,\"content_index\":0,\"delta\":\"thinking\"}\n\n" +
		"event: response.reasoning.done\ndata: {\"type\":\"response.reasoning.done\",\"sequence_number\":26,\"item_id\":\"rs_1\",\"output_index\":0,\"content_index\":0,\"text\":\"thinking\"}\n\n" +
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"sequence_number\":30,\"delta\":\"hello\"}\n\n"
	want := strings.ReplaceAll(input, "response.reasoning.delta", "response.reasoning_text.delta")
	want = strings.ReplaceAll(want, "response.reasoning.done", "response.reasoning_text.done")
	reader := adapter.TransformSSE(strings.NewReader(input))
	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != want {
		t.Fatalf("legacy reasoning events were not renamed:\nwant: %q\ngot:  %q", want, body)
	}
}

func TestVercelRenameDoesNotCorruptQuotedContent(t *testing.T) {
	adapter := VercelResponsesAdapter{}
	// The old event name quoted inside delta text must survive untouched.
	input := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"the event is response.reasoning.delta\"}\n\n"
	reader := adapter.TransformSSE(strings.NewReader(input))
	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != input {
		t.Fatalf("quoted content was corrupted:\nwant: %q\ngot:  %q", input, body)
	}
}

func TestVercelPassesResponsesStreamThroughUnchanged(t *testing.T) {
	adapter := VercelResponsesAdapter{}
	input := "event: response.created\ndata: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"r1\",\"status\":\"in_progress\"}}\n\n" +
		"event: response.reasoning_text.delta\ndata: {\"type\":\"response.reasoning_text.delta\",\"delta\":\"think\"}\n\n" +
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"status\":\"completed\"}}\n\n"
	reader := adapter.TransformSSE(strings.NewReader(input))
	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != input {
		t.Fatalf("stream was reserialized or changed unexpectedly:\nwant: %q\ngot:  %q", input, body)
	}
}

func TestSenseNovaTransformsStreamingToolCalls(t *testing.T) {
	adapter := SenseNovaChatAdapter{}
	input := "data: {\"id\":\"chunk-1\",\"created\":1,\"model\":\"demo\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"type\":\"function_call\"}]},\"finish_reason\":\"\"}],\"request_id\":\"chunk-1\"}\n\ndata: [DONE]\n\n"
	expected := strings.ReplaceAll(input, `"type":"function_call"`, `"type":"function"`)
	expected = strings.ReplaceAll(expected, `"finish_reason":""`, `"finish_reason":null`)
	reader := adapter.TransformSSE(strings.NewReader(input))
	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != expected {
		t.Fatalf("streaming response was reserialized or changed unexpectedly:\nwant: %s\ngot:  %s", expected, body)
	}
	if strings.Contains(string(body), `"type":"function_call"`) || !strings.Contains(string(body), `"type":"function"`) {
		t.Fatalf("streaming tool call type was not converted: %s", body)
	}
	if !strings.Contains(string(body), `"finish_reason":null`) {
		t.Fatalf("streaming empty finish_reason was not normalized to null: %s", body)
	}
}
