package web

import (
	"context"
	"encoding/json"
	"errors"
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

	reclaimMu  sync.Mutex
	reclaiming bool

	apiHandler     http.Handler
	apiHandlerOnce sync.Once

	gatewayHandler     http.Handler
	gatewayHandlerOnce sync.Once
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
	app.apiHandler = app.buildAPIRoutes()
	app.gatewayHandler = sameOriginGuard(http.HandlerFunc(app.proxy.ServeHTTP))
	return app
}

func (a *App) StartHealthCheck(ctx context.Context, interval time.Duration) {
	a.startHealthCheck(ctx, interval)
}

func (a *App) buildAPIRoutes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/config", a.handleGetConfig)
	mux.HandleFunc("PATCH /api/proxy", a.patchProxy)
	mux.HandleFunc("POST /api/gateways", a.handleCreateGateway)
	mux.HandleFunc("PATCH /api/gateways/{id}", a.patchGatewayFromPath)
	mux.HandleFunc("DELETE /api/gateways/{id}", a.handleDeleteGateway)
	mux.HandleFunc("POST /api/gateways/{id}/test", a.handleTestGateway)
	mux.HandleFunc("GET /api/metrics", a.handleMetrics)
	mux.HandleFunc("GET /api/pulse", a.handlePulse)
	mux.HandleFunc("GET /api/logs", a.listLogs)
	mux.HandleFunc("DELETE /api/logs", a.deleteLogs)
	mux.HandleFunc("GET /api/logs/{id}", a.getLogByID)
	mux.HandleFunc("GET /api/setup", a.handleSetup)

	return Chain(mux, sameOriginGuard)
}

func (a *App) handleHealth(w http.ResponseWriter, r *http.Request) {
	a.healthMu.RLock()
	upstreams := make(map[string]any, len(a.upstreams))
	for id, h := range a.upstreams {
		upstreams[id] = healthEntry(h)
	}
	a.healthMu.RUnlock()
	proxy.WriteJSON(w, http.StatusOK, map[string]any{"status": "ok", "upstreams": upstreams})
}

