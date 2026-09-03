package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"grok-gateway-proxy/internal/config"
	"grok-gateway-proxy/internal/proxy"
	"grok-gateway-proxy/internal/store"
)

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
	mux.HandleFunc("GET /api/metrics/timeseries", a.handleMetricsTimeseries)
	mux.HandleFunc("GET /api/events", a.handleEvents)
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

func (a *App) probeUpstream(id string, gateway config.GatewayConfig) upstreamHealth {
	return a.setHealth(id, a.checkUpstream(gateway))
}

func (a *App) checkUpstream(gateway config.GatewayConfig) upstreamHealth {
	if strings.TrimSpace(gateway.BaseURL) == "" {
		return upstreamHealth{Err: "base URL 未配置"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	healthPath := "/models"

	if gateway.Protocol == config.ProtocolAnthropic {
		baseTrimmed := strings.TrimRight(gateway.BaseURL, "/")
		if strings.HasSuffix(baseTrimmed, "/v1") {
			healthPath = "/messages"
		} else {
			healthPath = "/v1/messages"
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(gateway.BaseURL, "/")+healthPath, nil)
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

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return upstreamHealth{
			Status: resp.StatusCode,
			Err:    fmt.Sprintf("upstream rejected authentication (HTTP %d)", resp.StatusCode),
		}
	}
	return upstreamHealth{Reachable: resp.StatusCode < 500, Status: resp.StatusCode}
}

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

		a.apiHandlerOnce.Do(func() {
			if a.apiHandler == nil {
				a.apiHandler = a.buildAPIRoutes()
			}
		})
		a.apiHandler.ServeHTTP(w, r)
		return
	}
	if a.routeIsGateway(path) {

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

func (a *App) routeIsGateway(path string) bool {
	_, _, ok := a.config.MatchGateway(path)
	return ok
}

func (a *App) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	proxy.WriteJSON(w, http.StatusOK, map[string]any{
		"listen_addr": a.config.ListenAddr(),
		"proxy_url":   a.config.ProxyURL(),
		"version":     a.version,
		"gateways":    gatewayViews(a.config.Snapshot()),

		"gateway_rules": map[string]any{
			"prefix_pattern":    config.CustomGatewayIDPattern(),
			"reserved_prefixes": config.ReservedPrefixes(),
		},
		"session_affinity_modes": config.SessionAffinityModes,
	})
}

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
	a.store.NotifyChange()
	proxy.WriteJSON(w, http.StatusCreated, map[string]any{"gateway": gatewayViewOf(gateway)})
}

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
	a.store.NotifyChange()
	proxy.WriteJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

func gatewayStatusError(err error) int {
	switch {
	case errors.Is(err, config.ErrUnknownGateway):
		return http.StatusNotFound
	case errors.Is(err, config.ErrBuiltinGateway):

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

func (a *App) handleMetricsTimeseries(w http.ResponseWriter, r *http.Request) {
	filter, err := parseFilter(r)
	if err != nil {
		proxy.WriteError(w, http.StatusBadRequest, err)
		return
	}
	buckets := 48
	if value, err := strconv.Atoi(r.URL.Query().Get("buckets")); err == nil && value > 0 {
		buckets = min(value, 240)
	}
	series, err := a.store.TimeSeries(r.Context(), filter, buckets)
	if err != nil {
		proxy.WriteError(w, http.StatusInternalServerError, err)
		return
	}
	proxy.WriteJSON(w, http.StatusOK, series)
}

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

func (a *App) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		proxy.WriteError(w, http.StatusInternalServerError, fmt.Errorf("streaming unsupported"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")

	subID, changes := a.store.SubscribeChanges()
	defer a.store.UnsubscribeChanges(subID)

	if _, err := fmt.Fprint(w, "event: change\ndata: {}\n\n"); err == nil {
		flusher.Flush()
	}
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case _, ok := <-changes:
			if !ok {
				return
			}
			if _, err := fmt.Fprint(w, "event: change\ndata: {}\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
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
	a.store.NotifyChange()
	proxy.WriteJSON(w, http.StatusOK, map[string]any{"proxy_url": a.config.ProxyURL()})
}

func (a *App) patchGatewayFromPath(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a.patchGateway(w, r, id)
}

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

	gateway, err := a.config.PatchGateway(id, patch)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, config.ErrUnknownGateway) {
			status = http.StatusNotFound
		}
		proxy.WriteError(w, status, err)
		return
	}
	a.store.NotifyChange()
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

	if count > 0 {
		a.reclaimSpaceAsync()
	}
}

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

		if err := a.store.ReclaimSpace(context.Background()); err != nil && a.logger != nil {
			a.logger.Warn("reclaiming space after delete logs failed", "error", err)
		}
	}()
}
