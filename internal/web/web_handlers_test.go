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
	"grok-gateway-proxy/internal/store"
)

func newTestApp(t *testing.T) (*App, *store.Store) {
	t.Helper()
	st, err := store.OpenStore(filepath.Join(t.TempDir(), "proxy.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := config.DefaultConfig(filepath.Join(t.TempDir(), "config.json"))
	return NewApp(cfg, st, slog.Default(), "1.0.0"), st
}

func seedLog(t *testing.T, st *store.Store, id string) {
	t.Helper()
	if err := st.Insert(context.Background(), store.RequestLog{
		ID:        id,
		StartedAt: time.Now().UTC(),
		GatewayID: "st",
		Model:     "sense-model",
		Success:   true,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestListLogsReturnsStoredEntries(t *testing.T) {
	app, st := newTestApp(t)
	seedLog(t, st, "req-list-1")

	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8787/api/logs?limit=10", nil)
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	var body struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Items) != 1 || body.Items[0].ID != "req-list-1" {
		t.Fatalf("unexpected items: %+v", body.Items)
	}
}

func TestCountLogsMatchesListFilters(t *testing.T) {
	app, st := newTestApp(t)
	seedLog(t, st, "req-count-1")
	seedLog(t, st, "req-count-2")

	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8787/api/logs/count", nil)
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if count, ok := body["count"].(float64); !ok || count != 2 {
		t.Fatalf("expected count 2, got %+v", body)
	}
}

func TestGetLogByID(t *testing.T) {
	app, st := newTestApp(t)
	seedLog(t, st, "req-get-1")

	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8787/api/logs/req-get-1", nil)
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "req-get-1") {
		t.Fatalf("unexpected body: %s", recorder.Body.String())
	}

	missing := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8787/api/logs/does-not-exist", nil)
	recorder = httptest.NewRecorder()
	app.ServeHTTP(recorder, missing)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for an unknown log id, got %d", recorder.Code)
	}
}

func TestHandleMetrics(t *testing.T) {
	app, st := newTestApp(t)
	seedLog(t, st, "req-metrics-1")

	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8787/api/metrics", nil)
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body) == 0 {
		t.Fatal("expected /api/metrics to report at least one metric")
	}
}

func TestDeleteLogsRejectsMalformedBefore(t *testing.T) {
	app, _ := newTestApp(t)
	req := httptest.NewRequest(http.MethodDelete, "http://127.0.0.1:8787/api/logs?before=not-a-time", nil)
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

// parseFilter is the only thing standing between raw query strings and the
// SQL layer, so every malformed value must fall back to a safe default or be
// rejected outright instead of reaching the store with surprising semantics.
func TestParseFilterClampsAndDefaults(t *testing.T) {
	tests := []struct {
		name       string
		query      string
		wantLimit  int
		wantOffset int
		wantWindow bool
	}{
		{name: "default limit", query: "", wantLimit: 50},
		{name: "limit is capped", query: "limit=100000", wantLimit: 500},
		{name: "non-positive limit falls back", query: "limit=0", wantLimit: 50},
		{name: "negative limit falls back", query: "limit=-5", wantLimit: 50},
		{name: "garbage limit falls back", query: "limit=abc", wantLimit: 50},
		{name: "offset applied", query: "offset=25", wantLimit: 50, wantOffset: 25},
		{name: "negative offset ignored", query: "offset=-1", wantLimit: 50},
		{name: "valid window parsed", query: "from=2024-01-01T00:00:00Z&to=2024-02-01T00:00:00Z", wantLimit: 50, wantWindow: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8787/api/logs?"+test.query, nil)
			filter, err := parseFilter(req)
			if err != nil {
				t.Fatalf("parseFilter(%q) returned error: %v", test.query, err)
			}
			if filter.Limit != test.wantLimit {
				t.Fatalf("Limit = %d, want %d", filter.Limit, test.wantLimit)
			}
			if filter.Offset != test.wantOffset {
				t.Fatalf("Offset = %d, want %d", filter.Offset, test.wantOffset)
			}
			hasWindow := filter.From != nil && filter.To != nil
			if hasWindow != test.wantWindow {
				t.Fatalf("time window present = %v, want %v", hasWindow, test.wantWindow)
			}
		})
	}
}

// An unparsable time bound must be rejected, not silently dropped: the caller
// believes it is looking at a time window, and answering with every row would
// corrupt counts and totals.
func TestParseFilterRejectsInvalidTimeWindow(t *testing.T) {
	for _, query := range []string{"from=yesterday", "to=tomorrow"} {
		req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8787/api/logs?"+query, nil)
		if _, err := parseFilter(req); err == nil {
			t.Fatalf("parseFilter(%q) accepted an invalid timestamp", query)
		}
	}
}

func TestParseFilterPassesThroughTextFilters(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8787/api/logs?gateway=st&model=sense-model&status=success", nil)
	filter, err := parseFilter(req)
	if err != nil {
		t.Fatal(err)
	}
	if filter.GatewayID != "st" || filter.Model != "sense-model" || filter.Status != "success" {
		t.Fatalf("unexpected filter: %+v", filter)
	}
}

func TestHandleSetupRendersASnippetPerGateway(t *testing.T) {
	app, _ := newTestApp(t)
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8787/api/setup", nil)
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	var snippets map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &snippets); err != nil {
		t.Fatal(err)
	}
	for id, gateway := range config.DefaultGateways {
		snippet, ok := snippets[id]
		if !ok {
			t.Fatalf("no snippet for gateway %q: %+v", id, snippets)
		}
		if !strings.Contains(snippet, gateway.Prefix) {
			t.Fatalf("snippet for %q does not mention its prefix %q: %s", id, gateway.Prefix, snippet)
		}
	}
}