func healthEntry(h upstreamHealth) map[string]any {
	entry := map[string]any{"reachable": h.Reachable, "error": h.Err}
	if h.Status != 0 {
		entry["status"] = h.Status
	}
	if !h.CheckedAt.IsZero() {
		entry["checked_at"] = h.CheckedAt.UTC().Format(time.RFC3339)
	}
	return entry
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

// probeUpstream measures one gateway and caches the answer for /healthz. It
// returns what it recorded so the on-demand endpoint can report this probe
// rather than whatever a concurrent background sweep last wrote.
func (a *App) probeUpstream(id string, gateway config.GatewayConfig) upstreamHealth {
	return a.setHealth(id, a.checkUpstream(gateway))
}

func (a *App) checkUpstream(gateway config.GatewayConfig) upstreamHealth {
	if strings.TrimSpace(gateway.BaseURL) == "" {
		return upstreamHealth{Err: "base URL 未配置"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(gateway.BaseURL, "/")+"/models", nil)
	if err != nil {
		return upstreamHealth{Err: err.Error()}
	}
	req.Header.Set("User-Agent", fmt.Sprintf("grok-gateway-proxy/%s healthz", a.version))
	resp, err := a.proxy.ClientFor(gateway, false).Do(req)
	if err != nil {
		return upstreamHealth{Err: err.Error()}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	// 401/403 mean the proxy reached the upstream but cannot use it without
	// credentials: reporting that as plain reachability would light the
	// dashboard green for a gateway no request can succeed on.
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return upstreamHealth{
			Status: resp.StatusCode,
			Err:    fmt.Sprintf("upstream rejected authentication (HTTP %d)", resp.StatusCode),
		}
	}
	return upstreamHealth{Reachable: resp.StatusCode < 500, Status: resp.StatusCode}
}

// handleTestGateway probes one gateway on demand. The console's "测试连通"
// button needs an answer about the configuration as it stands, and /healthz
// only ever reports what the last background sweep measured — which for a
// freshly saved or edited gateway can be minutes stale, or about a base URL
// that no longer exists.
func (a *App) handleTestGateway(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" || strings.ContainsAny(id, "/") {
		proxy.WriteError(w, http.StatusBadRequest, fmt.Errorf("invalid gateway id"))
		return
	}
	gateway, ok := a.config.Snapshot()[id]
	if !ok {
		proxy.WriteError(w, http.StatusNotFound, fmt.Errorf("unknown gateway %q", id))
		return
	}
	// Deliberately not r.Context(): a client that gives up while the upstream is
	// still thinking would cancel the probe and blank the cached entry the
	// background sweep and the next caller both read.
	health := a.probeUpstream(id, gateway)
	proxy.WriteJSON(w, http.StatusOK, healthEntry(health))
}

func (a *App) setHealth(id string, h upstreamHealth) upstreamHealth {
	h.CheckedAt = time.Now()
	a.healthMu.Lock()
	a.upstreams[id] = h
	a.healthMu.Unlock()
	return h
}

func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	if path == "/healthz" {
		a.handleHealth(w, r)
		return
	}
	if strings.HasPrefix(path, "/api/") || path == "/api" {
		// NewApp builds the handler eagerly; this covers Apps built as bare
		// struct literals. Either way the build must be once-only: concurrent
		// requests would otherwise race on the field.
		a.apiHandlerOnce.Do(func() {
			if a.apiHandler == nil {
				a.apiHandler = a.buildAPIRoutes()
			}
		})
		a.apiHandler.ServeHTTP(w, r)
		return
	}
	if a.routeIsGateway(path) {
		// NewApp builds the handler eagerly; this covers Apps built as bare
		// struct literals. Either way the build must be once-only.
		a.gatewayHandlerOnce.Do(func() {
			if a.gatewayHandler == nil && a.proxy != nil {
				a.gatewayHandler = sameOriginGuard(http.HandlerFunc(a.proxy.ServeHTTP))
			}
		})
		if a.gatewayHandler != nil {
			a.gatewayHandler.ServeHTTP(w, r)
			return
		}
	}
	a.handleUI(w, r)
}

// routeIsGateway reports whether the path targets a configured gateway.
//
// This reads live config rather than config.DefaultGateways: a gateway created
// at runtime would otherwise fall through to the UI handler and 404 behind its
// own prefix. A bare App without a config (tests construct those) falls back to
// the built-in table, so the answer is never "no gateway" by omission. The same
// route table the proxy matches against decides, so the console can never
// dispatch a path the proxy would not serve.
func (a *App) routeIsGateway(path string) bool {
	_, _, ok := a.config.MatchGateway(path)
	return ok
}

// --- API handlers ---

func (a *App) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	proxy.WriteJSON(w, http.StatusOK, map[string]any{
		"listen_addr": a.config.ListenAddr(),
		"proxy_url":   a.config.ProxyURL(),
		"version":     a.version,
		"gateways":    gatewayViews(a.config.Snapshot()),
		// The rules the config layer enforces, so the console validates
		// against them instead of a copy that drifts the moment one changes.
		"gateway_rules": map[string]any{
			"prefix_pattern":    config.CustomGatewayIDPattern(),
			"reserved_prefixes": config.ReservedPrefixes(),
		},
		"session_affinity_modes": config.SessionAffinityModes,
	})
}

// gatewayView is a gateway as the console sees it. `custom` is computed here:
// the built-in list belongs to config, and a second copy of it in JavaScript
// would be wrong the first time a gateway is added to the build.
type gatewayView struct {
	config.GatewayConfig
	Custom bool `json:"custom"`
}

func gatewayViewOf(gateway config.GatewayConfig) gatewayView {
	return gatewayView{
		GatewayConfig: gateway,
		Custom:        !config.IsBuiltinGateway(gateway.ID),
	}
}

