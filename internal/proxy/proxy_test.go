package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
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
	cfg.SetUpstreamTimeout(200 * time.Millisecond)
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

	if recorder.Code != http.StatusGatewayTimeout {
		t.Fatalf("expected status %d, got %d", http.StatusGatewayTimeout, recorder.Code)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("timeout took %v, want ~300ms", elapsed)
	}
}

func TestProxyStreamingSurvivesUpstreamTimeout(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		for i := 0; i < 8; i++ {
			select {
			case <-time.After(60 * time.Millisecond):
			case <-r.Context().Done():
				return
			}
			chunk := fmt.Sprintf("data: {\"type\":\"response.output_text.delta\",\"sequence_number\":%d,\"delta\":\"x\"}\n\n", i)
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
	cfg.SetUpstreamTimeout(300 * time.Millisecond)
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

	if _, exists := upstreamBody["stream_tool_calls"]; exists {
		t.Fatalf("stream_tool_calls reached upstream: %+v", upstreamBody)
	}

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

func TestProxyRecordsMidStreamFailureAsFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"half\"}\n\n")); err != nil {
			return
		}
		w.(http.Flusher).Flush()

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
			cfg.SetBodyCaptureLimitKB(test.limitKB)
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
	cfg.SetBodyCaptureLimitKB(0)
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

func TestResponseBodyLimitZeroFallsBackToSafetyCeiling(t *testing.T) {
	st, err := store.OpenStore(t.TempDir() + "/proxy.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	for _, limitKB := range []int{0} {
		cfg := config.DefaultConfig(t.TempDir() + "/config.json")
		cfg.SetBodyCaptureLimitKB(limitKB)
		p := &Proxy{Config: cfg, Store: st}
		if got := p.responseBodyLimit(); got != maxBodyBytes {
			t.Fatalf("responseBodyLimit() with body_capture_limit_kb=%d = %d, want %d",
				limitKB, got, maxBodyBytes)
		}
	}

	cfg := config.DefaultConfig(t.TempDir() + "/config.json")
	cfg.SetBodyCaptureLimitKB(8)
	p := &Proxy{Config: cfg, Store: st}
	if got := p.responseBodyLimit(); got != 8<<10 {
		t.Fatalf("responseBodyLimit() = %d, want %d", got, 8<<10)
	}

	if maxBodyBytes < defaultResponseBodySize {
		t.Fatalf("maxBodyBytes (%d) is below the default cap (%d)", maxBodyBytes, defaultResponseBodySize)
	}
}

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

func TestRoutePrefersLongestPrefix(t *testing.T) {
	cfg := config.DefaultConfig(t.TempDir() + "/config.json")

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

		{path: "/static/app.js", resolved: false},
		{path: "/status", resolved: false},
	}

	for attempt := 0; attempt < 50; attempt++ {
		for _, test := range tests {
			gateway, subpath, ok := p.Config.MatchGateway(test.path)
			if ok != test.resolved {
				t.Fatalf("MatchGateway(%q) resolved=%v, want %v", test.path, ok, test.resolved)
			}
			if !test.resolved {
				continue
			}
			if gateway.ID != test.gateway || subpath != test.subpath {
				t.Fatalf("MatchGateway(%q) = (%q, %q), want (%q, %q)",
					test.path, gateway.ID, subpath, test.gateway, test.subpath)
			}
		}
	}
}

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

func TestRequestBodyRejectedFromDeclaredLengthAlone(t *testing.T) {
	p := &Proxy{Logger: slog.Default()}
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/ds/responses", strings.NewReader(`{}`))
	req.ContentLength = maxBodyBytes + 1
	recorder := httptest.NewRecorder()
	logEntry := store.RequestLog{ID: "req-test"}

	if _, ok := p.readRequestBody(recorder, req, &logEntry, 1024); ok {
		t.Fatal("an oversized declared body was accepted")
	}
	if logEntry.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("StatusCode = %d, want %d", logEntry.StatusCode, http.StatusRequestEntityTooLarge)
	}
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("client status = %d, want %d", recorder.Code, http.StatusRequestEntityTooLarge)
	}

	if logEntry.RequestBody != nil {
		t.Fatalf("a rejected body was still captured: %q", logEntry.RequestBody)
	}
}

func TestProxyTranslatesGrokHeadersToStandard(t *testing.T) {
	var gotClientReq, gotSessionLower, gotSessionUpper, gotReqID, gotCorr, gotUA, gotAccept string
	var gotGrokConv, gotGrokReq string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotClientReq = r.Header.Get("X-Client-Request-Id")
		gotSessionLower = r.Header.Get("session_id")
		gotSessionUpper = r.Header.Get("X-Session-Id")
		gotReqID = r.Header.Get("X-Request-Id")
		gotCorr = r.Header.Get("X-Correlation-Id")
		gotUA = r.Header.Get("User-Agent")
		gotAccept = r.Header.Get("Accept")
		gotGrokConv = r.Header.Get("X-Grok-Conv-Id")
		gotGrokReq = r.Header.Get("X-Grok-Req-Id")
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
	req.Header.Set("X-Grok-Session-Id", "sess-123")
	req.Header.Set("X-Grok-Conv-Id", "conv-999")
	req.Header.Set("X-Grok-Req-Id", "req-456")
	req.Header.Set("User-Agent", "grok-shell/9.9.9 (windows; x86_64)")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if gotGrokConv != "" || gotGrokReq != "" {
		t.Fatalf("raw grok headers leaked: conv=%q req=%q", gotGrokConv, gotGrokReq)
	}
	if gotClientReq != "sess-123" {
		t.Fatalf("X-Client-Request-Id not translated to session: %q", gotClientReq)
	}
	if gotSessionLower != "sess-123" {
		t.Fatalf("session_id not translated: %q", gotSessionLower)
	}

	if gotSessionUpper != "" {
		t.Fatalf("X-Session-Id sent under the OpenAI dialect: %q", gotSessionUpper)
	}

	if want := rec.Header().Get("X-Request-Id"); gotReqID != want {
		t.Fatalf("X-Request-Id = %q, want the audit id %q", gotReqID, want)
	}
	if gotCorr != "req-456" {
		t.Fatalf("X-Correlation-Id not translated: %q", gotCorr)
	}
	if gotUA != "grok-shell/9.9.9 (windows; x86_64)" {
		t.Fatalf("User-Agent not preserved: %q", gotUA)
	}
	if gotAccept != "application/json" {
		t.Fatalf("Accept default not set: %q", gotAccept)
	}
}

