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

	for _, path := range []string{"/static/style.css", "/static/app.js"} {
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
