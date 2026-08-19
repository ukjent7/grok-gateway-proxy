package web

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"grok-gateway-proxy/internal/config"
	"grok-gateway-proxy/internal/proxy"
)

// The middleware chain must recover from panics and return 500 instead of
// crashing the test process.
func TestRecoverMiddlewareCatchesPanic(t *testing.T) {
	panicking := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})
	handler := Chain(panicking, RecoverMiddleware(slog.Default()))

	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["error"] == nil {
		t.Fatalf("expected error in response body, got %s", rec.Body.String())
	}
}

// Non-panicking handlers must pass through untouched.
func TestRecoverMiddlewarePassthrough(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	handler := Chain(ok, RecoverMiddleware(slog.Default()))

	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Fatalf("expected 200 ok, got %d %s", rec.Code, rec.Body.String())
	}
}

// SecurityHeadersMiddleware must set both headers on every response.
func TestSecurityHeadersMiddlewareSetsHeaders(t *testing.T) {
	handler := Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), SecurityHeadersMiddleware)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("missing X-Content-Type-Options: %v", rec.Header())
	}
	if rec.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("missing Referrer-Policy: %v", rec.Header())
	}
}

// The full chain (recover → security headers → app) must set security headers
// even when the inner handler returns normally.
func TestFullChainSetsSecurityHeadersOnNormalResponse(t *testing.T) {
	cfg := config.DefaultConfig(filepath.Join(t.TempDir(), "config.json"))
	app := &App{config: cfg, logger: slog.Default()}
	handler := Chain(app, RecoverMiddleware(slog.Default()), SecurityHeadersMiddleware)

	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8787/api/config", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("security header missing through full chain")
	}
	if rec.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatal("referrer policy missing through full chain")
	}
}

// The full chain must recover from a panic inside ServeHTTP and still return
// the security headers alongside the 500 error.
func TestFullChainRecoversPanicWithSecurityHeaders(t *testing.T) {
	cfg := config.DefaultConfig(filepath.Join(t.TempDir(), "config.json"))
	app := &App{config: cfg, logger: slog.Default()}
	// Wrap with a panicking middleware to simulate a handler panic.
	panickingApp := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/config") {
			panic("simulated crash")
		}
		app.ServeHTTP(w, r)
	})
	handler := Chain(panickingApp, RecoverMiddleware(slog.Default()), SecurityHeadersMiddleware)

	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8787/api/config", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rec.Code)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("security header missing on panic recovery")
	}
}

// The chain ordering must be correct: the first middleware listed is
// outermost, so it wraps all subsequent layers.
func TestChainOrdering(t *testing.T) {
	var order []string
	mw := func(name string) Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name+"-pre")
				next.ServeHTTP(w, r)
				order = append(order, name+"-post")
			})
		}
	}
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		order = append(order, "inner")
		w.WriteHeader(http.StatusOK)
	})
	handler := Chain(inner, mw("A"), mw("B"))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	want := []string{"A-pre", "B-pre", "inner", "B-post", "A-post"}
	if len(order) != len(want) {
		t.Fatalf("expected %d entries, got %v", len(want), order)
	}
	for i, w := range want {
		if order[i] != w {
			t.Fatalf("position %d: got %q, want %q (full: %v)", i, order[i], w, order)
		}
	}
}

// Concurrent requests through the full chain must all get security headers
// without races or panics.
func TestFullChainConcurrentSafety(t *testing.T) {
	cfg := config.DefaultConfig(filepath.Join(t.TempDir(), "config.json"))
	p := proxy.NewProxy(cfg, nil, slog.Default())
	p.Client = &http.Client{}
	app := &App{config: cfg, logger: slog.Default(), proxy: p}
	handler := Chain(app, RecoverMiddleware(slog.Default()), SecurityHeadersMiddleware)

	const n = 20
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8787/healthz", nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Errorf("expected 200, got %d", rec.Code)
			}
			if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Error("missing security header in concurrent request")
			}
		}()
	}
	wg.Wait()
}
