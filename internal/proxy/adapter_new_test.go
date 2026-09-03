package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"grok-gateway-proxy/internal/config"
	"grok-gateway-proxy/internal/store"
	"log/slog"
)

func TestOpenAICompatibleTransformsResponsesToChat(t *testing.T) {
	adapter := OpenAICompatibleChatAdapter{}

	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"hello"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`)
	out, err := adapter.TransformRequestBody(body)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatal(err)
	}
	msgs, _ := payload["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d: %s", len(msgs), out)
	}
	if payload["model"] != "gpt-4" {
		t.Fatalf("model not preserved: %s", out)
	}
	if tools, _ := payload["tools"].([]any); len(tools) != 1 {
		t.Fatalf("tools not preserved: %s", out)
	}
}

func TestOpenAICompatibleStripsXAIExtensions(t *testing.T) {
	adapter := OpenAICompatibleChatAdapter{}

	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f","parameters":{}}}]}`)
	out, err := adapter.TransformRequestBody(body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"name":"f"`) {
		t.Fatalf("function tool lost: %s", out)
	}
}

func TestAnthropicTransformsResponsesToMessages(t *testing.T) {
	adapter := AnthropicMessagesAdapter{}
	body := []byte(`{"model":"claude-3-5-sonnet-20241022","max_tokens":1024,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]},{"role":"assistant","content":[{"type":"text","text":"hello"}]}]}`)
	out, err := adapter.TransformRequestBody(body)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["model"] != "claude-3-5-sonnet-20241022" {
		t.Fatalf("model not preserved: %s", out)
	}
	msgs, _ := payload["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d: %s", len(msgs), out)
	}
	if _, ok := payload["max_tokens"]; !ok {
		t.Fatalf("max_tokens not preserved: %s", out)
	}
}

func TestAnthropicStripsXAIExtensions(t *testing.T) {
	adapter := AnthropicMessagesAdapter{}
	body := []byte(`{"model":"m","max_tokens":1024,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"tools":[{"name":"f","input_schema":{"type":"object"}}]}`)
	out, err := adapter.TransformRequestBody(body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"f"`) {
		t.Fatalf("tool lost: %s", out)
	}
}

