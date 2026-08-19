package proxy

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"grok-gateway-proxy/internal/config"
	"grok-gateway-proxy/internal/store"
)

func TestConvertResponsesToV3InjectsPromoHeaders(t *testing.T) {
	body := []byte(`{"model":"zai/glm-5.2","input":"hi","max_output_tokens":100,"instructions":"be brief"}`)
	out, err := convertResponsesToV3(body, "fx/0.0.3")
	if err != nil {
		t.Fatal(err)
	}
	var v3 map[string]any
	if err := json.Unmarshal(out, &v3); err != nil {
		t.Fatal(err)
	}
	headers, ok := v3["headers"].(map[string]any)
	if !ok {
		t.Fatalf("missing headers object: %s", out)
	}
	if headers["user-agent"] != "fx/0.0.3" || headers["x-title"] != "fx" {
		t.Fatalf("promo headers not injected correctly: %+v", headers)
	}
	if v3["maxOutputTokens"] != float64(100) {
		t.Fatalf("maxOutputTokens mismatch: %v", v3["maxOutputTokens"])
	}
	if v3["reasoning"] != "xhigh" {
		t.Fatalf("default reasoning should be xhigh, got %v", v3["reasoning"])
	}
	prompt, ok := v3["prompt"].([]any)
	if !ok || len(prompt) != 2 {
		t.Fatalf("expected system+user prompt, got: %s", out)
	}
	sys := prompt[0].(map[string]any)
	if sys["role"] != "system" || sys["content"] != "be brief" {
		t.Fatalf("system part mismatch: %+v", sys)
	}
	user := prompt[1].(map[string]any)
	parts := user["content"].([]any)
	if parts[0].(map[string]any)["text"] != "hi" {
		t.Fatalf("user part mismatch: %s", out)
	}
}

func TestConvertResponsesToV3ConversationsAndTools(t *testing.T) {
	body := []byte(`{
		"model":"zai/glm-5.2-fast",
		"instructions":"you are helpful",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"what is 2+2"}]},
			{"type":"function_call","call_id":"call-1","name":"calc","arguments":"{\"expr\":\"2+2\"}"},
			{"type":"function_call_output","call_id":"call-1","output":"4"}
		],
		"tools":[{"type":"function","name":"calc","description":"calculator","parameters":{"type":"object","properties":{"expr":{"type":"string"}}}}],
		"tool_choice":{"type":"function","name":"calc"},
		"reasoning":{"effort":"high"},
		"text":{"format":{"type":"json_object"}}
	}`)
	out, err := convertResponsesToV3(body, "fx/0.0.3")
	if err != nil {
		t.Fatal(err)
	}
	var v3 map[string]any
	if err := json.Unmarshal(out, &v3); err != nil {
		t.Fatal(err)
	}
	if v3["reasoning"] != "high" {
		t.Fatalf("reasoning effort not mapped: %v", v3["reasoning"])
	}
	rf, ok := v3["responseFormat"].(map[string]any)
	if !ok || rf["type"] != "json" {
		t.Fatalf("responseFormat missing: %s", out)
	}
	tools := v3["tools"].([]any)
	tool := tools[0].(map[string]any)
	if tool["name"] != "calc" || tool["inputSchema"] == nil {
		t.Fatalf("tool conversion mismatch: %+v", tool)
	}
	tc := v3["toolChoice"].(map[string]any)
	if tc["type"] != "tool" || tc["toolName"] != "calc" {
		t.Fatalf("toolChoice mismatch: %+v", tc)
	}
	prompt := v3["prompt"].([]any)
	if len(prompt) != 4 {
		t.Fatalf("expected 4 prompt parts (system,user,assistant,user-tool), got %d: %s", len(prompt), out)
	}
	assistant := prompt[2].(map[string]any)
	if assistant["role"] != "assistant" {
		t.Fatalf("expected assistant part, got %+v", assistant)
	}
	toolMsg := prompt[3].(map[string]any)
	if toolMsg["role"] != "tool" {
		t.Fatalf("tool result should be a tool message, got %+v", toolMsg)
	}
	resultParts := toolMsg["content"].([]any)
	result := resultParts[0].(map[string]any)
	if result["type"] != "tool-result" || result["toolCallId"] != "call-1" || result["toolName"] != "calc" {
		t.Fatalf("tool result part mismatch: %+v", result)
	}
}

