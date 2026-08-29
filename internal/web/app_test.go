package web

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"grok-gateway-proxy/internal/config"
	"grok-gateway-proxy/internal/proxy"
	"grok-gateway-proxy/internal/store"
)

func TestStaticAssetsAreNotMatchedAsGatewayPaths(t *testing.T) {
	app := &App{}

	for _, path := range []string{"/static/style.css", "/static/js/app.js"} {
		req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8787"+path, nil)
		recorder := httptest.NewRecorder()
		app.ServeHTTP(recorder, req)

		if recorder.Code != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d: %s", path, recorder.Code, recorder.Body.String())
		}
		if strings.TrimSpace(recorder.Body.String()) == "" {
			t.Fatalf("%s: expected embedded asset body", path)
		}
	}
}

func TestHasPathPrefixUsesPathBoundary(t *testing.T) {
	for _, test := range []struct {
		path   string
		prefix string
		want   bool
	}{
		{path: "/st", prefix: "/st", want: true},
		{path: "/st/chat/completions", prefix: "/st", want: true},
		{path: "/static/app.js", prefix: "/st", want: false},
		{path: "/status", prefix: "/st", want: false},
	} {
		if got := hasPathPrefix(test.path, test.prefix); got != test.want {
			t.Errorf("hasPathPrefix(%q, %q) = %v, want %v", test.path, test.prefix, got, test.want)
		}
	}
}

func TestManagementAPIIsOpenForLocalTool(t *testing.T) {
	cfg := config.DefaultConfig(filepath.Join(t.TempDir(), "config.json"))
	app := &App{config: cfg, logger: slog.Default()}
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8787/api/config", nil)
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if _, exists := body["auth_enabled"]; exists {
		t.Fatalf("auth_enabled should not be present for local tool, got: %+v", body)
	}
}

func TestPatchGatewayPartialUpdate(t *testing.T) {
	cfg := config.DefaultConfig(filepath.Join(t.TempDir(), "config.json"))
	app := &App{config: cfg, logger: slog.Default()}
	before := cfg.Gateways["st"]

	req := httptest.NewRequest(http.MethodPatch, "http://127.0.0.1:8787/api/gateways/st", strings.NewReader(`{"enabled":false}`))
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	gw := cfg.Gateways["st"]
	if gw.Enabled {
		t.Fatal("enabled flag should be patched to false")
	}
	if gw.BaseURL != before.BaseURL || gw.Name != before.Name || gw.UserAgentOverride != before.UserAgentOverride {
		t.Fatalf("unchanged fields were clobbered: %+v", gw)
	}

	req = httptest.NewRequest(http.MethodPatch, "http://127.0.0.1:8787/api/gateways/missing", strings.NewReader(`{"enabled":true}`))
	recorder = httptest.NewRecorder()
	app.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown gateway, got %d", recorder.Code)
	}

	req = httptest.NewRequest(http.MethodPatch, "http://127.0.0.1:8787/api/gateways/st", strings.NewReader(`{"enabled":`))
	recorder = httptest.NewRecorder()
	app.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid JSON, got %d", recorder.Code)
	}
}

func TestHealthzReportsGatewayStatus(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer upstream.Close()

	cfg := config.DefaultConfig(filepath.Join(t.TempDir(), "config.json"))
	gw := cfg.Gateways["std"]
	gw.BaseURL = upstream.URL
	cfg.Gateways["std"] = gw
	app := &App{config: cfg, logger: slog.Default(), upstreams: map[string]upstreamHealth{}}
	app.proxy = proxy.NewProxy(cfg, nil, slog.Default())
	app.proxy.Client = upstream.Client()
	app.probeUpstream("std", cfg.Gateways["std"])

	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8787/healthz", nil)
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	upstreams, ok := body["upstreams"].(map[string]any)
	if !ok {
		t.Fatalf("expected upstreams map, got %s", recorder.Body.String())
	}
	std, ok := upstreams["std"].(map[string]any)
	if !ok || std["reachable"] != true || std["status"] != float64(200) {
		t.Fatalf("expected reachable std gateway with status 200, got: %+v", upstreams)
	}
}