func gatewayViews(gateways map[string]config.GatewayConfig) map[string]gatewayView {
	result := make(map[string]gatewayView, len(gateways))
	for id, gateway := range gateways {
		result[id] = gatewayViewOf(gateway)
	}
	return result
}

// handleCreateGateway adds a user-defined gateway. It reuses the standard
// Responses adapter; only the route prefix, the display name and the upstream
// base URL are the caller's to choose.
//
// Every rejection — a taken prefix, a display name whose client model key
// collides with another gateway's — is decided by the config layer under its
// write lock, before anything is written. The handler only maps the error to a
// status: checking here and undoing after a save would leave a window in which
// two concurrent creates both believed they were the only holder of a name,
// and a failed rollback would leave a gateway the caller was told never
// existed.
func (a *App) handleCreateGateway(w http.ResponseWriter, r *http.Request) {
	var request config.NewGateway
	if err := json.NewDecoder(io.LimitReader(r.Body, maxAPIBodySize)).Decode(&request); err != nil {
		proxy.WriteError(w, http.StatusBadRequest, err)
		return
	}
	gateway, err := a.config.AddGateway(request)
	if err != nil {
		proxy.WriteError(w, gatewayStatusError(err), err)
		return
	}
	proxy.WriteJSON(w, http.StatusCreated, map[string]any{"gateway": gatewayViewOf(gateway)})
}

// handleDeleteGateway removes a custom gateway. Built-in gateways are disabled
// instead of deleted, so the request is answered with 400 and the reason.
func (a *App) handleDeleteGateway(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" || strings.ContainsAny(id, "/") {
		proxy.WriteError(w, http.StatusBadRequest, fmt.Errorf("invalid gateway id"))
		return
	}
	if err := a.config.DeleteGateway(id); err != nil {
		proxy.WriteError(w, gatewayStatusError(err), err)
		return
	}
	proxy.WriteJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

// gatewayStatusError maps the config layer's sentinels onto HTTP statuses, so a
// missing gateway reads as 404 and a rejected value as 400 rather than both
// being an opaque 500.
func gatewayStatusError(err error) int {
	switch {
	case errors.Is(err, config.ErrUnknownGateway):
		return http.StatusNotFound
	case errors.Is(err, config.ErrBuiltinGateway):
		// Not a malformed request: the id is well-formed and belongs to a
		// gateway that ships with the build.
		return http.StatusConflict
	default:
		return http.StatusBadRequest
	}
}

func (a *App) handleMetrics(w http.ResponseWriter, r *http.Request) {
	filter, err := parseFilter(r)
	if err != nil {
		proxy.WriteError(w, http.StatusBadRequest, err)
		return
	}
	metrics, err := a.store.Metrics(r.Context(), filter)
	if err != nil {
		proxy.WriteError(w, http.StatusInternalServerError, err)
		return
	}
	proxy.WriteJSON(w, http.StatusOK, metrics)
}

// handlePulse returns the newest few requests for every gateway in one
// response, grouped by gateway id. The overview's ticker board used to ask for
// each gateway separately — one /logs request per card, so a refresh cost grew
// with the number of gateways the page was drawing.
//
// A gateway= filter is dropped rather than honoured: the point of this endpoint
// is every gateway at once, and answering a single-gateway pulse with a
// single gateway's rows would leave the rest of the board stale.
func (a *App) handlePulse(w http.ResponseWriter, r *http.Request) {
	filter, err := parseFilter(r)
	if err != nil {
		proxy.WriteError(w, http.StatusBadRequest, err)
		return
	}
	filter.GatewayID = ""
	recent, err := a.store.RecentByGateway(r.Context(), filter, filter.Limit)
	if err != nil {
		proxy.WriteError(w, http.StatusInternalServerError, err)
		return
	}
	proxy.WriteJSON(w, http.StatusOK, map[string]any{"gateways": recent})
}

