package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"grok-gateway-proxy/internal/config"
	"grok-gateway-proxy/internal/store"
)

func TestProxyForwardsSenseNovaChatAndLogs(t *testing.T) {
	var gotAuthorization string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthorization = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":      "chat-1",
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "ok"}}},
			"usage":   map[string]any{"prompt_tokens": 160, "prompt_cache_hit_tokens": 50, "prompt_cache_miss_tokens": 110, "completion_tokens": 4},
		})
	}))
	defer upstream.Close()

	st, err := store.OpenStore(t.TempDir() + "/proxy.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := config.DefaultConfig(t.TempDir() + "/config.json")
	gateway := cfg.Gateways["st"]
	gateway.BaseURL = upstream.URL
	cfg.Gateways["st"] = gateway
	p := &Proxy{Config: cfg, Store: st, Logger: slog.Default(), Client: upstream.Client()}

	requestBody, err := json.Marshal(map[string]any{"model": "sense-model", "stream": false})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/st/chat/completions", strings.NewReader(string(requestBody)))
	req.Header.Set("Authorization", "Bearer test-secret")
	recorder := httptest.NewRecorder()
	p.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "chat-1") {
		t.Fatalf("unexpected proxy response: %d %s", recorder.Code, recorder.Body.String())
	}
	if gotAuthorization != "Bearer test-secret" {
		t.Fatalf("authorization was not forwarded: %q", gotAuthorization)
	}
	logs, err := st.List(context.Background(), store.LogFilter{Limit: 10})
	if err != nil || len(logs) != 1 {
		t.Fatalf("expected one log, got %d, err=%v", len(logs), err)
	}
	if logs[0].Usage.CacheReadTokens != 50 || !logs[0].Usage.CacheSupported {
		t.Fatalf("usage was not logged: %+v", logs[0].Usage)
	}
	detail, err := st.Get(context.Background(), logs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(detail.RequestHeaders, "test-secret") {
		t.Fatal("credential leaked into request header log")
	}
}

func TestProxyCapturesBothSidesAndOverridesUserAgent(t *testing.T) {
	var gotUserAgent string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUserAgent = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Upstream-Trace", "trace-1")
		_, _ = w.Write([]byte(`{"id":"upstream-response","choices":[]}`))
	}))
	defer upstream.Close()

	st, err := store.OpenStore(t.TempDir() + "/proxy.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := config.DefaultConfig(t.TempDir() + "/config.json")
	gateway := cfg.Gateways["st"]
	gateway.BaseURL = upstream.URL
	gateway.UserAgentOverrideEnabled = true
	gateway.UserAgentOverride = "proxy-dev-agent/1"
	cfg.Gateways["st"] = gateway
	p := &Proxy{Config: cfg, Store: st, Logger: slog.Default(), Client: upstream.Client()}

	requestBody := []byte(`{"model":"capture-model","messages":[{"role":"user","content":"hello"}]}`)
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/st/chat/completions?trace=1", strings.NewReader(string(requestBody)))
	req.Header.Set("User-Agent", "client-agent/1")
	recorder := httptest.NewRecorder()
	p.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK || recorder.Body.String() != string(`{"id":"upstream-response","choices":[]}`) {
		t.Fatalf("unexpected proxy response: %d %s", recorder.Code, recorder.Body.String())
	}
	if gotUserAgent != "proxy-dev-agent/1" {
		t.Fatalf("user agent was not overridden: %q", gotUserAgent)
	}
	logs, err := st.List(context.Background(), store.LogFilter{Limit: 10})
	if err != nil || len(logs) != 1 {
		t.Fatalf("expected one log, got %d, err=%v", len(logs), err)
	}
	detail, err := st.Get(context.Background(), logs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.RequestURL != "/st/chat/completions?trace=1" {
		t.Fatalf("request URL was not captured: %q", detail.RequestURL)
	}
	if string(detail.RequestBody) != string(requestBody) || string(detail.UpstreamBody) != string(requestBody) {
		t.Fatal("client and upstream request bodies were not retained")
	}
	if string(detail.UpstreamResponseBody) != recorder.Body.String() || string(detail.ResponseBody) != recorder.Body.String() {
		t.Fatal("upstream and client response bodies were not retained")
	}
	if !strings.Contains(detail.UpstreamHeaders, "proxy-dev-agent/1") {
		t.Fatalf("upstream headers did not contain overridden user agent: %s", detail.UpstreamHeaders)
	}
	if detail.UpstreamResponseStatusCode != http.StatusOK || detail.ClientResponseStatusCode != http.StatusOK {
		t.Fatalf("response statuses were not captured: upstream=%d client=%d", detail.UpstreamResponseStatusCode, detail.ClientResponseStatusCode)
	}
}

func TestProxyTransformsSenseNovaToolCallsForUpstreamAndClient(t *testing.T) {
	var upstreamBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&upstreamBody); err != nil {
			t.Errorf("decode upstream request: %v", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call-1","type":"function_call","function":{"name":"lookup","arguments":"{}"}}]},"finish_reason":""}]}`))
	}))
	defer upstream.Close()

	st, err := store.OpenStore(t.TempDir() + "/proxy.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := config.DefaultConfig(t.TempDir() + "/config.json")
	gateway := cfg.Gateways["st"]
	gateway.BaseURL = upstream.URL
	cfg.Gateways["st"] = gateway
	p := &Proxy{Config: cfg, Store: st, Logger: slog.Default(), Client: upstream.Client()}

	requestBody := []byte(`{"model":"sense-model","messages":[{"role":"assistant","tool_calls":[{"id":"call-1","type":"function","function":{"name":"lookup","arguments":"{}"}}]}]}`)
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/st/chat/completions", strings.NewReader(string(requestBody)))
	recorder := httptest.NewRecorder()
	p.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK || strings.Contains(recorder.Body.String(), `"type":"function_call"`) || !strings.Contains(recorder.Body.String(), `"type":"function"`) || !strings.Contains(recorder.Body.String(), `"finish_reason":null`) {
		t.Fatalf("unexpected client tool call response: %d %s", recorder.Code, recorder.Body.String())
	}
	messages := upstreamBody["messages"].([]any)
	calls := messages[0].(map[string]any)["tool_calls"].([]any)
	if calls[0].(map[string]any)["type"] != "function_call" {
		t.Fatalf("upstream did not receive SenseNova tool call type: %+v", upstreamBody)
	}
	logs, err := st.List(context.Background(), store.LogFilter{Limit: 10})
	if err != nil || len(logs) != 1 {
		t.Fatalf("expected one log, got %d, err=%v", len(logs), err)
	}
	detail, err := st.Get(context.Background(), logs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(detail.RequestBody), `"type":"function"`) || !strings.Contains(string(detail.UpstreamBody), `"type":"function_call"`) {
		t.Fatalf("request comparison did not retain both protocol forms: request=%s upstream=%s", detail.RequestBody, detail.UpstreamBody)
	}
}