func TestVercelFXSSEToResponsesJSON(t *testing.T) {
	stream := "data: {\"type\":\"text-delta\",\"delta\":\"Hello\"}\n\n" +
		"data: {\"type\":\"reasoning-delta\",\"delta\":\"think\"}\n\n" +
		"data: {\"type\":\"text-delta\",\"delta\":\" world\"}\n\n" +
		"data: {\"type\":\"finish\",\"finishReason\":{\"unified\":\"stop\"},\"usage\":{\"inputTokens\":{\"total\":12},\"outputTokens\":{\"total\":3}}}\n\n" +
		"data: [DONE]\n\n"
	out, err := vercelFXSSEToResponses("zai/glm-5.2", strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	var resp map[string]any
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatal(err)
	}
	if resp["object"] != "response" || resp["status"] != "completed" {
		t.Fatalf("unexpected response envelope: %s", out)
	}
	if resp["model"] != "zai/glm-5.2" {
		t.Fatalf("model mismatch: %s", out)
	}
	output := resp["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("expected reasoning+message output, got %d: %s", len(output), out)
	}
	reasoning := output[0].(map[string]any)
	if reasoning["type"] != "reasoning" {
		t.Fatalf("expected reasoning item first: %s", out)
	}
	message := output[1].(map[string]any)
	if message["type"] != "message" {
		t.Fatalf("expected message item: %s", out)
	}
	content := message["content"].([]any)
	text := content[0].(map[string]any)["text"]
	if text != "Hello world" {
		t.Fatalf("text not assembled: %q", text)
	}
	usage := resp["usage"].(map[string]any)
	if usage["input_tokens"] != float64(12) || usage["output_tokens"] != float64(3) {
		t.Fatalf("usage mismatch: %+v", usage)
	}
}

func TestVercelFXSSEStreamReaderEmitsResponsesEvents(t *testing.T) {
	stream := "data: {\"type\":\"text-delta\",\"delta\":\"Hello\"}\n\n" +
		"data: {\"type\":\"tool-input-start\",\"id\":\"call-1\",\"toolName\":\"calc\"}\n\n" +
		"data: {\"type\":\"tool-input-delta\",\"id\":\"call-1\",\"delta\":\"{\\\"expr\\\":\\\"\"}\n\n" +
		"data: {\"type\":\"tool-input-delta\",\"id\":\"call-1\",\"delta\":\"2+2\\\"}\"}\n\n" +
		"data: {\"type\":\"finish\",\"finishReason\":{\"unified\":\"tool-calls\"},\"usage\":{\"inputTokens\":{\"total\":5},\"outputTokens\":{\"total\":2}}}\n\n"
	reader := newVercelFXSSEReader(strings.NewReader(stream), "zai/glm-5.2")
	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	result := string(body)
	for _, want := range []string{
		"event: response.created",
		"event: response.output_item.added",
		"event: response.output_text.delta",
		"event: response.function_call_arguments.delta",
		"event: response.function_call_arguments.done",
		"event: response.output_item.done",
		"event: response.completed",
	} {
		if !strings.Contains(result, want) {
			t.Fatalf("stream missing %s: %s", want, result)
		}
	}
	if !strings.Contains(result, `"call_id":"call-1"`) {
		t.Fatalf("tool call identity lost: %s", result)
	}
	if !strings.Contains(result, `"arguments":"{\"expr\":\"2+2\"}"`) {
		t.Fatalf("tool arguments not assembled: %s", result)
	}
	if strings.Contains(result, "[DONE]") {
		t.Fatalf("unexpected [DONE] in converted stream: %s", result)
	}
}