func (a *App) handleSetup(w http.ResponseWriter, r *http.Request) {
	proxy.WriteJSON(w, http.StatusOK, setupSnippets(a.config.ListenAddr(), a.config.Snapshot()))
}

func (a *App) getLogByID(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		proxy.WriteError(w, http.StatusBadRequest, fmt.Errorf("missing log id"))
		return
	}
	log, err := a.store.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrLogNotFound) {
			proxy.WriteError(w, http.StatusNotFound, err)
			return
		}
		proxy.WriteError(w, http.StatusInternalServerError, err)
		return
	}
	proxy.WriteJSON(w, http.StatusOK, log)
}

// maxAPIBodySize bounds the body of management API requests. The payloads are
// tiny (a gateway patch is a few hundred bytes); the limit is a courtesy cap,
// not a security control — the proxy path has its own, much larger bound.
const maxAPIBodySize = 1 << 20

func (a *App) patchProxy(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ProxyURL string `json:"proxy_url"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxAPIBodySize)).Decode(&body); err != nil {
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
	if err := json.NewDecoder(io.LimitReader(r.Body, maxAPIBodySize)).Decode(&patch); err != nil {
		proxy.WriteError(w, http.StatusBadRequest, err)
		return
	}
	// A rename can collide on the client model key just like a create; PatchGateway
	// validates the whole candidate table under the config's write lock, so the
	// rejected patch never reaches disk.
	gateway, err := a.config.PatchGateway(id, patch)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, config.ErrUnknownGateway) {
			status = http.StatusNotFound
		}
		proxy.WriteError(w, status, err)
		return
	}
	proxy.WriteJSON(w, http.StatusOK, map[string]any{"gateway": gatewayViewOf(gateway)})
}

func (a *App) listLogs(w http.ResponseWriter, r *http.Request) {
	filter, err := parseFilter(r)
	if err != nil {
		proxy.WriteError(w, http.StatusBadRequest, err)
		return
	}
	logs, err := a.store.List(r.Context(), filter)
	if err != nil {
		proxy.WriteError(w, http.StatusInternalServerError, err)
		return
	}
	// Counted with the same filter the page was selected by, so the console's
	// total cannot describe a different query than the rows under it. Count
	// ignores the page window, so this is the size of the whole match — which is
	// what a pager needs, and not what `len(logs)` would have been.
	total, err := a.store.Count(r.Context(), filter)
	if err != nil {
		proxy.WriteError(w, http.StatusInternalServerError, err)
		return
	}
	proxy.WriteJSON(w, http.StatusOK, map[string]any{"items": logs, "total": total})
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
	proxy.WriteJSON(w, http.StatusOK, map[string]any{"deleted": count})
	// The rows are already gone, so reclaiming their pages is not part of the
	// answer: VACUUM rebuilds the whole file and holds the single pooled
	// connection while it runs, which would stall every audit insert behind
	// this request. Reclaim off the request path instead.
	if count > 0 {
		a.reclaimSpaceAsync()
	}
}

// reclaimSpaceAsync reclaims the pages freed by a delete in the background,
// coalescing concurrent requests: a second caller while one reclaim is already
// in flight is a no-op, because queueing N full rebuilds on a single
// connection is exactly the stall this avoids.
func (a *App) reclaimSpaceAsync() {
	a.reclaimMu.Lock()
	if a.reclaiming {
		a.reclaimMu.Unlock()
		return
	}
	a.reclaiming = true
	a.reclaimMu.Unlock()

	go func() {
		defer func() {
			a.reclaimMu.Lock()
			a.reclaiming = false
			a.reclaimMu.Unlock()
		}()
		// Deliberately not the request context: it is cancelled as soon as the
		// handler returns, which is before this goroutine gets to run.
		if err := a.store.ReclaimSpace(context.Background()); err != nil && a.logger != nil {
			a.logger.Warn("reclaiming space after delete logs failed", "error", err)
		}
	}()
}