func TestProxyRejectsWrongProtocol(t *testing.T) {
	st, err := store.OpenStore(t.TempDir() + "/proxy.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := config.DefaultConfig(t.TempDir() + "/config.json")
	p := &Proxy{Config: cfg, Store: st, Logger: slog.Default(), Client: http.DefaultClient}
	body, err := json.Marshal(map[string]string{"model": "sense-model"})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/ds/chat/completions", "/st/responses", "/std/chat/completions", "/ds/models"} {
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787"+path, strings.NewReader(string(body)))
		recorder := httptest.NewRecorder()
		p.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s: expected 405, got %d", path, recorder.Code)
		}
	}
}

func TestProxyRoutesBothNativeResponseAdapters(t *testing.T) {
	var paths []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":     "response-1",
			"output": []any{map[string]any{"type": "reasoning", "summary": []any{map[string]any{"text": "thinking"}}}},
			"usage":  map[string]any{"input_tokens": 2, "output_tokens": 1},
		})
	}))
	defer upstream.Close()

	st, err := store.OpenStore(t.TempDir() + "/proxy.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := config.DefaultConfig(t.TempDir() + "/config.json")
	for id, gateway := range cfg.Gateways {
		gateway.BaseURL = upstream.URL + "/v1"
		cfg.Gateways[id] = gateway
	}
	p := &Proxy{Config: cfg, Store: st, Logger: slog.Default(), Client: upstream.Client()}

	for _, path := range []string{"/ds/responses", "/std/responses"} {
		body, _ := json.Marshal(map[string]any{"model": "response-model", "input": "hello"})
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787"+path, strings.NewReader(string(body)))
		recorder := httptest.NewRecorder()
		p.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "response-1") {
			t.Fatalf("%s: unexpected response: %d %s", path, recorder.Code, recorder.Body.String())
		}
	}
	if len(paths) != 2 || paths[0] != "/v1/responses" || paths[1] != "/v1/responses" {
		t.Fatalf("unexpected upstream paths: %v", paths)
	}
}

