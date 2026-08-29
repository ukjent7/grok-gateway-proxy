package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"grok-gateway-proxy/internal/config"
	"grok-gateway-proxy/internal/proxy"
	"grok-gateway-proxy/internal/store"
)

// upstreamHealth records the last reachability probe for a gateway.
type upstreamHealth struct {
	Reachable bool
	Status    int
	Err       string
	CheckedAt time.Time
}

type App struct {
	config  *config.Config
	store   *store.Store
	logger  *slog.Logger
	proxy   *proxy.Proxy
	version string

	healthMu  sync.RWMutex
	upstreams map[string]upstreamHealth

	apiMux *http.ServeMux
}

func NewApp(cfg *config.Config, st *store.Store, logger *slog.Logger, version string) *App {
	app := &App{
		config:    cfg,
		store:     st,
		logger:    logger,
		version:   version,
		proxy:     proxy.NewProxy(cfg, st, logger),
		upstreams: make(map[string]upstreamHealth),
	}
	app.apiMux = app.buildAPIRoutes()
	return app
}

// StartHealthCheck exposes the background health checker for main to launch.
func (a *App) StartHealthCheck(ctx context.Context, interval time.Duration) {
	a.startHealthCheck(ctx, interval)
}

// buildAPIRoutes registers all management API routes on a ServeMux using
// Go 1.22+ method+pattern matching. Returns the configured mux.
func (a *App) buildAPIRoutes() *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/config", a.handleGetConfig)
	mux.HandleFunc("PATCH /api/proxy", a.patchProxy)
	mux.HandleFunc("PATCH /api/gateways/{id}", a.patchGatewayFromPath)
	mux.HandleFunc("GET /api/metrics", a.handleMetrics)
	mux.HandleFunc("GET /api/logs", a.listLogs)
	mux.HandleFunc("DELETE /api/logs", a.deleteLogs)
	mux.HandleFunc("GET /api/logs/count", a.countLogs)
	mux.HandleFunc("GET /api/logs/{id}", a.getLogByID)
	mux.HandleFunc("GET /api/setup", a.handleSetup)

	return mux
}

// handleHealth reports liveness plus the most recent reachability probe for
// every configured gateway. Probes run in the background; this handler only
// reads the cached result so it stays fast and never blocks on upstreams.
func (a *App) handleHealth(w http.ResponseWriter, r *http.Request) {
	a.healthMu.RLock()
	upstreams := make(map[string]any, len(a.upstreams))
	for id, h := range a.upstreams {
		entry := map[string]any{"reachable": h.Reachable, "error": h.Err}
		if h.Status != 0 {
			entry["status"] = h.Status
		}
		if !h.CheckedAt.IsZero() {
			entry["checked_at"] = h.CheckedAt.UTC().Format(time.RFC3339)
		}
		upstreams[id] = entry
	}
	a.healthMu.RUnlock()
	proxy.WriteJSON(w, http.StatusOK, map[string]any{"status": "ok", "upstreams": upstreams})
}

// startHealthCheck probes each enabled gateway's /models endpoint in the
// background and caches the result for /healthz. It runs immediately, then on
// a fixed interval, and stops when ctx is canceled.
func (a *App) startHealthCheck(ctx context.Context, interval time.Duration) {
	checkAll := func() {
		for id, gateway := range a.config.Snapshot() {
			if !gateway.Enabled {
				continue
			}
			a.probeUpstream(id, gateway)
		}
	}
	checkAll()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			checkAll()
		}
	}
}

func (a *App) probeUpstream(id string, gateway config.GatewayConfig) {
	if strings.TrimSpace(gateway.BaseURL) == "" {
		a.setHealth(id, upstreamHealth{Err: "base URL 未配置"})
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(gateway.BaseURL, "/")+"/models", nil)
	if err != nil {
		a.setHealth(id, upstreamHealth{Err: err.Error()})
		return
	}
	req.Header.Set("User-Agent", "grok-gateway-proxy/healthz")
	resp, err := a.proxy.ClientFor(gateway).Do(req)
	if err != nil {
		a.setHealth(id, upstreamHealth{Err: err.Error()})
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	a.setHealth(id, upstreamHealth{Reachable: resp.StatusCode < 500, Status: resp.StatusCode})
}

func (a *App) setHealth(id string, h upstreamHealth) {
	h.CheckedAt = time.Now()
	a.healthMu.Lock()
	a.upstreams[id] = h
	a.healthMu.Unlock()
}

func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	if path == "/healthz" {
		a.handleHealth(w, r)
		return
	}
	if strings.HasPrefix(path, "/api/") || path == "/api" {
		if a.apiMux == nil {
			a.apiMux = a.buildAPIRoutes()
		}
		a.apiMux.ServeHTTP(w, r)
		return
	}
	if isGatewayPath(path) {
		a.proxy.ServeHTTP(w, r)
		return
	}
	a.handleUI(w, r)
}