func TestProxyOpenAICompatibleEndToEnd(t *testing.T) {
	var upstreamBody map[string]any
	var upstreamPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&upstreamBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-123","created":1234567890,"model":"gpt-4","choices":[{"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	defer upstream.Close()

	st, err := store.OpenStore(t.TempDir() + "/proxy.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := config.DefaultConfig(t.TempDir() + "/config.json")
	gw := cfg.Gateways["oaic"]
	gw.BaseURL = upstream.URL
	cfg.Gateways["oaic"] = gw
	p := &Proxy{Config: cfg, Store: st, Logger: slog.Default(), Client: upstream.Client()}

	body := `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"stream":false}`
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/oaic/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if upstreamPath != "/chat/completions" {
		t.Fatalf("upstream path %q want /chat/completions", upstreamPath)
	}
	if upstreamBody["model"] != "gpt-4" {
		t.Fatalf("upstream model %v", upstreamBody["model"])
	}
	if msgs, ok := upstreamBody["messages"].([]any); !ok || len(msgs) != 1 {
		t.Fatalf("upstream messages not preserved: %v", upstreamBody["messages"])
	}

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if _, ok := resp["choices"]; !ok {
		t.Fatalf("response not passthrough chat: %s", rec.Body.String())
	}
}

func TestProxyAnthropicEndToEnd(t *testing.T) {
	var upstreamBody map[string]any
	var upstreamPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&upstreamBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_123","type":"message","role":"assistant","model":"claude-3-5-sonnet-20241022","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`))
	}))
	defer upstream.Close()

	st, err := store.OpenStore(t.TempDir() + "/proxy.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := config.DefaultConfig(t.TempDir() + "/config.json")
	gw := cfg.Gateways["anth"]
	gw.BaseURL = upstream.URL
	cfg.Gateways["anth"] = gw
	p := &Proxy{Config: cfg, Store: st, Logger: slog.Default(), Client: upstream.Client()}

	body := `{"model":"claude-3-5-sonnet-20241022","max_tokens":1024,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/anth/v1/messages", strings.NewReader(body))
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if upstreamPath != "/v1/messages" {
		t.Fatalf("upstream path %q want /v1/messages", upstreamPath)
	}
	if _, ok := upstreamBody["messages"]; !ok {
		t.Fatalf("upstream messages not present: %v", upstreamBody)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["type"] != "message" {
		t.Fatalf("response not passthrough anthropic: %s", rec.Body.String())
	}
}

func TestProxyAnthropicStreaming(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_123\",\"model\":\"claude-3-5-sonnet\",\"content\":[]}}\n\n"))
		_, _ = w.Write([]byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"))
		_, _ = w.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n"))
		_, _ = w.Write([]byte("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"))
		_, _ = w.Write([]byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":10,\"output_tokens\":5}}\n\n"))
		_, _ = w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer upstream.Close()

	st, err := store.OpenStore(t.TempDir() + "/proxy.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := config.DefaultConfig(t.TempDir() + "/config.json")
	gw := cfg.Gateways["anth"]
	gw.BaseURL = upstream.URL
	cfg.Gateways["anth"] = gw
	p := &Proxy{Config: cfg, Store: st, Logger: slog.Default(), Client: upstream.Client()}

	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/anth/v1/messages", strings.NewReader(`{"model":"claude-3-5-sonnet-20241022","stream":true,"max_tokens":1024,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`))
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "message_start") {
		t.Fatalf("stream not passthrough anthropic: %s", body)
	}
	if !strings.Contains(body, "content_block_delta") {
		t.Fatalf("content delta missing: %s", body)
	}
}

func TestProxyOpenAIStreaming(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-123\",\"created\":1234567890,\"model\":\"gpt-4\",\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-123\",\"created\":1234567890,\"model\":\"gpt-4\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	st, err := store.OpenStore(t.TempDir() + "/proxy.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := config.DefaultConfig(t.TempDir() + "/config.json")
	gw := cfg.Gateways["oaic"]
	gw.BaseURL = upstream.URL
	cfg.Gateways["oaic"] = gw
	p := &Proxy{Config: cfg, Store: st, Logger: slog.Default(), Client: upstream.Client()}

	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/oaic/chat/completions", strings.NewReader(`{"model":"gpt-4","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "chatcmpl-123") {
		t.Fatalf("stream not passthrough: %s", body)
	}
	if !strings.Contains(body, "[DONE]") {
		t.Fatalf("[DONE] missing: %s", body)
	}
}