// Upstream error bodies are forwarded verbatim: `error.type` and `error.code`
// are what clients branch on for retry / rate-limit / request-error handling,
// so the proxy must not swap in an envelope of its own.
func TestProxyPassesThroughUpstreamErrors(t *testing.T) {
	const upstreamError = `{"error":{"message":"quota exceeded","type":"rate_limit_error","code":"rate_limit_exceeded"}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(upstreamError))
	}))
	defer upstream.Close()

	st, err := store.OpenStore(t.TempDir() + "/proxy.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := config.DefaultConfig(t.TempDir() + "/config.json")
	gateway := cfg.Gateways["st"]
	gateway.BaseURL = upstream.URL
	cfg.Gateways["st"] = gateway
	p := &Proxy{Config: cfg, Store: st, Logger: slog.Default(), Client: upstream.Client()}
	body := []byte(`{"model":"sense-model"}`)
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/st/chat/completions", strings.NewReader(string(body)))
	recorder := httptest.NewRecorder()
	p.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("unexpected status: %d %s", recorder.Code, recorder.Body.String())
	}
	if recorder.Body.String() != upstreamError {
		t.Fatalf("upstream error body was rewritten: %s", recorder.Body.String())
	}
	if got := recorder.Header().Get("Retry-After"); got != "30" {
		t.Fatalf("Retry-After was dropped: %q", got)
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("upstream Content-Type was rewritten: %q", got)
	}
	logs, err := st.List(context.Background(), store.LogFilter{Limit: 10})
	if err != nil || len(logs) != 1 || logs[0].Success {
		t.Fatalf("unexpected error log: %+v, err=%v", logs, err)
	}
}

func TestJoinUpstreamURL(t *testing.T) {
	got, err := joinUpstreamURL("https://example.test/v1", "/responses", "x=1")
	if err != nil || got != "https://example.test/v1/responses?x=1" {
		t.Fatalf("unexpected URL: %s, err=%v", got, err)
	}
}

// A response larger than the capture cap must still be forwarded to the
// client in full, while the log keeps only the first cap bytes and flags the
// log with response_truncated.
func TestProxyCapsResponseBodyCapture(t *testing.T) {
	const capSize = 1024
	bigBody := bytes.Repeat([]byte("x"), 4*capSize)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(bigBody)
	}))
	defer upstream.Close()

	st, err := store.OpenStore(t.TempDir() + "/proxy.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := config.DefaultConfig(t.TempDir() + "/config.json")
	gateway := cfg.Gateways["st"]
	gateway.BaseURL = upstream.URL
	cfg.Gateways["st"] = gateway
	p := &Proxy{Config: cfg, Store: st, Logger: slog.Default(), Client: upstream.Client(), ResponseBodySize: capSize}

	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/st/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	recorder := httptest.NewRecorder()
	p.ServeHTTP(recorder, req)

	// The client still receives the full body.
	if recorder.Code != http.StatusOK || recorder.Body.Len() != len(bigBody) {
		t.Fatalf("client did not receive full body: status=%d len=%d", recorder.Code, recorder.Body.Len())
	}

	logs, err := st.List(context.Background(), store.LogFilter{Limit: 10})
	if err != nil || len(logs) != 1 {
		t.Fatalf("expected one log, got %d, err=%v", len(logs), err)
	}
	detail, err := st.Get(context.Background(), logs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.ResponseBody) != capSize || len(detail.UpstreamResponseBody) != capSize {
		t.Fatalf("expected capped bodies of %d bytes, got response=%d upstream=%d", capSize, len(detail.ResponseBody), len(detail.UpstreamResponseBody))
	}
	if !detail.ResponseTruncated {
		t.Fatal("expected response_truncated flag on the log")
	}
}

// An SSE stream larger than the capture cap is forwarded in full to the
// client; only the logged raw/transformed captures are capped and flagged.
func TestProxyCapsSSEResponseBodyCapture(t *testing.T) {
	const capSize = 1024
	var stream bytes.Buffer
	for i := 0; i < 40; i++ {
		stream.WriteString("data: {\"type\":\"response.output_text.delta\",\"sequence_number\":" + strconv.Itoa(i) + ",\"delta\":\"" + strings.Repeat("x", 80) + "\"}\n\n")
	}
	stream.WriteString("data: [DONE]\n\n")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(stream.Bytes())
	}))
	defer upstream.Close()

	st, err := store.OpenStore(t.TempDir() + "/proxy.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := config.DefaultConfig(t.TempDir() + "/config.json")
	gateway := cfg.Gateways["std"]
	gateway.BaseURL = upstream.URL
	cfg.Gateways["std"] = gateway
	p := &Proxy{Config: cfg, Store: st, Logger: slog.Default(), Client: upstream.Client(), ResponseBodySize: capSize}

	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/std/responses", strings.NewReader(`{"model":"m","stream":true,"input":"hi"}`))
	recorder := httptest.NewRecorder()
	p.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK || recorder.Body.Len() != stream.Len() {
		t.Fatalf("client did not receive full stream: status=%d len=%d want=%d", recorder.Code, recorder.Body.Len(), stream.Len())
	}

	logs, err := st.List(context.Background(), store.LogFilter{Limit: 10})
	if err != nil || len(logs) != 1 {
		t.Fatalf("expected one log, got %d, err=%v", len(logs), err)
	}
	detail, err := st.Get(context.Background(), logs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.ResponseBody) != capSize || len(detail.UpstreamResponseBody) != capSize {
		t.Fatalf("expected capped captures of %d bytes, got response=%d upstream=%d", capSize, len(detail.ResponseBody), len(detail.UpstreamResponseBody))
	}
	if !detail.ResponseTruncated {
		t.Fatal("expected response_truncated flag on the log")
	}
}

// Usage must be metered from the live stream, not from the capped capture: the
// terminal usage-bearing event lands outside the capture window whenever the
// stream is longer than the cap, and re-scanning the capture would then report
// zero tokens for a response that was billed in full.
func TestProxyStreamUsageSurvivesCaptureTruncation(t *testing.T) {
	const capSize = 1024
	var stream bytes.Buffer
	for i := 0; i < 40; i++ {
		stream.WriteString("data: {\"type\":\"response.output_text.delta\",\"delta\":\"" + strings.Repeat("x", 80) + "\"}\n\n")
	}
	stream.WriteString(`data: {"type":"response.completed","response":{"usage":{"input_tokens":111,"output_tokens":222,"output_tokens_details":{"reasoning_tokens":33}}}}` + "\n\n")
	if stream.Len() <= capSize {
		t.Fatalf("stream (%d bytes) must exceed the cap to exercise truncation", stream.Len())
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(stream.Bytes())
	}))
	defer upstream.Close()

	st, err := store.OpenStore(t.TempDir() + "/proxy.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := config.DefaultConfig(t.TempDir() + "/config.json")
	gateway := cfg.Gateways["std"]
	gateway.BaseURL = upstream.URL
	cfg.Gateways["std"] = gateway
	p := &Proxy{Config: cfg, Store: st, Logger: slog.Default(), Client: upstream.Client(), ResponseBodySize: capSize}

	recorder := httptest.NewRecorder()
	p.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/std/responses", strings.NewReader(`{"model":"m","stream":true,"input":"hi"}`)))

	logs, err := st.List(context.Background(), store.LogFilter{Limit: 10})
	if err != nil || len(logs) != 1 {
		t.Fatalf("expected one log, got %d, err=%v", len(logs), err)
	}
	detail, err := st.Get(context.Background(), logs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if !detail.ResponseTruncated {
		t.Fatal("expected the capture to be truncated, otherwise this test proves nothing")
	}
	if detail.Usage.InputTokens != 111 || detail.Usage.OutputTokens != 222 || detail.Usage.ReasoningTokens != 33 {
		t.Fatalf("usage lost to capture truncation: in=%d out=%d reasoning=%d, want 111/222/33",
			detail.Usage.InputTokens, detail.Usage.OutputTokens, detail.Usage.ReasoningTokens)
	}
}

// A response within the cap is stored in full and not flagged.
func TestProxyResponseWithinCapIsNotFlagged(t *testing.T) {
	const capSize = 4096
	body := []byte(`{"id":"ok","choices":[]}`)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer upstream.Close()

	st, err := store.OpenStore(t.TempDir() + "/proxy.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := config.DefaultConfig(t.TempDir() + "/config.json")
	gateway := cfg.Gateways["st"]
	gateway.BaseURL = upstream.URL
	cfg.Gateways["st"] = gateway
	p := &Proxy{Config: cfg, Store: st, Logger: slog.Default(), Client: upstream.Client(), ResponseBodySize: capSize}

	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/st/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	recorder := httptest.NewRecorder()
	p.ServeHTTP(recorder, req)

	logs, err := st.List(context.Background(), store.LogFilter{Limit: 10})
	if err != nil || len(logs) != 1 {
		t.Fatalf("expected one log, got %d, err=%v", len(logs), err)
	}
	detail, err := st.Get(context.Background(), logs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.ResponseTruncated {
		t.Fatal("response within cap must not be flagged truncated")
	}
	if string(detail.ResponseBody) != string(body) {
		t.Fatalf("response body mismatch: %q", detail.ResponseBody)
	}
}

// A slow upstream must be cut off by the per-request timeout and reported as
// a 504 gateway timeout instead of hanging the client indefinitely.
func TestProxyEnforcesUpstreamTimeout(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(5 * time.Second):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"late"}`))
		case <-r.Context().Done():
			return
		}
	}))
	defer upstream.Close()

	st, err := store.OpenStore(t.TempDir() + "/proxy.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cfg := config.DefaultConfig(t.TempDir() + "/config.json")
	cfg.UpstreamTimeout = 200 * time.Millisecond
	gateway := cfg.Gateways["st"]
	gateway.BaseURL = upstream.URL
	cfg.Gateways["st"] = gateway
	p := &Proxy{Config: cfg, Store: st, Logger: slog.Default(), Client: upstream.Client()}

	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/st/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("X-Proxy-Timeout", "300ms")
	recorder := httptest.NewRecorder()
	start := time.Now()
	p.ServeHTTP(recorder, req)
	elapsed := time.Since(start)

	if recorder.Code != http.StatusGatewayTimeout {
		t.Fatalf("expected 504 gateway timeout, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if elapsed > 2*time.Second {
		t.Fatalf("timeout was not enforced promptly: %v", elapsed)
	}
	logs, err := st.List(context.Background(), store.LogFilter{Limit: 1})
	if err != nil || len(logs) != 1 {
		t.Fatalf("expected one log, got %d, err=%v", len(logs), err)
	}
	if logs[0].StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("expected 504 in log, got %d", logs[0].StatusCode)
	}
}