// Strict Responses clients (async-openai) require every SSE event to carry a
// strictly increasing sequence_number, matching what the real Vercel gateway
// emits.
func TestVercelFXSSEEventsHaveIncreasingSequenceNumber(t *testing.T) {
	stream := "data: {\"type\":\"text-delta\",\"delta\":\"a\"}\n\n" +
		"data: {\"type\":\"text-delta\",\"delta\":\"b\"}\n\n" +
		"data: {\"type\":\"finish\",\"finishReason\":{\"unified\":\"stop\"},\"usage\":{\"inputTokens\":{\"total\":4},\"outputTokens\":{\"total\":1}}}\n\n"
	reader := newVercelFXSSEReader(strings.NewReader(stream), "zai/glm-5.2")
	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	var lastSeq int64 = -1
	for _, line := range strings.Split(string(body), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			t.Fatal(err)
		}
		seq, ok := ev["sequence_number"].(float64)
		if !ok {
			t.Fatalf("event missing sequence_number: %s", line)
		}
		if int64(seq) != lastSeq+1 {
			t.Fatalf("sequence_number not strictly increasing: got %v after %d: %s", seq, lastSeq, line)
		}
		lastSeq = int64(seq)
	}
	if lastSeq < 0 {
		t.Fatal("no SSE events emitted")
	}
}

// The async-openai parser deserializes the usage object on response.created
// strictly, so every usage-bearing event must carry the full details shape.
func TestVercelFXSSECreatedEventHasFullUsageShape(t *testing.T) {
	stream := "data: {\"type\":\"text-delta\",\"delta\":\"hi\"}\n\n" +
		"data: {\"type\":\"finish\",\"finishReason\":{\"unified\":\"stop\"},\"usage\":{\"inputTokens\":{\"total\":4},\"outputTokens\":{\"total\":1}}}\n\n"
	reader := newVercelFXSSEReader(strings.NewReader(stream), "zai/glm-5.2")
	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	result := string(body)
	for _, line := range strings.Split(result, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			t.Fatal(err)
		}
		resp, ok := ev["response"].(map[string]any)
		if !ok {
			continue
		}
		usage, ok := resp["usage"].(map[string]any)
		if !ok {
			t.Fatalf("response event missing usage: %s", line)
		}
		if _, ok := usage["input_tokens_details"].(map[string]any); !ok {
			t.Fatalf("usage missing input_tokens_details: %s", line)
		}
		if _, ok := usage["output_tokens_details"].(map[string]any); !ok {
			t.Fatalf("usage missing output_tokens_details: %s", line)
		}
	}
}

func TestResponsesToolResultUsesFallbackToolName(t *testing.T) {
	body := []byte(`{"model":"zai/glm-5.2","input":[{"type":"function_call_output","call_id":"call-1","output":"ok"}]}`)
	out, err := convertResponsesToV3(body, "fx/0.0.3")
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatal(err)
	}
	prompt := payload["prompt"].([]any)
	toolMessage := prompt[0].(map[string]any)
	part := toolMessage["content"].([]any)[0].(map[string]any)
	if part["toolName"] != "tool" {
		t.Fatalf("missing fallback tool name: %+v", part)
	}
}