func TestGatewayConfigAPIUpdatesUserAgentOverride(t *testing.T) {
	cfg := config.DefaultConfig(filepath.Join(t.TempDir(), "config.json"))
	app := &App{config: cfg, logger: slog.Default()}
	req := httptest.NewRequest(http.MethodPatch, "http://127.0.0.1:8787/api/gateways/st", strings.NewReader(`{"user_agent_override_enabled":true,"user_agent_override":"debug-agent/1"}`))
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	st, ok := response["gateway"].(map[string]any)
	if !ok || st["user_agent_override_enabled"] != true || st["user_agent_override"] != "debug-agent/1" {
		t.Fatalf("unexpected gateway response: %+v", response)
	}
	if !cfg.Gateways["st"].UserAgentOverrideEnabled || cfg.Gateways["st"].UserAgentOverride != "debug-agent/1" {
		t.Fatalf("config was not updated: %+v", cfg.Gateways["st"])
	}
}

func TestGatewayConfigAPIUpdatesGlobalProxySetting(t *testing.T) {
	cfg := config.DefaultConfig(filepath.Join(t.TempDir(), "config.json"))
	app := &App{config: cfg, logger: slog.Default()}
	req := httptest.NewRequest(http.MethodPatch, "http://127.0.0.1:8787/api/gateways/st", strings.NewReader(`{"use_proxy":false}`))
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if cfg.Gateways["st"].UseProxy {
		t.Fatal("environment proxy setting should be patched to false")
	}
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	gateway, ok := response["gateway"].(map[string]any)
	if !ok || gateway["use_proxy"] != false {
		t.Fatalf("unexpected gateway response: %+v", response)
	}
}

func TestGatewayConfigWithoutProxySettingDefaultsToGlobalProxy(t *testing.T) {
	var gateway config.GatewayConfig
	if err := json.Unmarshal([]byte(`{"id":"st","prefix":"/st","name":"SenseNova","base_url":"https://example.test","protocol":"chat_completions","enabled":true}`), &gateway); err != nil {
		t.Fatal(err)
	}
	if !gateway.UseProxy {
		t.Fatal("missing use_proxy should preserve the global-proxy default")
	}
}

func TestUpstreamClientProxyModes(t *testing.T) {
	proxyTransport, ok := proxy.NewUpstreamClient("http://127.0.0.1:7890").Transport.(*http.Transport)
	if !ok || proxyTransport.Proxy == nil {
		t.Fatal("configured-proxy client must configure a proxy function")
	}
	proxyURL, err := proxyTransport.Proxy(httptest.NewRequest(http.MethodGet, "https://example.test/", nil))
	if err != nil || proxyURL == nil || proxyURL.String() != "http://127.0.0.1:7890" {
		t.Fatalf("unexpected configured proxy URL: %v, err=%v", proxyURL, err)
	}
	directTransport, ok := proxy.NewUpstreamClient("").Transport.(*http.Transport)
	if !ok || directTransport.Proxy != nil {
		t.Fatal("direct client must not configure a proxy function")
	}
}

func TestProxyConfigAPIUpdatesGlobalProxyURL(t *testing.T) {
	cfg := config.DefaultConfig(filepath.Join(t.TempDir(), "config.json"))
	p := proxy.NewProxy(cfg, nil, slog.Default())
	p.Client = proxy.NewUpstreamClient("")
	app := &App{config: cfg, logger: slog.Default(), proxy: p}
	req := httptest.NewRequest(http.MethodPatch, "http://127.0.0.1:8787/api/proxy", strings.NewReader(`{"proxy_url":"https://proxy.example.test:8443"}`))
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if cfg.ProxyURL() != "https://proxy.example.test:8443" {
		t.Fatalf("proxy URL was not updated: %q", cfg.ProxyURL())
	}
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response["proxy_url"] != cfg.ProxyURL() {
		t.Fatalf("unexpected proxy response: %+v", response)
	}

	req = httptest.NewRequest(http.MethodPatch, "http://127.0.0.1:8787/api/proxy", strings.NewReader(`{"proxy_url":"socks5://127.0.0.1:1080"}`))
	recorder = httptest.NewRecorder()
	app.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for SOCKS URL, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if cfg.ProxyURL() != "https://proxy.example.test:8443" {
		t.Fatalf("invalid proxy URL changed config: %q", cfg.ProxyURL())
	}
}