func TestProxyTranslatesGrokConvIdFallback(t *testing.T) {
	var gotSessionLower string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSessionLower = r.Header.Get("session_id")
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
	req.Header.Set("X-Grok-Conv-Id", "conv-only")
	p.ServeHTTP(httptest.NewRecorder(), req)

	if gotSessionLower != "conv-only" {
		t.Fatalf("fallback to conv-id not translated: %q", gotSessionLower)
	}
}

func TestProxyInjectsPromptCacheKeyFromGrokHeader(t *testing.T) {
	var upstreamBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&upstreamBody)
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
	req.Header.Set("X-Grok-Session-Id", "sess-cache-123")
	p.ServeHTTP(httptest.NewRecorder(), req)

	if upstreamBody["prompt_cache_key"] != "sess-cache-123" {
		t.Fatalf("prompt_cache_key not injected: %+v", upstreamBody)
	}

	logs, err := st.List(context.Background(), store.LogFilter{Limit: 1})
	if err != nil || len(logs) != 1 {
		t.Fatalf("expected one log, got %d", len(logs))
	}
	detail, err := st.Get(context.Background(), logs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(detail.UpstreamBody), "prompt_cache_key") {
		t.Fatalf("audit upstream body missing injected key: %s", detail.UpstreamBody)
	}
}

func TestProxyDoesNotOverwriteExistingPromptCacheKey(t *testing.T) {
	var upstreamBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&upstreamBody)
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

	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/std/responses", strings.NewReader(`{"model":"m","prompt_cache_key":"keep-me","input":"hi"}`))
	req.Header.Set("X-Grok-Session-Id", "sess-new")
	p.ServeHTTP(httptest.NewRecorder(), req)

	if upstreamBody["prompt_cache_key"] != "keep-me" {
		t.Fatalf("existing prompt_cache_key was overwritten: %+v", upstreamBody)
	}
}

func TestProxyEnsuresStandardDefaults(t *testing.T) {
	var gotUA, gotAccept, gotAuth, gotAPIKey string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		gotAccept = r.Header.Get("Accept")
		gotAuth = r.Header.Get("Authorization")
		gotAPIKey = r.Header.Get("X-Api-Key")
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

	gateway.ForwardHeaders = []string{"Accept"}
	cfg.Gateways["std"] = gateway
	p := &Proxy{Config: cfg, Store: st, Logger: slog.Default(), Client: upstream.Client()}

	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/std/responses", strings.NewReader(`{"model":"m","input":"hi"}`))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("X-Api-Key", "key-123")

	p.ServeHTTP(httptest.NewRecorder(), req)

	if gotUA == "" {
		t.Fatalf("User-Agent default not set: %q", gotUA)
	}
	if gotAccept != "application/json" {
		t.Fatalf("Accept default not ensured: %q", gotAccept)
	}
	if gotAuth != "Bearer secret" {
		t.Fatalf("Authorization not ensured despite restrictive allowlist: %q", gotAuth)
	}
	if gotAPIKey != "key-123" {
		t.Fatalf("X-Api-Key not ensured: %q", gotAPIKey)
	}
}

func TestProxyOpenCodeBaseURLInjectsSessionHeader(t *testing.T) {
	var gotOpenCodeSession string
	var gotUA string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotOpenCodeSession = r.Header.Get("x-opencode-session")
		gotUA = r.Header.Get("User-Agent")
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
	gateway.BaseURL = "http://opencode.ai/v1"
	cfg.Gateways["std"] = gateway

	customClient := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return net.Dial(network, upstream.Listener.Addr().String())
			},
		},
	}

	p := &Proxy{Config: cfg, Store: st, Logger: slog.Default(), Client: customClient}

	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/std/responses", strings.NewReader(`{"model":"m","input":"hi"}`))
	req.Header.Set("X-Grok-Conv-Id", "conv-grok-stable-1")
	req.Header.Set("User-Agent", "grok-pager/1.0.13 grok-shell/1.0.13 (windows; x86_64)")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if gotOpenCodeSession != "conv-grok-stable-1" {
		t.Fatalf("x-opencode-session = %q, want %q", gotOpenCodeSession, "conv-grok-stable-1")
	}
	if gotUA != "grok-pager/1.0.13 grok-shell/1.0.13 (windows; x86_64)" {
		t.Fatalf("User-Agent = %q, want grok UA preserved", gotUA)
	}
}