func TestNormalizeListenAddr(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{in: "", want: "http://127.0.0.1:8787"},
		{in: "   ", want: "http://127.0.0.1:8787"},
		{in: ":8787", want: "http://127.0.0.1:8787"},
		// A wildcard listen address is a valid listen target but not a usable
		// base URL: no browser can open 0.0.0.0, so the snippet pins loopback.
		{in: "0.0.0.0:9000", want: "http://127.0.0.1:9000"},
		{in: "[::]:9000", want: "http://127.0.0.1:9000"},
		{in: "http://example.test:1234", want: "http://example.test:1234"},
		{in: "http://example.test/", want: "http://example.test"},
		{in: "https://example.test//", want: "https://example.test"},
	}
	for _, test := range tests {
		t.Run(test.in, func(t *testing.T) {
			if got := normalizeListenAddr(test.in); got != test.want {
				t.Fatalf("normalizeListenAddr(%q) = %q, want %q", test.in, got, test.want)
			}
		})
	}
}

func TestUIServesIndexAndStaticAssets(t *testing.T) {
	app, _ := newTestApp(t)

	for _, path := range []string{"/", "/ui", "/ui/"} {
		recorder := httptest.NewRecorder()
		app.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8787"+path, nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", path, recorder.Code)
		}
		if got := recorder.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
			t.Fatalf("GET %s Content-Type = %q, want text/html", path, got)
		}
		if !strings.Contains(recorder.Body.String(), "<html") {
			t.Fatalf("GET %s did not return the index page", path)
		}
	}

	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8787/static/js/app.js", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /static/js/app.js = %d, want 200", recorder.Code)
	}
}

func TestUIRejectsNonGET(t *testing.T) {
	app, _ := newTestApp(t)
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/", nil))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", recorder.Code)
	}
}

// The health checker must report an unconfigured gateway as unreachable
// rather than probing an empty base URL, and must stop when the context is
// cancelled.
func TestStartHealthCheckReportsUnconfiguredAndReachableUpstreams(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	st, err := store.OpenStore(filepath.Join(t.TempDir(), "proxy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := config.DefaultConfig(filepath.Join(t.TempDir(), "config.json"))
	reachable := cfg.Gateways["st"]
	reachable.BaseURL = upstream.URL
	cfg.Gateways["st"] = reachable
	// "ds" keeps its empty default base URL.
	app := NewApp(cfg, st, slog.Default(), "1.0.0")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		app.StartHealthCheck(ctx, 10*time.Millisecond)
	}()

	// The first pass runs before the ticker, but from a goroutine, so wait
	// for it rather than racing it.
	deadline := time.Now().Add(2 * time.Second)
	var unconfigured upstreamHealth
	for time.Now().Before(deadline) {
		app.healthMu.RLock()
		unconfigured = app.upstreams["ds"]
		app.healthMu.RUnlock()
		if unconfigured.Err != "" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if unconfigured.Reachable || unconfigured.Err == "" {
		t.Fatalf("expected an unconfigured gateway to report an error, got %+v", unconfigured)
	}

	var last map[string]map[string]any
	reachableNow := func() bool {
		entry, ok := last["st"]
		return ok && entry["reachable"] == true
	}
	for time.Now().Before(deadline) {
		recorder := httptest.NewRecorder()
		app.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8787/healthz", nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", recorder.Code)
		}
		var body struct {
			Status    string                    `json:"status"`
			Upstreams map[string]map[string]any `json:"upstreams"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.Status != "ok" {
			t.Fatalf("status = %q, want ok", body.Status)
		}
		last = body.Upstreams
		if reachableNow() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !reachableNow() {
		t.Fatalf("expected the reachable upstream to be reported, got %+v", last)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("StartHealthCheck did not stop when its context was cancelled")
	}
}
