package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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

	store, err := OpenStore(t.TempDir() + "/proxy.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := DefaultConfig(t.TempDir() + "/config.json")
	gateway := cfg.Gateways["st"]
	gateway.BaseURL = upstream.URL
	cfg.Gateways["st"] = gateway
	p := &Proxy{config: cfg, store: store, logger: slog.Default(), client: upstream.Client()}

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
	logs, err := store.List(context.Background(), LogFilter{Limit: 10})
	if err != nil || len(logs) != 1 {
		t.Fatalf("expected one log, got %d, err=%v", len(logs), err)
	}
	if logs[0].Usage.CacheReadTokens != 50 || !logs[0].Usage.CacheSupported {
		t.Fatalf("usage was not logged: %+v", logs[0].Usage)
	}
	detail, err := store.Get(context.Background(), logs[0].ID)
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

	store, err := OpenStore(t.TempDir() + "/proxy.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := DefaultConfig(t.TempDir() + "/config.json")
	gateway := cfg.Gateways["st"]
	gateway.BaseURL = upstream.URL
	gateway.UserAgentOverrideEnabled = true
	gateway.UserAgentOverride = "proxy-dev-agent/1"
	cfg.Gateways["st"] = gateway
	p := &Proxy{config: cfg, store: store, logger: slog.Default(), client: upstream.Client()}

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
	logs, err := store.List(context.Background(), LogFilter{Limit: 10})
	if err != nil || len(logs) != 1 {
		t.Fatalf("expected one log, got %d, err=%v", len(logs), err)
	}
	detail, err := store.Get(context.Background(), logs[0].ID)
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
	if !strings.Contains(detail.UpstreamHeadersActual, "proxy-dev-agent/1") {
		t.Fatalf("actual upstream headers did not contain overridden user agent: %s", detail.UpstreamHeadersActual)
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

	store, err := OpenStore(t.TempDir() + "/proxy.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := DefaultConfig(t.TempDir() + "/config.json")
	gateway := cfg.Gateways["st"]
	gateway.BaseURL = upstream.URL
	cfg.Gateways["st"] = gateway
	p := &Proxy{config: cfg, store: store, logger: slog.Default(), client: upstream.Client()}

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
	logs, err := store.List(context.Background(), LogFilter{Limit: 10})
	if err != nil || len(logs) != 1 {
		t.Fatalf("expected one log, got %d, err=%v", len(logs), err)
	}
	detail, err := store.Get(context.Background(), logs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(detail.RequestBody), `"type":"function"`) || !strings.Contains(string(detail.UpstreamBody), `"type":"function_call"`) {
		t.Fatalf("request comparison did not retain both protocol forms: request=%s upstream=%s", detail.RequestBody, detail.UpstreamBody)
	}
}

func TestProxyRejectsWrongProtocol(t *testing.T) {
	store, err := OpenStore(t.TempDir() + "/proxy.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := DefaultConfig(t.TempDir() + "/config.json")
	p := &Proxy{config: cfg, store: store, logger: slog.Default(), client: http.DefaultClient}
	body, err := json.Marshal(map[string]string{"model": "sense-model"})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/oc/chat/completions", "/st/responses", "/ve/chat/completions", "/oc/models"} {
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

	store, err := OpenStore(t.TempDir() + "/proxy.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := DefaultConfig(t.TempDir() + "/config.json")
	for id, gateway := range cfg.Gateways {
		gateway.BaseURL = upstream.URL + "/v1"
		cfg.Gateways[id] = gateway
	}
	p := &Proxy{config: cfg, store: store, logger: slog.Default(), client: upstream.Client()}

	for _, path := range []string{"/oc/responses", "/ve/responses"} {
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

	store, err := OpenStore(t.TempDir() + "/proxy.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := DefaultConfig(t.TempDir() + "/config.json")
	gateway := cfg.Gateways["st"]
	gateway.BaseURL = upstream.URL
	cfg.Gateways["st"] = gateway
	p := &Proxy{config: cfg, store: store, logger: slog.Default(), client: upstream.Client()}
	body := []byte(`{"model":"sense-model"}`)
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/st/chat/completions", strings.NewReader(string(body)))
	recorder := httptest.NewRecorder()
	p.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusTooManyRequests || !strings.Contains(recorder.Body.String(), "upstream_error") || !strings.Contains(recorder.Body.String(), "quota exceeded") {
		t.Fatalf("unexpected normalized error: %d %s", recorder.Code, recorder.Body.String())
	}
	logs, err := store.List(context.Background(), LogFilter{Limit: 10})
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
