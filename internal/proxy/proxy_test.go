package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
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
	gateway := cfg.Gateways["ve"]
	gateway.BaseURL = upstream.URL
	cfg.Gateways["ve"] = gateway
	p := &Proxy{Config: cfg, Store: st, Logger: slog.Default(), Client: upstream.Client(), ResponseBodySize: capSize}

	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/ve/responses", strings.NewReader(`{"model":"m","stream":true,"input":"hi"}`))
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
	gateway := cfg.Gateways["ve"]
	gateway.BaseURL = upstream.URL
	cfg.Gateways["ve"] = gateway
	p := &Proxy{Config: cfg, Store: st, Logger: slog.Default(), Client: upstream.Client()}

	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/ve/responses",
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

func TestProxyAppliesMuseProfileOnlyForMuseModel(t *testing.T) {
	var upstreamBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&upstreamBody); err != nil {
			t.Errorf("decode upstream request: %v", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"muse-response","output":[]}`))
	}))
	defer upstream.Close()

	st, err := store.OpenStore(t.TempDir() + "/proxy.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := config.DefaultConfig(t.TempDir() + "/config.json")
	gateway := cfg.Gateways["oc"]
	gateway.BaseURL = upstream.URL
	cfg.Gateways["oc"] = gateway
	p := &Proxy{Config: cfg, Store: st, Logger: slog.Default(), Client: upstream.Client()}

	requestBody := []byte(`{"model":"muse-spark-1.2-contributo","stream":true,"stream_tool_calls":true,"input":[]}`)
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/oc/responses", strings.NewReader(string(requestBody)))
	recorder := httptest.NewRecorder()
	p.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "muse-response") {
		t.Fatalf("unexpected Muse response: %d %s", recorder.Code, recorder.Body.String())
	}
	if _, exists := upstreamBody["stream_tool_calls"]; exists {
		t.Fatalf("unsupported Muse parameter reached upstream: %+v", upstreamBody)
	}
	if upstreamBody["model"] != "muse-spark-1.2-contributo" {
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
		t.Fatalf("request comparison did not preserve the Muse rewrite: request=%s upstream=%s", detail.RequestBody, detail.UpstreamBody)
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
	gateway := cfg.Gateways["oc"]
	gateway.BaseURL = upstream.URL
	cfg.Gateways["oc"] = gateway
	p := &Proxy{Config: cfg, Store: st, Logger: slog.Default(), Client: upstream.Client()}

	requestBody := []byte(`{"model":"grok-4.6","stream":true,"input":"title this"}`)
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/oc/responses", strings.NewReader(string(requestBody)))
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
	logs, err := st.List(context.Background(), store.LogFilter{Limit: 1})
	if err != nil || len(logs) != 1 {
		t.Fatalf("expected one blocked request log, got %d, err=%v", len(logs), err)
	}
	if logs[0].StatusCode != http.StatusOK || !logs[0].Success || !strings.Contains(logs[0].Error, "blocked") {
		t.Fatalf("blocked request was not logged clearly: %+v", logs[0])
	}
}

func TestFxSessionIDFromPromptCacheKey(t *testing.T) {
	body := []byte(`{"model":"zai/glm-5.2","prompt_cache_key":"01a01ba6-5d84-7403-bbc6-c52a282efc6f","input":"hi"}`)
	req := httptest.NewRequest(http.MethodPost, "/ve/responses", bytes.NewReader(body))
	if sid := fxSessionID(req, body); sid != "01a01ba6-5d84-7403-bbc6-c52a282efc6f" {
		t.Fatalf("expected prompt_cache_key as session id, got %q", sid)
	}
}

func TestFxSessionIDFromSessionHeader(t *testing.T) {
	body := []byte(`{"model":"zai/glm-5.2","input":"hi"}`)
	req := httptest.NewRequest(http.MethodPost, "/ve/responses", bytes.NewReader(body))
	req.Header.Set("X-Session-Id", "session-123")
	if sid := fxSessionID(req, body); sid != "session-123" {
		t.Fatalf("expected session header, got %q", sid)
	}
}

func TestFxSessionIDIgnoresGrokHeaders(t *testing.T) {
	body := []byte(`{"model":"zai/glm-5.2","input":"hi"}`)
	req := httptest.NewRequest(http.MethodPost, "/ve/responses", bytes.NewReader(body))
	req.Header.Set("X-Grok-Session-Id", "grok-session-123")
	req.Header.Set("X-Grok-Conv-Id", "conv-456")
	sid := fxSessionID(req, body)
	if sid == "grok-session-123" || sid == "conv-456" {
		t.Fatalf("fx session id must not leak grok headers, got %q", sid)
	}
	if !strings.HasPrefix(sid, "pi-") {
		t.Fatalf("expected random pi- fallback when no cache key or session header, got %q", sid)
	}
}

func TestFxSessionIDFallbackRandom(t *testing.T) {
	body := []byte(`{"model":"zai/glm-5.2","input":"hi"}`)
	req := httptest.NewRequest(http.MethodPost, "/ve/responses", bytes.NewReader(body))
	sid := fxSessionID(req, body)
	if !strings.HasPrefix(sid, "pi-") {
		t.Fatalf("expected random pi- fallback, got %q", sid)
	}
}

func TestFxSessionIDStableAcrossRequests(t *testing.T) {
	body := []byte(`{"model":"zai/glm-5.2","prompt_cache_key":"stable-key","input":"hi"}`)
	req1 := httptest.NewRequest(http.MethodPost, "/ve/responses", bytes.NewReader(body))
	req2 := httptest.NewRequest(http.MethodPost, "/ve/responses", bytes.NewReader(body))
	if fxSessionID(req1, body) != fxSessionID(req2, body) {
		t.Fatal("session id must be stable across requests with same prompt_cache_key")
	}
}

func TestFXModeStableSessionIDEndToEnd(t *testing.T) {
	var gotSessionIDs []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSessionIDs = append(gotSessionIDs, r.Header.Get("X-Session-Id"))
		w.Header().Set("Content-Type", "text/event-stream")
		// Minimal v3 SSE: a finish event with empty output.
		finish := `data: {"type":"finish","finishReason":{"unified":"stop","raw":"stop"},"usage":{"inputTokens":{"total":10,"noCache":10,"cacheRead":0},"outputTokens":{"total":5,"text":5,"reasoning":0}}}` + "\n\n"
		_, _ = io.WriteString(w, finish)
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	st, err := store.OpenStore(t.TempDir() + "/proxy.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := config.DefaultConfig(t.TempDir() + "/config.json")
	gateway := cfg.Gateways["ve"]
	gateway.BaseURL = upstream.URL
	gateway.FXDisguiseEnabled = true
	cfg.Gateways["ve"] = gateway
	p := &Proxy{Config: cfg, Store: st, Logger: slog.Default(), Client: upstream.Client()}

	requestBody := []byte(`{"model":"zai/glm-5.2","prompt_cache_key":"conv-abc","stream":true,"input":"hi"}`)
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/ve/responses", bytes.NewReader(requestBody))
		recorder := httptest.NewRecorder()
		p.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusOK {
			t.Fatalf("request %d failed: %d %s", i, recorder.Code, recorder.Body.String())
		}
	}

	if len(gotSessionIDs) != 3 {
		t.Fatalf("expected 3 upstream calls, got %d", len(gotSessionIDs))
	}
	for i, sid := range gotSessionIDs {
		if sid != "conv-abc" {
			t.Fatalf("upstream request %d got session id %q, want %q", i, sid, "conv-abc")
		}
	}
}
