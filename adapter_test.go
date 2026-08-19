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

func TestSenseNovaDropsIncompleteToolCallHistory(t *testing.T) {
	body := []byte(`{"model":"demo","messages":[` +
		`{"role":"user","content":"inspect the project"},` +
		`{"role":"assistant","content":"I will inspect it.","tool_calls":[` +
		`{"id":"","type":"function","function":{"name":"","arguments":"{}"}},` +
		`{"id":"call-valid","type":"function","function":{"name":"lookup","arguments":"{}"}}]},` +
		`{"role":"tool","tool_call_id":"","content":"orphan result"},` +
		`{"role":"tool","tool_call_id":"call-valid","content":"valid result"}` + `]}`)

	adapter := SenseNovaChatAdapter{}
	upstreamBody, err := adapter.TransformRequestBody(body)
	if err != nil {
		t.Fatal(err)
	}

	var payload map[string]any
	if err := json.Unmarshal(upstreamBody, &payload); err != nil {
		t.Fatal(err)
	}
	messages := payload["messages"].([]any)
	if len(messages) != 3 {
		t.Fatalf("incomplete tool history was not removed: %s", upstreamBody)
	}
	assistant := messages[1].(map[string]any)
	calls := assistant["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("expected one valid tool call, got %d: %s", len(calls), upstreamBody)
	}
	call := calls[0].(map[string]any)
	if call["id"] != "call-valid" || call["type"] != "function_call" {
		t.Fatalf("valid tool call was changed incorrectly: %s", upstreamBody)
	}
	if strings.Contains(string(upstreamBody), `"name":""`) || strings.Contains(string(upstreamBody), `"tool_call_id":""`) {
		t.Fatalf("empty tool-call fields survived sanitization: %s", upstreamBody)
	}
	toolMessage := messages[2].(map[string]any)
	if toolMessage["role"] != "tool" || toolMessage["tool_call_id"] != "call-valid" {
		t.Fatalf("valid tool result was removed or changed: %s", upstreamBody)
	}
}

func TestSenseNovaDropsToolMessagesForRemovedCalls(t *testing.T) {
	body := []byte(`{"messages":[` +
		`{"role":"assistant","tool_calls":[{"id":"call-bad","type":"function","function":{"name":"lookup","arguments":""}}]},` +
		`{"role":"tool","tool_call_id":"call-bad","content":"result"}` +
		`]}`)

	adapter := SenseNovaChatAdapter{}
	upstreamBody, err := adapter.TransformRequestBody(body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(upstreamBody), `tool_calls`) || strings.Contains(string(upstreamBody), `"role":"tool"`) {
		t.Fatalf("orphaned incomplete tool history survived: %s", upstreamBody)
	}
}

// The response-side conversion must only rewrite tool-call entries inside
// tool_calls arrays; any other "type" property (e.g. echoed tool definitions)
// is left byte-for-byte intact.
func TestSenseNovaResponseTransformScopedToToolCalls(t *testing.T) {
	adapter := SenseNovaChatAdapter{}
	body := []byte(`{"tools":[{"type":"function_call","function":{"name":"defn"}}],"choices":[{"message":{"tool_calls":[{"id":"call-1","type":"function_call","function":{"name":"lookup","arguments":"{}"}}]},"finish_reason":""}]}`)
	clientBody, err := adapter.TransformResponseBody(body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(clientBody), `"tools":[{"type":"function_call"`) {
		t.Fatalf("non-tool_calls type was rewritten: %s", clientBody)
	}
	if !strings.Contains(string(clientBody), `"tool_calls":[{"id":"call-1","type":"function"`) {
		t.Fatalf("tool_calls type was not converted: %s", clientBody)
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

func TestSenseNovaStreamingToolCallContinuationKeepsIdentity(t *testing.T) {
	adapter := SenseNovaChatAdapter{}
	input := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call-1\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{\"}}]},\"finish_reason\":\"\"}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"\",\"type\":\"function\",\"function\":{\"name\":\"\",\"arguments\":\"x\"}}]},\"finish_reason\":\"\"}]}\n\n"
	reader := adapter.TransformSSE(strings.NewReader(input))
	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	result := string(body)
	if !strings.Contains(result, `"id":"call-1"`) || !strings.Contains(result, `"name":"lookup"`) {
		t.Fatalf("initial tool-call identity was lost: %s", result)
	}
	if strings.Contains(result, `"id":""`) || strings.Contains(result, `"name":""`) {
		t.Fatalf("empty continuation identity fields were forwarded: %s", result)
	}
	if !strings.Contains(result, `"arguments":"x"`) {
		t.Fatalf("tool-call argument continuation was changed: %s", result)
	}
}

func TestOpenCodeSelectsMuseProfileByModel(t *testing.T) {
	adapter := OpenCodeResponsesAdapter{}
	profile := adapter.ProfileForModel("muse-spark-1.2")
	if profile == nil || profile.ID() != "muse-spark-1.2" {
		t.Fatalf("Muse profile was not selected: %#v", profile)
	}

	body := []byte(`{"model":"muse-spark-1.2","stream":true,"stream_tool_calls":true,"input":[]}`)
	upstreamBody, err := profile.TransformRequestBody(body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(upstreamBody), `"stream_tool_calls"`) {
		t.Fatalf("unsupported Muse parameter survived: %s", upstreamBody)
	}
	if !strings.Contains(string(upstreamBody), `"model":"muse-spark-1.2"`) {
		t.Fatalf("Muse request was changed unexpectedly: %s", upstreamBody)
	}

	if other := adapter.ProfileForModel("deepseek-chat"); other != nil {
		t.Fatalf("DeepSeek unexpectedly selected the Muse profile: %#v", other)
	}
}

func TestMuseProfileLeavesRequestsWithoutUnsupportedParameterUnchanged(t *testing.T) {
	profile := MuseSpark12Profile{}
	body := []byte(`{"model":"muse-spark-1.2","stream":true,"input":[]}`)
	transformed, err := profile.TransformRequestBody(body)
	if err != nil {
		t.Fatal(err)
	}
	if string(transformed) != string(body) {
		t.Fatalf("request without stream_tool_calls was rewritten: %s", transformed)
	}
}