func TestVercelFXDoesNotDuplicateTerminalToolCall(t *testing.T) {
	t.Skip("covered by TestVercelFXTerminalToolCallSnapshot in vercelfx_regression_test.go")
	stream := "data: {\"type\":\"tool-input-start\",\"id\":\"call-1\",\"toolName\":\"calc\"}\\n\\n" +
		"data: {\"type\":\"tool-input-delta\",\"id\":\"call-1\",\"delta\":\"{\\\\\"x\\\\\":1}\"}\\n\\n" +
		"data: {\"type\":\"tool-call\",\"toolCallId\":\"call-1\",\"toolName\":\"calc\",\"input\":{\"x\":1}}\\n\\n" +
		"data: {\"type\":\"finish\",\"finishReason\":{\"unified\":\"tool-calls\"}}\\n\\n"
	out, err := vercelFXSSEToResponses("zai/glm-5.2", strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if err := json.Unmarshal(out, &response); err != nil {
		t.Fatal(err)
	}
	output := response["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("expected one function call, got %d: %s", len(output), out)
	}
	call := output[0].(map[string]any)
	if call["type"] != "function_call" || call["arguments"] != `{"x":1}` {
		t.Fatalf("terminal tool snapshot was duplicated or malformed: %+v", call)
	}
}

func TestExtractFXUsagePreservesCacheUnsupportedState(t *testing.T) {
	t.Skip("covered by TestExtractFXUsageCacheMissIsCacheSupported and TestExtractFXUsageNoCacheFieldsIsUnsupported")
	withoutCache := []byte(`data: {"type":"finish","usage":{"inputTokens":{"total":100},"outputTokens":{"total":5}}}\n\ndata: [DONE]\n\n`)
	usage := extractFXUsage(withoutCache)
	if !usage.UsagePresent || usage.CacheSupported || usage.InputTokens != 100 || usage.PromptTokens != 100 {
		t.Fatalf("unexpected unsupported-cache usage: %+v", usage)
	}

	withCache := []byte(`data: {"type":"finish","usage":{"inputTokens":{"total":100,"cacheRead":60},"outputTokens":{"total":5}}}\n\ndata: [DONE]\n\n`)
	usage = extractFXUsage(withCache)
	if !usage.CacheSupported || usage.CacheReadTokens != 60 || usage.InputTokens != 40 || usage.PromptTokens != 100 {
		t.Fatalf("unexpected cached usage: %+v", usage)
	}
}

func TestProxyVercelFXDisguiseEndToEnd(t *testing.T) {
	var gotPath, gotUA, gotReferer, gotTitle string
	var gotBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotUA = r.Header.Get("User-Agent")
		gotReferer = r.Header.Get("HTTP-Referer")
		gotTitle = r.Header.Get("X-Title")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"text-delta\",\"delta\":\"pong\"}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"finish\",\"finishReason\":{\"unified\":\"stop\"},\"usage\":{\"inputTokens\":{\"total\":4},\"outputTokens\":{\"total\":1}}}\n\n"))
	}))
	defer upstream.Close()

	st, err := store.OpenStore(t.TempDir() + "/proxy.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := config.DefaultConfig(t.TempDir() + "/config.json")
	gateway := cfg.Gateways["ve"]
	gateway.BaseURL = upstream.URL + "/v1"
	gateway.FXDisguiseEnabled = true
	gateway.FXDisguiseUserAgent = "fx/0.0.3"
	cfg.Gateways["ve"] = gateway
	p := &Proxy{Config: cfg, Store: st, Logger: slog.Default(), Client: upstream.Client()}

	requestBody := []byte(`{"model":"zai/glm-5.2","input":"hi","stream":false}`)
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/ve/responses", strings.NewReader(string(requestBody)))
	req.Header.Set("Authorization", "Bearer vck_test")
	recorder := httptest.NewRecorder()
	p.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if gotPath != "/v1/v3/ai/language-model" && gotPath != "/v3/ai/language-model" {
		t.Fatalf("unexpected upstream path: %s", gotPath)
	}
	if gotUA != "fx/0.0.3" {
		t.Fatalf("fx UA not applied: %q", gotUA)
	}
	if !strings.Contains(gotReferer, "vercel-labs/fx") {
		t.Fatalf("fx referer not applied: %q", gotReferer)
	}
	if gotTitle != "fx" {
		t.Fatalf("fx title not applied: %q", gotTitle)
	}
	headers, ok := gotBody["headers"].(map[string]any)
	if !ok || headers["user-agent"] != "fx/0.0.3" || headers["x-title"] != "fx" {
		t.Fatalf("promo headers not injected into upstream body: %s", mustJSON(t, gotBody))
	}
	if _, ok := gotBody["prompt"]; !ok {
		t.Fatalf("upstream body not converted to v3 prompt: %s", mustJSON(t, gotBody))
	}

	var clientResp map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &clientResp); err != nil {
		t.Fatalf("client did not receive a JSON Responses object: %s", recorder.Body.String())
	}
	if clientResp["object"] != "response" || clientResp["status"] != "completed" {
		t.Fatalf("unexpected client response: %s", recorder.Body.String())
	}

	logs, err := st.List(t.Context(), store.LogFilter{Limit: 1})
	if err != nil || len(logs) != 1 {
		t.Fatalf("expected one log, got %d, err=%v", len(logs), err)
	}
	detail, err := st.Get(t.Context(), logs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(detail.UpstreamBody), `"user-agent":"fx/0.0.3"`) {
		t.Fatalf("logged upstream body missing injected headers: %s", detail.UpstreamBody)
	}
	if !logs[0].Success {
		t.Fatalf("FX request should be logged as success: %+v", logs[0])
	}
}

