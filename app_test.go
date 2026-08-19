package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
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

func TestDefaultDataDirIsLocalDataFolder(t *testing.T) {
	if got, want := defaultDataDir(), filepath.Join(".", "data"); got != want {
		t.Fatalf("defaultDataDir() = %q, want %q", got, want)
	}
}

func TestAPITokenGatesManagementAPIWhenConfigured(t *testing.T) {
	cfg := DefaultConfig(filepath.Join(t.TempDir(), "config.json"))
	cfg.APIToken = "secret-token"
	app := &App{config: cfg, logger: slog.Default()}

	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8787/api/config", nil)
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d: %s", recorder.Code, recorder.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8787/api/config", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	recorder = httptest.NewRecorder()
	app.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with wrong token, got %d", recorder.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8787/api/config", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	recorder = httptest.NewRecorder()
	app.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200 with correct token, got %d: %s", recorder.Code, recorder.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["auth_enabled"] != true {
		t.Fatalf("expected auth_enabled true, got: %+v", body)
	}
}

func TestAPITokenUnsetAllowsManagementAPI(t *testing.T) {
	cfg := DefaultConfig(filepath.Join(t.TempDir(), "config.json"))
	app := &App{config: cfg, logger: slog.Default()}
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8787/api/config", nil)
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200 without token configured, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestPatchGatewayPartialUpdate(t *testing.T) {
	cfg := DefaultConfig(filepath.Join(t.TempDir(), "config.json"))
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

	cfg := DefaultConfig(filepath.Join(t.TempDir(), "config.json"))
	gw := cfg.Gateways["oc"]
	gw.BaseURL = upstream.URL
	cfg.Gateways["oc"] = gw
	app := &App{config: cfg, logger: slog.Default(), upstreams: map[string]upstreamHealth{}}
	app.proxy = &Proxy{config: cfg, logger: slog.Default(), client: upstream.Client()}
	app.probeUpstream("oc", cfg.Gateways["oc"])

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
	oc, ok := upstreams["oc"].(map[string]any)
	if !ok || oc["reachable"] != true || oc["status"] != float64(200) {
		t.Fatalf("expected reachable oc gateway with status 200, got: %+v", upstreams)
	}
}

func TestGatewayConfigAPIUpdatesUserAgentOverride(t *testing.T) {
	cfg := DefaultConfig(filepath.Join(t.TempDir(), "config.json"))
	app := &App{config: cfg, logger: slog.Default()}
	req := httptest.NewRequest(http.MethodPut, "http://127.0.0.1:8787/api/gateways", strings.NewReader(`{"gateways":{"st":{"name":"SenseNova","base_url":"https://token.sensenova.cn/v1","enabled":true,"forward_headers":[],"user_agent_override_enabled":true,"user_agent_override":"debug-agent/1"}}}`))
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	gateways, ok := response["gateways"].(map[string]any)
	if !ok {
		t.Fatalf("unexpected gateways response: %+v", response)
	}
	st, ok := gateways["st"].(map[string]any)
	if !ok || st["user_agent_override_enabled"] != true || st["user_agent_override"] != "debug-agent/1" {
		t.Fatalf("unexpected config response: %+v", response)
	}
	if !cfg.Gateways["st"].UserAgentOverrideEnabled || cfg.Gateways["st"].UserAgentOverride != "debug-agent/1" {
		t.Fatalf("config was not updated: %+v", cfg.Gateways["st"])
	}
}