// A streaming request must NOT be cut off by the total upstream timeout: the
// client applies its own 300s idle timeout, so an active stream that outlives
// the configured timeout must be delivered in full.
func TestProxyStreamingSurvivesUpstreamTimeout(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		for i := 0; i < 8; i++ {
			select {
			case <-time.After(120 * time.Millisecond):
			case <-r.Context().Done():
				return
			}
			chunk := "data: {\"type\":\"response.output_text.delta\",\"sequence_number\":" + strconv.Itoa(i) + ",\"delta\":\"x\"}\n\n"
			if _, err := w.Write([]byte(chunk)); err != nil {
				return
			}
			flusher.Flush()
		}
	}))
	defer upstream.Close()

	st, err := store.OpenStore(t.TempDir() + "/proxy.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := config.DefaultConfig(t.TempDir() + "/config.json")
	cfg.UpstreamTimeout = 300 * time.Millisecond
	gateway := cfg.Gateways["std"]
	gateway.BaseURL = upstream.URL
	cfg.Gateways["std"] = gateway
	p := &Proxy{Config: cfg, Store: st, Logger: slog.Default(), Client: upstream.Client()}

	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/std/responses",
		strings.NewReader(`{"model":"m","stream":true,"input":"hi"}`))
	recorder := httptest.NewRecorder()
	p.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	for i := 0; i < 8; i++ {
		if !strings.Contains(recorder.Body.String(), `"sequence_number":`+strconv.Itoa(i)) {
			t.Fatalf("stream was truncated before chunk %d: %s", i, recorder.Body.String())
		}
	}
	logs, err := st.List(context.Background(), store.LogFilter{Limit: 1})
	if err != nil || len(logs) != 1 {
		t.Fatalf("expected one log, got %d, err=%v", len(logs), err)
	}
	if !logs[0].Success || logs[0].StatusCode != http.StatusOK {
		t.Fatalf("long stream must be logged as success: %+v", logs[0])
	}
}