func TestProxyVercelFXDisabledKeepsResponsesPassthrough(t *testing.T) {
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-1","object":"response","status":"completed","output":[]}`))
	}))
	defer upstream.Close()

	st, err := store.OpenStore(t.TempDir() + "/proxy.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := config.DefaultConfig(t.TempDir() + "/config.json")
	gateway := cfg.Gateways["ve"]
	gateway.BaseURL = upstream.URL + "/v1"
	cfg.Gateways["ve"] = gateway
	p := &Proxy{Config: cfg, Store: st, Logger: slog.Default(), Client: upstream.Client()}

	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/ve/responses", strings.NewReader(`{"model":"m","input":"hi","stream":false}`))
	recorder := httptest.NewRecorder()
	p.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if gotPath != "/v1/responses" {
		t.Fatalf("FX disabled should keep /v1/responses, got %s", gotPath)
	}
}

func TestConfigValidatesFXDisguiseOnlyOnVercel(t *testing.T) {
	cfg := config.DefaultConfig(t.TempDir() + "/config.json")
	// enabling FX on the ve gateway is fine
	gateway := cfg.Gateways["ve"]
	gateway.FXDisguiseEnabled = true
	cfg.Gateways["ve"] = gateway
	if err := cfg.Validate(); err != nil {
		t.Fatalf("ve FX enable should validate: %v", err)
	}
	// enabling FX on a non-ve gateway must fail
	gateway = cfg.Gateways["st"]
	gateway.FXDisguiseEnabled = true
	cfg.Gateways["st"] = gateway
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "only supported on the ve gateway") {
		t.Fatalf("expected ve-only validation error, got: %v", err)
	}
}

func TestConfigPatchFXDisguise(t *testing.T) {
	cfg := config.DefaultConfig(t.TempDir() + "/config.json")
	patched, err := cfg.PatchGateway("ve", config.GatewayPatch{
		FXDisguiseEnabled:   boolPtr(true),
		FXDisguiseUserAgent: stringPtr("fx/0.0.3"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !patched.FXDisguiseEnabled || patched.FXDisguiseUserAgent != "fx/0.0.3" {
		t.Fatalf("FX disguise not patched: %+v", patched)
	}
	if !cfg.Gateways["ve"].FXDisguiseEnabled {
		t.Fatalf("config not persisted: %+v", cfg.Gateways["ve"])
	}
	// missing UA must be rejected when enabling
	if _, err := cfg.PatchGateway("ve", config.GatewayPatch{
		FXDisguiseEnabled:   boolPtr(true),
		FXDisguiseUserAgent: stringPtr(""),
	}); err == nil || !strings.Contains(err.Error(), "fx_disguise_user_agent") {
		t.Fatalf("expected UA-required error, got: %v", err)
	}
}

func boolPtr(v bool) *bool       { return &v }
func stringPtr(v string) *string { return &v }