// isGatewayPath reports whether the path targets a known gateway prefix.
// Uses exact prefix matching (not string HasPrefix) so /static/ is never
// mistaken for /st/.
func isGatewayPath(path string) bool {
	for _, prefix := range []string{"/ds", "/st", "/std"} {
		if hasPathPrefix(path, prefix) {
			return true
		}
	}
	return false
}

func hasPathPrefix(path, prefix string) bool {
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}

// --- API handlers ---

func (a *App) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	proxy.WriteJSON(w, http.StatusOK, map[string]any{
		"listen_addr": a.config.ListenAddr,
		"proxy_url":   a.config.ProxyURL(),
		"version":     a.version,
		"gateways":    a.config.Snapshot(),
	})
}

func (a *App) handleMetrics(w http.ResponseWriter, r *http.Request) {
	filter := parseFilter(r)
	metrics, err := a.store.Metrics(r.Context(), filter)
	if err != nil {
		proxy.WriteError(w, http.StatusInternalServerError, err)
		return
	}
	proxy.WriteJSON(w, http.StatusOK, metrics)
}

func (a *App) handleSetup(w http.ResponseWriter, r *http.Request) {
	proxy.WriteJSON(w, http.StatusOK, setupSnippets(a.config.ListenAddr, a.config.Snapshot()))
}

func (a *App) getLogByID(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		proxy.WriteError(w, http.StatusBadRequest, fmt.Errorf("missing log id"))
		return
	}
	log, err := a.store.Get(r.Context(), id)
	if err != nil {
		proxy.WriteError(w, http.StatusNotFound, err)
		return
	}
	proxy.WriteJSON(w, http.StatusOK, log)
}

func (a *App) patchProxy(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ProxyURL string `json:"proxy_url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		proxy.WriteError(w, http.StatusBadRequest, err)
		return
	}
	if err := a.config.SetProxyURL(body.ProxyURL); err != nil {
		proxy.WriteError(w, http.StatusBadRequest, err)
		return
	}
	if a.proxy != nil {
		a.proxy.SetProxyURL(a.config.ProxyURL())
	}
	proxy.WriteJSON(w, http.StatusOK, map[string]any{"proxy_url": a.config.ProxyURL()})
}

// patchGatewayFromPath extracts the gateway id from the route pattern and
// delegates to patchGateway.
func (a *App) patchGatewayFromPath(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a.patchGateway(w, r, id)
}

// patchGateway applies a partial update to a single gateway. Only the
// provided fields change; everything else is preserved.
func (a *App) patchGateway(w http.ResponseWriter, r *http.Request, id string) {
	if id == "" || strings.ContainsAny(id, "/") {
		proxy.WriteError(w, http.StatusBadRequest, fmt.Errorf("invalid gateway id"))
		return
	}
	var patch config.GatewayPatch
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		proxy.WriteError(w, http.StatusBadRequest, err)
		return
	}
	gateway, err := a.config.PatchGateway(id, patch)
	if err != nil {
		status := http.StatusBadRequest
		if strings.HasPrefix(err.Error(), "unknown gateway") {
			status = http.StatusNotFound
		}
		proxy.WriteError(w, status, err)
		return
	}
	proxy.WriteJSON(w, http.StatusOK, map[string]any{"gateway": gateway})
}

func (a *App) listLogs(w http.ResponseWriter, r *http.Request) {
	logs, err := a.store.List(r.Context(), parseFilter(r))
	if err != nil {
		proxy.WriteError(w, http.StatusInternalServerError, err)
		return
	}
	proxy.WriteJSON(w, http.StatusOK, map[string]any{"items": logs})
}

func (a *App) deleteLogs(w http.ResponseWriter, r *http.Request) {
	var before *time.Time
	if raw := r.URL.Query().Get("before"); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			proxy.WriteError(w, http.StatusBadRequest, err)
			return
		}
		before = &parsed
	}
	count, err := a.store.Delete(r.Context(), before)
	if err != nil {
		proxy.WriteError(w, http.StatusInternalServerError, err)
		return
	}
	if count > 0 && a.store != nil {
		if err := a.store.Vacuum(r.Context()); err != nil && a.logger != nil {
			a.logger.Warn("VACUUM failed after delete logs", "error", err)
		}
		if err := a.store.CheckpointWAL(r.Context()); err != nil && a.logger != nil {
			a.logger.Warn("WAL checkpoint failed after delete logs", "error", err)
		}
	}
	proxy.WriteJSON(w, http.StatusOK, map[string]any{"deleted": count})
}

// countLogs reports how many logs match the same filters as /api/logs,
// letting the dashboard render total counts without fetching every row.
func (a *App) countLogs(w http.ResponseWriter, r *http.Request) {
	n, err := a.store.Count(r.Context(), parseFilter(r))
	if err != nil {
		proxy.WriteError(w, http.StatusInternalServerError, err)
		return
	}
	proxy.WriteJSON(w, http.StatusOK, map[string]any{"count": n})
}