// Responses requests must reach the upstream conforming to the standard
// protocol: xAI-only extensions are stripped while the rest of the body is
// preserved, and the log keeps both sides for comparison.
func TestProxySanitizesResponsesRequestToUpstream(t *testing.T) {
	var upstreamBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&upstreamBody); err != nil {
			t.Errorf("decode upstream request: %v", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"sanitized-response","created_at":1,"object":"response","status":"completed","model":"m","output":[]}`))
	}))
	defer upstream.Close()

	st, err := store.OpenStore(t.TempDir() + "/proxy.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := config.DefaultConfig(t.TempDir() + "/config.json")
	gateway := cfg.Gateways["std"]
	gateway.BaseURL = upstream.URL
	cfg.Gateways["std"] = gateway
	p := &Proxy{Config: cfg, Store: st, Logger: slog.Default(), Client: upstream.Client()}

	requestBody := []byte(`{"model":"deepseek-v4-flash","stream":true,"stream_tool_calls":true,"tools":[{"type":"x_search"},{"type":"function","name":"lookup","parameters":{}}],"input":[]}`)
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/std/responses", strings.NewReader(string(requestBody)))
	recorder := httptest.NewRecorder()
	p.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "sanitized-response") {
		t.Fatalf("unexpected response: %d %s", recorder.Code, recorder.Body.String())
	}
	if _, exists := upstreamBody["stream_tool_calls"]; exists {
		t.Fatalf("non-standard stream_tool_calls reached upstream: %+v", upstreamBody)
	}
	tools, _ := upstreamBody["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["type"] != "function" {
		t.Fatalf("non-standard x_search tool reached upstream: %+v", upstreamBody)
	}
	if upstreamBody["model"] != "deepseek-v4-flash" {
		t.Fatalf("upstream model changed unexpectedly: %+v", upstreamBody)
	}

	logs, err := st.List(context.Background(), store.LogFilter{Limit: 1})
	if err != nil || len(logs) != 1 {
		t.Fatalf("expected one request log, got %d, err=%v", len(logs), err)
	}
	detail, err := st.Get(context.Background(), logs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(detail.RequestBody), `"stream_tool_calls":true`) || strings.Contains(string(detail.UpstreamBody), `"stream_tool_calls"`) {
		t.Fatalf("request comparison did not preserve the sanitization: request=%s upstream=%s", detail.RequestBody, detail.UpstreamBody)
	}
}

// End-to-end check with the exact request shape Grok Build sends and the
// event stream its strict parser accepts: xAI-only request extensions are
// stripped before the upstream sees them, and ping / unknown event types are
// dropped from the reply instead of failing the whole client stream.
func TestProxyGrokBuildResponsesEndToEnd(t *testing.T) {
	var upstreamBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&upstreamBody)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":1,\"status\":\"in_progress\",\"model\":\"m\",\"output\":[]}}\n\n")
		_, _ = fmt.Fprint(w, "data: ping\n\n")
		_, _ = fmt.Fprint(w, "event: response.reasoning_text.delta\ndata: {\"type\":\"response.reasoning_text.delta\",\"sequence_number\":1,\"item_id\":\"rs_1\",\"output_index\":0,\"content_index\":0,\"delta\":\"thinking\"}\n\n")
		_, _ = fmt.Fprint(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"sequence_number\":2,\"item_id\":\"msg_1\",\"output_index\":1,\"content_index\":0,\"delta\":\"hello\"}\n\n")
		_, _ = fmt.Fprint(w, "event: response.apply_patch_call_operation_diff.delta\ndata: {\"type\":\"response.apply_patch_call_operation_diff.delta\",\"sequence_number\":3,\"delta\":\"x\"}\n\n")
		_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"sequence_number\":4,\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":1,\"status\":\"completed\",\"model\":\"m\",\"output\":[],\"usage\":{\"input_tokens\":2,\"output_tokens\":1,\"total_tokens\":3,\"input_tokens_details\":{\"cached_tokens\":0},\"output_tokens_details\":{\"reasoning_tokens\":0}}}}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	st, err := store.OpenStore(t.TempDir() + "/proxy.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := config.DefaultConfig(t.TempDir() + "/config.json")
	gateway := cfg.Gateways["std"]
	gateway.BaseURL = upstream.URL
	cfg.Gateways["std"] = gateway
	p := &Proxy{Config: cfg, Store: st, Logger: slog.Default(), Client: upstream.Client()}

	// The xAI-flavored body: backend-only option, raw hosted tools, xAI-only
	// include, reasoning history with reasoning_text content parts.
	requestBody := `{"model":"some-model","stream":true,"stream_tool_calls":true,"store":false,` +
		`"include":["reasoning.encrypted_content","no_inline_citations"],` +
		`"tools":[{"type":"x_search","from_date":"2026-01-01"},{"type":"web_search","filters":{"excluded_domains":["a.example"]}},` +
		`{"type":"function","name":"lookup","description":"find","parameters":{"type":"object","properties":{}}}],` +
		`"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},` +
		`{"type":"reasoning","id":"rs_1","content":[{"type":"reasoning_text","text":"old"}],"summary":[]}]}`
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/std/responses", strings.NewReader(requestBody))
	recorder := httptest.NewRecorder()
	p.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d %s", recorder.Code, recorder.Body.String())
	}

	// Request direction: standard vocabulary only.
	if _, exists := upstreamBody["stream_tool_calls"]; exists {
		t.Fatalf("stream_tool_calls reached upstream: %+v", upstreamBody)
	}
	// x_search is dropped; the excluded-domains web_search survives with its
	// filter renamed to the standard blocked_domains.
	tools := upstreamBody["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("non-standard tools reached upstream: %+v", tools)
	}
	webSearch := tools[0].(map[string]any)
	if webSearch["type"] != "web_search" {
		t.Fatalf("web_search tool was dropped instead of renamed: %+v", tools)
	}
	filters := webSearch["filters"].(map[string]any)
	if _, exists := filters["excluded_domains"]; exists {
		t.Fatalf("excluded_domains reached upstream: %+v", filters)
	}
	blocked := filters["blocked_domains"].([]any)
	if len(blocked) != 1 || blocked[0] != "a.example" {
		t.Fatalf("blocklist was not preserved as blocked_domains: %+v", filters)
	}
	if tools[1].(map[string]any)["type"] != "function" {
		t.Fatalf("function tool was dropped: %+v", tools)
	}
	include := upstreamBody["include"].([]any)
	if len(include) != 1 || include[0] != "reasoning.encrypted_content" {
		t.Fatalf("non-standard include values reached upstream: %+v", include)
	}
	input := upstreamBody["input"].([]any)
	reasoning := input[1].(map[string]any)
	content := reasoning["content"].([]any)
	if content[0].(map[string]any)["type"] != "reasoning_text" {
		t.Fatalf("reasoning input item was not preserved: %+v", reasoning)
	}

	// Response direction: client-parseable events only.
	client := recorder.Body.String()
	for _, forbidden := range []string{"ping", "apply_patch"} {
		if strings.Contains(client, forbidden) {
			t.Fatalf("client stream contains %q event: %s", forbidden, client)
		}
	}
	for _, required := range []string{"response.created", "response.reasoning_text.delta", "response.output_text.delta", "response.completed", "data: [DONE]"} {
		if !strings.Contains(client, required) {
			t.Fatalf("client stream lost %q: %s", required, client)
		}
	}
}

