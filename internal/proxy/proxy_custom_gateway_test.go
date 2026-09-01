package proxy

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"grok-gateway-proxy/internal/config"
	"grok-gateway-proxy/internal/store"
)

// A custom gateway is not in gatewayAdapters — that table is compiled in and a
// custom gateway is created at runtime. This is the test that would fail if the
// adapter lookup ever went back to keying strictly on the gateway id.
func TestCustomGatewayReusesStandardResponsesAdapter(t *testing.T) {
	var upstreamBody map[string]any
	var upstreamPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&upstreamBody)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n" +
			"data: {\"type\":\"response.brand_new_event\"}\n\n"))
	}))
	defer upstream.Close()

	st, err := store.OpenStore(t.TempDir() + "/proxy.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cfg := config.DefaultConfig(t.TempDir() + "/config.json")
	cfg.Gateways["mygate"] = config.GatewayConfig{
		ID:       "mygate",
		Prefix:   "/mygate",
		Name:     "My Gate",
		BaseURL:  upstream.URL + "/v1",
		Protocol: config.ProtocolResponses,
		Enabled:  true,
	}
	p := &Proxy{Config: cfg, Store: st, Logger: slog.Default(), Client: upstream.Client()}

	body, _ := json.Marshal(map[string]any{
		"model":             "my-model",
		"input":             "hello",
		"stream":            true,
		"stream_tool_calls": true,
		"tools":             []any{map[string]any{"type": "x_search"}, map[string]any{"type": "function", "name": "lookup"}},
		"include":           []any{"no_inline_citations"},
	})
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/mygate/responses", strings.NewReader(string(body)))
	recorder := httptest.NewRecorder()
	p.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	if upstreamPath != "/v1/responses" {
		t.Fatalf("upstream path = %q, want /v1/responses", upstreamPath)
	}
	// The xAI-only extensions the standard adapter strips must be gone.
	if _, ok := upstreamBody["stream_tool_calls"]; ok {
		t.Error("stream_tool_calls reached the upstream")
	}
	if raw, _ := json.Marshal(upstreamBody["tools"]); strings.Contains(string(raw), "x_search") {
		t.Errorf("x_search tool reached the upstream: %s", raw)
	}
	// The standard adapter strips non-standard include *values* but keeps the
	// key; dropping it wholesale is DeepSeek's dialect handling, not this one.
	if raw, _ := json.Marshal(upstreamBody["include"]); strings.Contains(string(raw), "no_inline_citations") {
		t.Errorf("non-standard include value reached the upstream: %s", raw)
	}
	// And the reply stream is filtered to the client's event vocabulary.
	stream := recorder.Body.String()
	if !strings.Contains(stream, "response.output_text.delta") {
		t.Fatalf("known event was dropped: %s", stream)
	}
	if strings.Contains(stream, "response.brand_new_event") {
		t.Fatalf("unknown event survived the filter: %s", stream)
	}
}

// Without a base URL the custom gateway must answer like any other: a 503 from
// the proxy, not a 404 from the UI handler that owns unknown paths.
func TestCustomGatewayWithoutBaseURLReportsThroughProxy(t *testing.T) {
	st, err := store.OpenStore(t.TempDir() + "/proxy.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := config.DefaultConfig(t.TempDir() + "/config.json")
	cfg.Gateways["mygate"] = config.GatewayConfig{
		ID: "mygate", Prefix: "/mygate", Name: "My Gate", Protocol: config.ProtocolResponses, Enabled: true,
	}
	p := &Proxy{Config: cfg, Store: st, Logger: slog.Default(), Client: http.DefaultClient}

	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/mygate/responses", strings.NewReader(`{"model":"m"}`))
	recorder := httptest.NewRecorder()
	p.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusServiceUnavailable)
	}
	if !strings.Contains(recorder.Body.String(), "mygate") {
		t.Fatalf("error should name the gateway: %s", recorder.Body.String())
	}
}
