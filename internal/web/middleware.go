package web

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"grok-gateway-proxy/internal/proxy"
)

// Middleware wraps an http.Handler with cross-cutting concerns.
type Middleware func(http.Handler) http.Handler

// Chain composes middlewares so the first listed runs outermost.
func Chain(h http.Handler, mws ...Middleware) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

// RecoverMiddleware catches panics from any handler, logs them, and returns
// a 500 instead of crashing the process.
func RecoverMiddleware(logger *slog.Logger) Middleware {
	if logger == nil {
		logger = slog.Default()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					logger.Error("panic recovered",
						"error", rec,
						"path", r.URL.Path,
						"method", r.Method,
					)
					proxy.WriteError(w, http.StatusInternalServerError, fmt.Errorf("internal error"))
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// SecurityHeadersMiddleware sets baseline response headers on every route.
func SecurityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

// sameOriginGuard is browser-CSRF mitigation, not gateway authentication.
// It blocks cross-site POST/PATCH/DELETE to /api/* when the browser sends
// Origin or Sec-Fetch-Site. Non-browser clients (Grok Build, curl) send
// neither header and therefore pass — do not rely on this for authorization.
func sameOriginGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		if isCrossSite(r) {
			proxy.WriteError(w, http.StatusForbidden, fmt.Errorf("cross-site requests to the management API are not allowed"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isCrossSite(r *http.Request) bool {
	if origin := r.Header.Get("Origin"); origin != "" {
		parsed, err := url.Parse(origin)
		if err != nil || parsed.Host != r.Host {
			return true
		}
	}
	switch strings.ToLower(r.Header.Get("Sec-Fetch-Site")) {
	case "cross-site", "cross-origin":
		return true
	}
	return false
}