// DeepSeek 网关全链路：grok build 形状的请求清洗为 DeepSeek 接受的标准
// Responses 请求（include 移除、reasoning 明文 content 保留回传），上游
// 事件流无 data: [DONE] 时客户端按 EOF 完整收尾。
func TestProxyDeepSeekResponsesEndToEnd(t *testing.T) {
	var upstreamBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&upstreamBody)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":1,\"status\":\"in_progress\",\"model\":\"deepseek-v4-flash\",\"output\":[]}}\n\n")
		_, _ = fmt.Fprint(w, "event: response.reasoning_text.delta\ndata: {\"type\":\"response.reasoning_text.delta\",\"sequence_number\":1,\"item_id\":\"rs_1\",\"output_index\":0,\"content_index\":0,\"delta\":\"thinking\"}\n\n")
		_, _ = fmt.Fprint(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"sequence_number\":2,\"item_id\":\"msg_1\",\"output_index\":1,\"content_index\":0,\"delta\":\"hello\"}\n\n")
		_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"sequence_number\":3,\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":1,\"status\":\"completed\",\"model\":\"deepseek-v4-flash\",\"output\":[],\"usage\":{\"input_tokens\":3,\"output_tokens\":2,\"total_tokens\":5,\"input_tokens_details\":{\"cached_tokens\":0},\"output_tokens_details\":{\"reasoning_tokens\":1}}}}\n\n")
	}))
	defer upstream.Close()

	st, err := store.OpenStore(t.TempDir() + "/proxy.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := config.DefaultConfig(t.TempDir() + "/config.json")
	gateway := cfg.Gateways["ds"]
	gateway.BaseURL = upstream.URL
	cfg.Gateways["ds"] = gateway
	p := &Proxy{Config: cfg, Store: st, Logger: slog.Default(), Client: upstream.Client()}

	requestBody := `{"model":"deepseek-v4-flash","stream":true,"stream_tool_calls":true,` +
		`"include":["reasoning.encrypted_content"],` +
		`"reasoning":{"effort":"xhigh"},` +
		`"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],` +
		`"input":[{"type":"reasoning","id":"rs_1","summary":[],"content":[{"type":"reasoning_text","text":"prior thoughts"}]},{"type":"message","role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/ds/responses", strings.NewReader(requestBody))
	recorder := httptest.NewRecorder()
	p.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d %s", recorder.Code, recorder.Body.String())
	}
	if _, exists := upstreamBody["include"]; exists {
		t.Fatalf("include reached DeepSeek: %+v", upstreamBody)
	}
	if upstreamBody["reasoning"].(map[string]any)["effort"] != "xhigh" {
		t.Fatalf("reasoning.effort was rewritten: %+v", upstreamBody["reasoning"])
	}
	input := upstreamBody["input"].([]any)
	reasoning := input[0].(map[string]any)
	if _, hasSummary := reasoning["summary"]; hasSummary {
		t.Fatalf("reasoning summary reached DeepSeek: %+v", reasoning)
	}
	content := reasoning["content"].([]any)
	if content[0].(map[string]any)["text"] != "prior thoughts" {
		t.Fatalf("reasoning content was lost: %+v", reasoning)
	}

	client := recorder.Body.String()
	for _, required := range []string{"response.created", "response.reasoning_text.delta", "response.output_text.delta", "response.completed"} {
		if !strings.Contains(client, required) {
			t.Fatalf("client stream lost %q: %s", required, client)
		}
	}
	if strings.Contains(client, "response.failed") || !strings.Contains(client, "response.completed") {
		t.Fatalf("stream was terminated prematurely: %s", client)
	}
}

// Base URL 留空的网关在请求时必须返回明确的 503，而不是模糊的上游错误。
func TestProxyUnconfiguredBaseURLReturns503(t *testing.T) {
	st, err := store.OpenStore(t.TempDir() + "/proxy.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := config.DefaultConfig(t.TempDir() + "/config.json")
	p := &Proxy{Config: cfg, Store: st, Logger: slog.Default()}

	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/ds/responses", strings.NewReader(`{"model":"deepseek-v4-flash","input":"hi"}`))
	recorder := httptest.NewRecorder()
	p.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusServiceUnavailable || !strings.Contains(recorder.Body.String(), "base URL") {
		t.Fatalf("expected 503 with a clear message, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

// End-to-end proof that a gateway's configured ForwardHeaders reaches the
// upstream: the units in headers_test.go only cover copyForwardHeaders, not
// that buildUpstreamRequest actually feeds the configured list into it.
func TestProxyForwardsConfiguredGrokHeaders(t *testing.T) {
	var gotDoomLoop, gotUserID, gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotDoomLoop = r.Header.Get("X-Grok-Doom-Loop-Check")
		gotUserID = r.Header.Get("X-Grok-User-Id")
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp","object":"response","model":"m","status":"completed","output":[]}`))
	}))
	defer upstream.Close()

	st, err := store.OpenStore(t.TempDir() + "/proxy.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := config.DefaultConfig(t.TempDir() + "/config.json")
	gateway := cfg.Gateways["std"]
	gateway.BaseURL = upstream.URL
	gateway.ForwardHeaders = []string{"Authorization", "Content-Type", "X-Grok-Doom-Loop-Check"}
	cfg.Gateways["std"] = gateway
	p := &Proxy{Config: cfg, Store: st, Logger: slog.Default(), Client: upstream.Client()}

	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/std/responses", strings.NewReader(`{"model":"m","input":"hi"}`))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("X-Grok-Doom-Loop-Check", "64")
	req.Header.Set("X-Grok-User-Id", "u-1")
	p.ServeHTTP(httptest.NewRecorder(), req)

	if gotDoomLoop != "64" {
		t.Fatalf("configured x-grok-doom-loop-check did not reach upstream: %q", gotDoomLoop)
	}
	if gotUserID != "" {
		t.Fatalf("unconfigured x-grok-user-id reached upstream: %q", gotUserID)
	}
	if gotAuth != "Bearer secret" {
		t.Fatalf("Authorization did not reach upstream: %q", gotAuth)
	}
}

// Without configuration the same request must leak nothing to the upstream.
func TestProxyDropsGrokHeadersWithoutConfiguration(t *testing.T) {
	var gotDoomLoop, gotConvID string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotDoomLoop = r.Header.Get("X-Grok-Doom-Loop-Check")
		gotConvID = r.Header.Get("X-Grok-Conv-Id")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp","object":"response","model":"m","status":"completed","output":[]}`))
	}))
	defer upstream.Close()

	st, err := store.OpenStore(t.TempDir() + "/proxy.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := config.DefaultConfig(t.TempDir() + "/config.json")
	gateway := cfg.Gateways["std"]
	gateway.BaseURL = upstream.URL
	cfg.Gateways["std"] = gateway
	p := &Proxy{Config: cfg, Store: st, Logger: slog.Default(), Client: upstream.Client()}

	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/std/responses", strings.NewReader(`{"model":"m","input":"hi"}`))
	req.Header.Set("X-Grok-Doom-Loop-Check", "64")
	req.Header.Set("X-Grok-Conv-Id", "c-1")
	p.ServeHTTP(httptest.NewRecorder(), req)

	if gotDoomLoop != "" || gotConvID != "" {
		t.Fatalf("default config leaked grok headers upstream: doom_loop=%q conv=%q", gotDoomLoop, gotConvID)
	}
}