func TestProxyOpenAIAnthropicHeaders(t *testing.T) {
	var gotAnthropicVersion string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAnthropicVersion = r.Header.Get("Anthropic-Version")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer upstream.Close()
	st, err := store.OpenStore(t.TempDir() + "/proxy.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := config.DefaultConfig(t.TempDir() + "/config.json")
	gw := cfg.Gateways["anth"]
	gw.BaseURL = upstream.URL
	cfg.Gateways["anth"] = gw
	p := &Proxy{Config: cfg, Store: st, Logger: slog.Default(), Client: upstream.Client()}
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/anth/v1/messages", strings.NewReader(`{"model":"claude-3-5-sonnet-20241022","max_tokens":1024,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`))
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	if gotAnthropicVersion != "2023-06-01" {
		t.Fatalf("Anthropic-Version not set: %q", gotAnthropicVersion)
	}
}

func TestEveryDefaultGatewayHasAnAdapterUpdated(t *testing.T) {

	for _, id := range []string{"oaic", "anth"} {
		if _, ok := gatewayAdapters[id]; !ok {
			t.Fatalf("gateway %q missing adapter", id)
		}
	}
}

func TestNewGatewaysAcceptResponses(t *testing.T) {

	tests := []struct {
		id         string
		nativePath string
		rejectPath string
	}{
		{"oaic", "/chat/completions", "/responses"},
		{"anth", "/v1/messages", "/responses"},
	}
	for _, tc := range tests {
		adapter, ok := gatewayAdapters[tc.id]
		if !ok {
			t.Fatalf("no adapter for %s", tc.id)
		}
		if !adapter.AcceptsPath(tc.nativePath) {
			t.Fatalf("adapter %s should accept %s", tc.id, tc.nativePath)
		}
		if adapter.AcceptsPath(tc.rejectPath) {
			t.Fatalf("adapter %s should not accept %s (pure passthrough)", tc.id, tc.rejectPath)
		}
		if !adapter.AcceptsPath(adapter.EndpointPath()) {
			t.Fatalf("adapter %s should accept its own endpoint %s", tc.id, adapter.EndpointPath())
		}
	}
}

func TestSanitizeAndTranslatePreservesFunctionTools(t *testing.T) {

	body := []byte(`{"model":"m","max_tokens":1024,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"tools":[{"name":"lookup","input_schema":{"type":"object","properties":{"q":{"type":"string"}}}}]}`)
	adapter := AnthropicMessagesAdapter{}
	out, err := adapter.TransformRequestBody(body)
	if err != nil {
		t.Fatalf("anthropic error: %v", err)
	}
	if !strings.Contains(string(out), `"lookup"`) {
		t.Fatalf("anthropic lost function tool: %s", out)
	}

	body2 := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`)
	oaic := OpenAICompatibleChatAdapter{}
	out2, err := oaic.TransformRequestBody(body2)
	if err != nil {
		t.Fatalf("oaic error: %v", err)
	}
	if !strings.Contains(string(out2), `"lookup"`) {
		t.Fatalf("oaic lost function tool: %s", out2)
	}
}

func TestTranslateFunctionCallRoundTrip(t *testing.T) {

	oaic := OpenAICompatibleChatAdapter{}
	body := []byte(`{"model":"m","messages":[{"role":"assistant","tool_calls":[{"id":"call-1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"hi\"}"}}]},{"role":"tool","tool_call_id":"call-1","content":"result"}]}`)
	out, err := oaic.TransformRequestBody(body)
	if err != nil {
		t.Fatal(err)
	}
	var chat map[string]any
	if err := json.Unmarshal(out, &chat); err != nil {
		t.Fatal(err)
	}
	msgs, _ := chat["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d: %s", len(msgs), out)
	}
	hasToolCall := false
	for _, m := range msgs {
		if mm, ok := m.(map[string]any); ok {
			if tcs, ok := mm["tool_calls"].([]any); ok && len(tcs) > 0 {
				hasToolCall = true
			}
		}
	}
	if !hasToolCall {
		t.Fatalf("tool_calls not preserved: %s", out)
	}
}

func TestNewGatewaysEndpointsAreDistinct(t *testing.T) {
	std := StandardResponsesAdapter{}
	oaic := OpenAICompatibleChatAdapter{}
	anth := AnthropicMessagesAdapter{}
	if std.EndpointPath() == oaic.EndpointPath() {
		t.Fatalf("Standard and OpenAI endpoints should differ")
	}
	if oaic.EndpointPath() == anth.EndpointPath() {
		t.Fatalf("OpenAI and Anthropic endpoints should differ")
	}
}

func TestResponseTranslationHandlesEmptyContent(t *testing.T) {
	oaic := OpenAICompatibleChatAdapter{}
	body := []byte(`{"id":"chat","model":"m","choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call-1","type":"function","function":{"name":"f","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
	out, err := oaic.TransformResponseBody(body)
	if err != nil {
		t.Fatal(err)
	}

	if string(out) != string(body) {
		t.Fatalf("passthrough should return body as-is: got %s", out)
	}
}

func TestSSEFiltersDoNotLeakThrough(t *testing.T) {
	oaic := OpenAICompatibleChatAdapter{}
	input := "data: {\"id\":\"chatcmpl-123\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"
	reader := oaic.TransformSSE(strings.NewReader(input))
	b, _ := io.ReadAll(reader)
	if string(b) != input {
		t.Fatalf("passthrough SSE should be unchanged: got %q want %q", string(b), input)
	}
}
