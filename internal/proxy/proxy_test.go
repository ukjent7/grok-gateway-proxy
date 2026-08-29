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

func TestProxyNormalizesUpstreamErrors(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"quota exceeded"}}`))
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
	if recorder.Code != http.StatusTooManyRequests || !strings.Contains(recorder.Body.String(), "upstream_error") || !strings.Contains(recorder.Body.String(), "quota exceeded") {
		t.Fatalf("unexpected normalized error: %d %s", recorder.Code, recorder.Body.String())
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

func TestProxyBlocksGrokModelsWithoutCallingUpstream(t *testing.T) {
	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		t.Error("blocked Grok request reached upstream")
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

	requestBody := []byte(`{"model":"grok-4.6","stream":true,"input":"title this"}`)
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/std/responses", strings.NewReader(string(requestBody)))
	recorder := httptest.NewRecorder()
	p.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected synthetic success, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if upstreamCalls != 0 {
		t.Fatalf("blocked request called upstream %d times", upstreamCalls)
	}
	if !strings.Contains(recorder.Body.String(), "response.completed") || !strings.Contains(recorder.Body.String(), "data: [DONE]") {
		t.Fatalf("synthetic Responses stream was incomplete: %s", recorder.Body.String())
	}
	// The synthetic response object must deserialize under the strict client
	// schema: created_at is a required field.
	if !strings.Contains(recorder.Body.String(), `"created_at":`) {
		t.Fatalf("synthetic Responses stream is missing created_at: %s", recorder.Body.String())
	}
	logs, err := st.List(context.Background(), store.LogFilter{Limit: 1})
	if err != nil || len(logs) != 1 {
		t.Fatalf("expected one blocked request log, got %d, err=%v", len(logs), err)
	}
	if logs[0].StatusCode != http.StatusOK || !logs[0].Success || !strings.Contains(logs[0].Error, "blocked") {
		t.Fatalf("blocked request was not logged clearly: %+v", logs[0])
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
	tools := upstreamBody["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["type"] != "function" {
		t.Fatalf("non-standard tools reached upstream: %+v", tools)
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