// An upstream that hangs up mid-stream answered 200 but never delivered a
// complete response, so it must not be logged as a success: the dashboard's
// success rate and token totals would otherwise count half an answer as a
// whole one.
func TestProxyRecordsMidStreamFailureAsFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"half\"}\n\n")); err != nil {
			return
		}
		w.(http.Flusher).Flush()
		// Hang up mid-stream instead of terminating the event stream.
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		conn.Close()
	}))
	defer upstream.Close()

	st, err := store.OpenStore(t.TempDir() + "/proxy.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := config.DefaultConfig(t.TempDir() + "/config.json")
	gateway := cfg.Gateways["std"]
	gateway.BaseURL = upstream.URL
	cfg.Gateways["std"] = gateway
	p := &Proxy{Config: cfg, Store: st, Logger: slog.Default(), Client: upstream.Client()}

	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/std/responses",
		strings.NewReader(`{"model":"m","stream":true,"input":"hi"}`))
	p.ServeHTTP(httptest.NewRecorder(), req)

	logs, err := st.List(context.Background(), store.LogFilter{Limit: 1})
	if err != nil || len(logs) != 1 {
		t.Fatalf("expected one log, got %d, err=%v", len(logs), err)
	}
	if logs[0].Success {
		t.Fatalf("stream broken mid-flight must not be logged as success: %+v", logs[0])
	}
	if logs[0].Error == "" {
		t.Fatal("expected the copy error to be recorded on the log entry")
	}
}

// config documents body_capture_limit_kb: 0 as "capture everything", so it
// must not silently fall back to the default cap. It is still bounded by
// maxCapturedBodySize — an opt-out must not become an unbounded write to
// SQLite — but a 300KB body sits far below that ceiling and is stored whole.
func TestProxyCaptureLimitZeroDisablesTruncation(t *testing.T) {
	body := []byte(`{"id":"ok","choices":[{"text":"` + strings.Repeat("x", 300*1024) + `"}]}`)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer upstream.Close()

	for _, test := range []struct {
		name          string
		limitKB       int
		wantTruncated bool
	}{
		{name: "zero captures everything below the ceiling", limitKB: 0, wantTruncated: false},
		{name: "one KB truncates", limitKB: 1, wantTruncated: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			st, err := store.OpenStore(t.TempDir() + "/proxy.db")
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			cfg := config.DefaultConfig(t.TempDir() + "/config.json")
			cfg.BodyCaptureLimitKB = test.limitKB
			gateway := cfg.Gateways["st"]
			gateway.BaseURL = upstream.URL
			cfg.Gateways["st"] = gateway
			p := &Proxy{Config: cfg, Store: st, Logger: slog.Default(), Client: upstream.Client()}

			req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/st/chat/completions",
				strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
			p.ServeHTTP(httptest.NewRecorder(), req)

			logs, err := st.List(context.Background(), store.LogFilter{Limit: 1})
			if err != nil || len(logs) != 1 {
				t.Fatalf("expected one log, got %d, err=%v", len(logs), err)
			}
			detail, err := st.Get(context.Background(), logs[0].ID)
			if err != nil {
				t.Fatal(err)
			}
			if detail.ResponseTruncated != test.wantTruncated {
				t.Fatalf("ResponseTruncated = %v, want %v (%d bytes stored of %d)",
					detail.ResponseTruncated, test.wantTruncated, len(detail.ResponseBody), len(body))
			}
			if !test.wantTruncated && string(detail.ResponseBody) != string(body) {
				t.Fatalf("body was altered: %d bytes, want %d", len(detail.ResponseBody), len(body))
			}
		})
	}
}

// Upstream error bodies take the capBody path instead of the cappedBuffer one,
// so they must honour "0 = capture everything" the same way a 200 body does:
// comparing against a zero limit would flag every error response as truncated.
// A 300KB error body is far below maxCapturedBodySize, so it is stored whole.
func TestProxyCaptureLimitZeroLeavesErrorBodyUntruncated(t *testing.T) {
	errorBody := []byte(`{"error":{"type":"invalid_request_error","message":"` + strings.Repeat("y", 300*1024) + `"}}`)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write(errorBody)
	}))
	defer upstream.Close()

	st, err := store.OpenStore(t.TempDir() + "/proxy.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := config.DefaultConfig(t.TempDir() + "/config.json")
	cfg.BodyCaptureLimitKB = 0
	gateway := cfg.Gateways["st"]
	gateway.BaseURL = upstream.URL
	cfg.Gateways["st"] = gateway
	p := &Proxy{Config: cfg, Store: st, Logger: slog.Default(), Client: upstream.Client()}

	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/st/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	p.ServeHTTP(httptest.NewRecorder(), req)

	logs, err := st.List(context.Background(), store.LogFilter{Limit: 1})
	if err != nil || len(logs) != 1 {
		t.Fatalf("expected one log, got %d, err=%v", len(logs), err)
	}
	detail, err := st.Get(context.Background(), logs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.ResponseTruncated {
		t.Fatal("error body was flagged truncated although capture is unlimited")
	}
	if string(detail.ResponseBody) != string(errorBody) {
		t.Fatalf("error body was altered: %d bytes, want %d", len(detail.ResponseBody), len(errorBody))
	}
}

// With a cap in effect, an oversized upstream error body is still flagged, and
// the client still receives it in full.
func TestProxyCapsErrorResponseBodyCapture(t *testing.T) {
	const capSize = 1024
	errorBody := bytes.Repeat([]byte("z"), 4*capSize)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write(errorBody)
	}))
	defer upstream.Close()

	st, err := store.OpenStore(t.TempDir() + "/proxy.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := config.DefaultConfig(t.TempDir() + "/config.json")
	gateway := cfg.Gateways["st"]
	gateway.BaseURL = upstream.URL
	cfg.Gateways["st"] = gateway
	p := &Proxy{Config: cfg, Store: st, Logger: slog.Default(), Client: upstream.Client(), ResponseBodySize: capSize}

	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/st/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	recorder := httptest.NewRecorder()
	p.ServeHTTP(recorder, req)

	if recorder.Body.Len() != len(errorBody) {
		t.Fatalf("client did not receive the full error body: %d bytes, want %d", recorder.Body.Len(), len(errorBody))
	}

	logs, err := st.List(context.Background(), store.LogFilter{Limit: 1})
	if err != nil || len(logs) != 1 {
		t.Fatalf("expected one log, got %d, err=%v", len(logs), err)
	}
	detail, err := st.Get(context.Background(), logs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.ResponseBody) != capSize {
		t.Fatalf("expected capped error body of %d bytes, got %d", capSize, len(detail.ResponseBody))
	}
	if !detail.ResponseTruncated {
		t.Fatal("expected response_truncated flag on the log")
	}
}