// TestEmbeddedJavaScriptImportsAndReferences verifies that all embedded ES modules
// have matching export/import bindings and no referenced utils functions are omitted.
func TestEmbeddedJavaScriptImportsAndReferences(t *testing.T) {
	entries, err := staticFiles.ReadDir("static/js")
	if err != nil {
		t.Fatalf("read static/js directory: %v", err)
	}

	jsFiles := make(map[string]string)
	exports := make(map[string]map[string]bool)

	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".js") {
			continue
		}
		contentBytes, err := staticFiles.ReadFile("static/js/" + entry.Name())
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		content := string(contentBytes)
		jsFiles[entry.Name()] = content
		exports[entry.Name()] = make(map[string]bool)

		// Parse export function, export const, export async function, export { ... }
		lines := strings.Split(content, "\n")
		for _, line := range lines {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "export function") || strings.HasPrefix(trimmed, "export async function") {
				parts := strings.Fields(trimmed)
				for i, p := range parts {
					if strings.HasPrefix(p, "function") && i+1 < len(parts) {
						name := strings.Split(parts[i+1], "(")[0]
						exports[entry.Name()][strings.TrimSpace(name)] = true
					}
				}
			} else if strings.HasPrefix(trimmed, "export const") || strings.HasPrefix(trimmed, "export let") {
				parts := strings.Fields(trimmed)
				if len(parts) >= 3 {
					name := strings.TrimRight(parts[2], "=;, ")
					exports[entry.Name()][name] = true
				}
			}
		}
	}

	for fileName, content := range jsFiles {
		// Verify import { a, b } from './mod.js'
		for _, line := range strings.Split(content, "\n") {
			trimmed := strings.TrimSpace(line)
			if !strings.HasPrefix(trimmed, "import") || !strings.Contains(trimmed, "from") {
				continue
			}
			start := strings.Index(trimmed, "{")
			end := strings.Index(trimmed, "}")
			modStart := strings.Index(trimmed, "from")
			if start == -1 || end == -1 || modStart == -1 || end <= start {
				continue
			}
			rawSymbols := trimmed[start+1 : end]
			rawMod := strings.Trim(strings.TrimSpace(trimmed[modStart+4:]), "';\"")
			modName := strings.TrimPrefix(rawMod, "./")

			modExports, exists := exports[modName]
			if !exists {
				continue
			}

			symbols := strings.Split(rawSymbols, ",")
			for _, sym := range symbols {
				sym = strings.TrimSpace(sym)
				if sym == "" {
					continue
				}
				if !modExports[sym] {
					t.Errorf("%s: imports %q from %q, but %q is not exported by %s", fileName, sym, modName, sym, modName)
				}
			}
		}
	}
}

func TestDeleteLogsReclaimsSpace(t *testing.T) {
	st, err := store.OpenStore(filepath.Join(t.TempDir(), "proxy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if err := st.Insert(context.Background(), store.RequestLog{
		ID:        "req-to-delete",
		StartedAt: time.Now().UTC(),
		GatewayID: "oc",
	}); err != nil {
		t.Fatal(err)
	}

	cfg := config.DefaultConfig(filepath.Join(t.TempDir(), "config.json"))
	app := NewApp(cfg, st, slog.Default(), "1.0.0")
	req := httptest.NewRequest(http.MethodDelete, "http://127.0.0.1:8787/api/logs", nil)
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if deleted, ok := body["deleted"].(float64); !ok || deleted != 1 {
		t.Fatalf("expected deleted 1, got %+v", body)
	}
	count, err := st.Count(context.Background(), store.LogFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("expected 0 logs remaining, got %d", count)
	}
}
