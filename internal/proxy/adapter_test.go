package proxy

import (
	"encoding/json"
	"io"
	"strings"
	"testing"

	"grok-gateway-proxy/internal/config"
)

// The two tables listing the supported gateways — config.DefaultGateways
// (identity: prefix, protocol, base URL) and gatewayAdapters (behaviour) —
// are separate literals in separate packages, so adding a gateway can easily
// update only one of them. That is only caught at request time today, as a
// 500 from adapterFor. This pins the two together.
func TestEveryDefaultGatewayHasAnAdapter(t *testing.T) {
	if len(config.DefaultGateways) == 0 {
		t.Fatal("config.DefaultGateways is empty")
	}
	for id := range config.DefaultGateways {
		if _, ok := adapterFor(id); !ok {
			t.Errorf("gateway %q has no entry in gatewayAdapters; requests to it will fail with 500", id)
		}
	}
	for id := range gatewayAdapters {
		if _, ok := config.DefaultGateways[id]; !ok {
			t.Errorf("gatewayAdapters has an entry for %q, which is not a known gateway", id)
		}
	}
}

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

func TestStandardFiltersPingEventsFromResponsesStream(t *testing.T) {
	adapter := StandardResponsesAdapter{}
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

func TestStandardRenamesLegacyReasoningEvents(t *testing.T) {
	adapter := StandardResponsesAdapter{}
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

func TestStandardRenameDoesNotCorruptQuotedContent(t *testing.T) {
	adapter := StandardResponsesAdapter{}
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

// Event types outside the client's vocabulary are dropped; the legacy
// reasoning names count as known because they are renamed downstream.
func TestStandardFiltersUnknownEventTypes(t *testing.T) {
	adapter := StandardResponsesAdapter{}
	input := "event: response.created\ndata: {\"type\":\"response.created\",\"sequence_number\":0}\n\n" +
		"event: response.apply_patch_call_operation_diff.delta\ndata: {\"type\":\"response.apply_patch_call_operation_diff.delta\",\"delta\":\"diff\"}\n\n" +
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n" +
		"data: [DONE]\n\n"
	want := "event: response.created\ndata: {\"type\":\"response.created\",\"sequence_number\":0}\n\n" +
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n" +
		"data: [DONE]\n\n"
	reader := adapter.TransformSSE(strings.NewReader(input))
	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != want {
		t.Fatalf("unknown event type was not filtered:\nwant: %q\ngot:  %q", want, body)
	}
}

func TestStandardPassesResponsesStreamThroughUnchanged(t *testing.T) {
	adapter := StandardResponsesAdapter{}
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

// TestVercelRenameHandlesSpacedAndNestedSerialization verifies the structured
// type-field rewrite copes with serialization variants the old byte-replace
// approach missed: extra whitespace around the colon, and the legacy event
// name appearing as the value of a different key (e.g. "delta" text) where it
// must survive untouched. Only "type" properties whose value is the legacy
// event name are renamed; other string values are left intact.
func TestStandardRenameHandlesSpacedAndNestedSerialization(t *testing.T) {
	adapter := StandardResponsesAdapter{}
	// Extra spaces around the colon; the legacy name also appears as a "delta"
	// string value, which must NOT be rewritten (it is payload text, not an
	// event name).
	input := "event: response.reasoning.delta\n" +
		`data: {"type" : "response.reasoning.delta","delta":"response.reasoning.delta"}` + "\n\n"
	reader := adapter.TransformSSE(strings.NewReader(input))
	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	// The top-level type field and the event: line must be renamed.
	if !strings.Contains(got, "event: response.reasoning_text.delta") {
		t.Fatalf("event: line was not renamed: %s", got)
	}
	if !strings.Contains(got, `"type" : "response.reasoning_text.delta"`) {
		t.Fatalf("type field was not renamed despite spaced colon: %s", got)
	}
	// The "delta" string value must survive untouched — it is payload text,
	// not the event name.
	if !strings.Contains(got, `"delta":"response.reasoning.delta"`) {
		t.Fatalf("delta string value was wrongly renamed: %s", got)
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

// The upstream payload is not required to be compact: pretty-printed or
// otherwise spaced JSON must be cleaned up just the same. A naive
// bytes.Contains(`"id":""`) pre-filter silently misses {"id": ""}.
func TestSenseNovaSpacedToolCallContinuationKeepsIdentity(t *testing.T) {
	adapter := SenseNovaChatAdapter{}
	input := "data: { \"choices\": [ { \"delta\": { \"tool_calls\": [ { \"index\": 0, \"id\": \"call-1\", \"type\": \"function\", \"function\": { \"name\": \"lookup\", \"arguments\": \"{\" } } ] }, \"finish_reason\": \"\" } ] }\n\n" +
		"data: { \"choices\": [ { \"delta\": { \"tool_calls\": [ { \"index\": 0, \"id\": \"\", \"type\": \"function\", \"function\": { \"name\": \"\", \"arguments\": \"x\" } } ] }, \"finish_reason\": \"\" } ] }\n\n"
	body, err := io.ReadAll(adapter.TransformSSE(strings.NewReader(input)))
	if err != nil {
		t.Fatal(err)
	}
	result := string(body)
	// Whitespace is collapsed so the assertions match regardless of whether
	// the adapter re-marshals a chunk or forwards it verbatim.
	compacted := strings.Join(strings.Fields(result), "")
	if !strings.Contains(compacted, `"id":"call-1"`) || !strings.Contains(compacted, `"name":"lookup"`) {
		t.Fatalf("initial tool-call identity was lost: %s", result)
	}
	if strings.Contains(compacted, `"id":""`) || strings.Contains(compacted, `"name":""`) {
		t.Fatalf("empty continuation identity fields were forwarded: %s", result)
	}
	if !strings.Contains(compacted, `"arguments":"x"`) {
		t.Fatalf("tool-call argument continuation was changed: %s", result)
	}
}

// hasEmptyJSONStringField is the parse pre-filter: it must tolerate
// whitespace between JSON tokens, including none at all.
func TestHasEmptyJSONStringField(t *testing.T) {
	tests := []struct {
		name  string
		body  string
		key   string
		found bool
	}{
		{"compact", `{"id":"","name":"x"}`, "id", true},
		{"spaced after colon", `{"id": "","name":"x"}`, "id", true},
		{"spaced around colon", `{"id" : "","name":"x"}`, "id", true},
		{"newline separated", "{\n  \"id\":\n  \"\",\n  \"name\": \"x\"\n}", "id", true},
		{"non-empty value", `{"id":"call-1"}`, "id", false},
		{"missing key", `{"name":""}`, "id", false},
		{"key as substring", `{"parent_id":""}`, "id", false},
		{"key not a member", `{"values":["id"]}`, "id", false},
		{"empty body", ``, "id", false},
		{"truncated after key", `{"id"`, "id", false},
		{"second occurrence empty", `{"id":"a","id": ""}`, "id", true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := hasEmptyJSONStringField([]byte(test.body), test.key); got != test.found {
				t.Fatalf("hasEmptyJSONStringField(%q, %q) = %v, want %v", test.body, test.key, got, test.found)
			}
		})
	}
}

// Every Responses request sent upstream must conform to the standard
// protocol: xAI-only extensions are stripped regardless of model, while
// everything else survives untouched.
func TestStandardSanitizesResponsesRequestsForAllModels(t *testing.T) {
	adapter := StandardResponsesAdapter{}
	body := []byte(`{"model":"deepseek-v4-flash","stream":true,"stream_tool_calls":true,"tools":[{"type":"x_search"},{"type":"web_search","filters":{"excluded_domains":["evil.example"]}},{"type":"web_search","filters":{"allowed_domains":["docs.example"]}},{"type":"function","name":"lookup","parameters":{}}],"include":["reasoning.encrypted_content","no_inline_citations"],"input":[]}`)
	for _, model := range []string{"deepseek-chat", "deepseek-v4-flash"} {
		upstreamBody, err := adapter.TransformRequestBody(body)
		if err != nil {
			t.Fatal(err)
		}
		var payload map[string]any
		if err := json.Unmarshal(upstreamBody, &payload); err != nil {
			t.Fatal(err)
		}
		if _, exists := payload["stream_tool_calls"]; exists {
			t.Fatalf("non-standard stream_tool_calls survived for %q: %s", model, upstreamBody)
		}
		tools := payload["tools"].([]any)
		// x_search is dropped; the excluded-domains web_search survives with
		// its filter renamed to blocked_domains.
		if len(tools) != 3 {
			t.Fatalf("x_search was not dropped, or a standard tool was lost, for %q: %s", model, upstreamBody)
		}
		if tools[0].(map[string]any)["type"] != "web_search" ||
			tools[1].(map[string]any)["type"] != "web_search" ||
			tools[2].(map[string]any)["type"] != "function" {
			t.Fatalf("standard tools were not preserved for %q: %s", model, upstreamBody)
		}
		if _, exists := tools[0].(map[string]any)["filters"].(map[string]any)["excluded_domains"]; exists {
			t.Fatalf("excluded_domains survived for %q: %s", model, upstreamBody)
		}
		blocked := tools[0].(map[string]any)["filters"].(map[string]any)["blocked_domains"].([]any)
		if len(blocked) != 1 || blocked[0] != "evil.example" {
			t.Fatalf("blocklist was not preserved as blocked_domains for %q: %s", model, upstreamBody)
		}
		allowed := tools[1].(map[string]any)["filters"].(map[string]any)["allowed_domains"].([]any)
		if len(allowed) != 1 || allowed[0] != "docs.example" {
			t.Fatalf("allowed_domains was lost for %q: %s", model, upstreamBody)
		}
		include := payload["include"].([]any)
		if len(include) != 1 || include[0] != "reasoning.encrypted_content" {
			t.Fatalf("non-standard include value survived for %q: %s", model, upstreamBody)
		}
		if payload["model"] != "deepseek-v4-flash" {
			t.Fatalf("request fields were changed unexpectedly for %q: %s", model, upstreamBody)
		}
	}
}

// A request that already conforms to the standard protocol must pass through
// byte-for-byte.
func TestStandardLeavesConformantRequestsUnchanged(t *testing.T) {
	adapter := StandardResponsesAdapter{}
	body := []byte(`{"model":"deepseek-v4-flash","stream":true,"input":[]}`)
	transformed, err := adapter.TransformRequestBody(body)
	if err != nil {
		t.Fatal(err)
	}
	if string(transformed) != string(body) {
		t.Fatalf("conformant request was rewritten: %s", transformed)
	}
}

// Pings and event types outside the client's vocabulary are dropped, legacy
// reasoning event names are renamed to the standard reasoning_text variants,
// and known events (and the [DONE] sentinel) are preserved — identically for
// every model.
func TestStandardFiltersPingsAndUnknownEvents(t *testing.T) {
	adapter := StandardResponsesAdapter{}
	input := "event: ping\ndata: {\"type\":\"ping\",\"cost\":\"0\"}\n\n" +
		"event: ping\ndata:\n\n" +
		"event: response.created\ndata: {\"type\":\"response.created\",\"sequence_number\":0}\n\n" +
		"event: response.reasoning.delta\ndata: {\"type\":\"response.reasoning.delta\",\"sequence_number\":1,\"delta\":\"think\"}\n\n" +
		"event: response.apply_patch_call_operation_diff.delta\ndata: {\"type\":\"response.apply_patch_call_operation_diff.delta\",\"delta\":\"diff\"}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"sequence_number\":2}\n\n" +
		"data: [DONE]\n\n"
	want := "event: response.created\ndata: {\"type\":\"response.created\",\"sequence_number\":0}\n\n" +
		"event: response.reasoning_text.delta\ndata: {\"type\":\"response.reasoning_text.delta\",\"sequence_number\":1,\"delta\":\"think\"}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"sequence_number\":2}\n\n" +
		"data: [DONE]\n\n"
	for _, model := range []string{"deepseek-chat", "deepseek-v4-flash"} {
		reader := adapter.TransformSSE(strings.NewReader(input))
		body, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != want {
			t.Fatalf("stream was not aligned for %q:\nwant: %q\ngot:  %q", model, want, body)
		}
	}
}