// A configured limit of zero must still be bounded: "capture everything" means
// up to maxCapturedBodySize, not unbounded. The ceiling mirrors the request
// side (maxRequestBodySize) so both directions are capped the same way, and it
// has to be large enough that no ordinary payload is truncated by it.
func TestResponseBodyLimitZeroFallsBackToSafetyCeiling(t *testing.T) {
	st, err := store.OpenStore(t.TempDir() + "/proxy.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	for _, limitKB := range []int{0} {
		cfg := config.DefaultConfig(t.TempDir() + "/config.json")
		cfg.BodyCaptureLimitKB = limitKB
		p := &Proxy{Config: cfg, Store: st}
		if got := p.responseBodyLimit(); got != maxCapturedBodySize {
			t.Fatalf("responseBodyLimit() with body_capture_limit_kb=%d = %d, want %d",
				limitKB, got, maxCapturedBodySize)
		}
	}

	// A non-zero configured limit is honoured verbatim and is not raised to
	// the ceiling.
	cfg := config.DefaultConfig(t.TempDir() + "/config.json")
	cfg.BodyCaptureLimitKB = 8
	p := &Proxy{Config: cfg, Store: st}
	if got := p.responseBodyLimit(); got != 8<<10 {
		t.Fatalf("responseBodyLimit() = %d, want %d", got, 8<<10)
	}

	// The ceiling must not be smaller than what callers already rely on.
	if maxCapturedBodySize < defaultResponseBodySize {
		t.Fatalf("maxCapturedBodySize (%d) is below the default cap (%d)", maxCapturedBodySize, defaultResponseBodySize)
	}
}

// capBody is the shared truncation helper for the request, upstream and error
// bodies; a zero limit has to mean "keep everything" at this level too, with
// the ceiling applied by the caller.
func TestCapBodyHonoursLimit(t *testing.T) {
	body := []byte(strings.Repeat("z", 4096))
	if got := capBody(body, 0); len(got) != len(body) {
		t.Fatalf("capBody with limit 0 returned %d bytes, want %d", len(got), len(body))
	}
	if got := capBody(body, 1024); len(got) != 1024 {
		t.Fatalf("capBody with limit 1024 returned %d bytes, want 1024", len(got))
	}
	if got := capBody(body, 1<<20); len(got) != len(body) {
		t.Fatalf("capBody with an oversized limit truncated: %d bytes", len(got))
	}
}

// gatewayForPath must pick the most specific prefix, not whichever gateway
// the map happens to yield first. Prefixes are not required to be disjoint,
// and Go randomizes map iteration order, so a first-match-wins scan would
// route the same path to different gateways across restarts.
func TestGatewayForPathPrefersLongestPrefix(t *testing.T) {
	cfg := config.DefaultConfig(t.TempDir() + "/config.json")
	// Deliberately overlapping prefixes: /std is nested inside /s.
	cfg.Gateways = map[string]config.GatewayConfig{
		"s":   {ID: "s", Prefix: "/s", Protocol: config.ProtocolChat, Enabled: true},
		"std": {ID: "std", Prefix: "/std", Protocol: config.ProtocolResponses, Enabled: true},
	}
	p := &Proxy{Config: cfg}

	tests := []struct {
		path     string
		gateway  string
		subpath  string
		resolved bool
	}{
		{path: "/std/chat/completions", gateway: "std", subpath: "/chat/completions", resolved: true},
		{path: "/std", gateway: "std", subpath: "", resolved: true},
		{path: "/s/chat/completions", gateway: "s", subpath: "/chat/completions", resolved: true},
		// Path-component aware: /static must not match /s.
		{path: "/static/app.js", resolved: false},
		{path: "/status", resolved: false},
	}
	// Repeat because the failure mode is order-dependent and only shows up
	// for some map iteration orders.
	for attempt := 0; attempt < 50; attempt++ {
		for _, test := range tests {
			gateway, subpath, ok := p.gatewayForPath(test.path)
			if ok != test.resolved {
				t.Fatalf("gatewayForPath(%q) resolved=%v, want %v", test.path, ok, test.resolved)
			}
			if !test.resolved {
				continue
			}
			if gateway.ID != test.gateway || subpath != test.subpath {
				t.Fatalf("gatewayForPath(%q) = (%q, %q), want (%q, %q)",
					test.path, gateway.ID, subpath, test.gateway, test.subpath)
			}
		}
	}
}

// A request body that cannot be read must leave the same audit trail as the
// other error paths: a row with only a bare 400 status and an empty error
// field gives nothing to debug from.
func TestReadRequestBodyRecordsFailureInLogEntry(t *testing.T) {
	p := &Proxy{Logger: slog.Default()}
	failing := &failingBody{}
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/ds/responses", failing)
	recorder := httptest.NewRecorder()
	logEntry := store.RequestLog{ID: "req-test"}

	if _, ok := p.readRequestBody(recorder, req, &logEntry, 1024); ok {
		t.Fatal("readRequestBody reported success for a failing body")
	}
	if logEntry.StatusCode != http.StatusBadRequest {
		t.Fatalf("StatusCode = %d, want %d", logEntry.StatusCode, http.StatusBadRequest)
	}
	if logEntry.Error == "" {
		t.Fatal("Error was not recorded in the log entry")
	}
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("client status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
}

type failingBody struct{}

func (failingBody) Read([]byte) (int, error) {
	return 0, fmt.Errorf("simulated read failure")
}
