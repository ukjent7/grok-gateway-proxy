package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// upstreamHealth records the last reachability probe for a gateway.
type upstreamHealth struct {
	Reachable bool
	Status    int
	Err       string
	CheckedAt time.Time
}

type App struct {
	config *Config
	store  *Store
	logger *slog.Logger
	proxy  *Proxy

	healthMu  sync.RWMutex
	upstreams map[string]upstreamHealth
}

func NewApp(config *Config, store *Store, logger *slog.Logger) *App {
	app := &App{
		config: config,
		store:  store,
		logger: logger,
		proxy: &Proxy{
			config:       config,
			store:        store,
			logger:       logger,
			client:       newUpstreamClient(config.ProxyURL),
			directClient: newUpstreamClient(""),
		},
		upstreams: make(map[string]upstreamHealth),
	}
	return app
}

func newUpstreamClient(proxyURL string) *http.Client {
	transport := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   50,
		IdleConnTimeout:       90 * time.Second,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
	}
	if strings.TrimSpace(proxyURL) != "" {
		if parsed, err := url.Parse(proxyURL); err == nil {
			transport.Proxy = http.ProxyURL(parsed)
		}
	}
	return &http.Client{Transport: transport}
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
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "upstreams": upstreams})
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

func (a *App) probeUpstream(id string, gateway GatewayConfig) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(gateway.BaseURL, "/")+"/models", nil)
	if err != nil {
		a.setHealth(id, upstreamHealth{Err: err.Error()})
		return
	}
	req.Header.Set("User-Agent", "grok-gateway-proxy/healthz")
	resp, err := a.proxy.clientFor(gateway).Do(req)
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
	if r.URL.Path == "/healthz" {
		a.handleHealth(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/api" {
		a.handleAPI(w, r)
		return
	}
	if hasPathPrefix(r.URL.Path, "/oc") || hasPathPrefix(r.URL.Path, "/st") || hasPathPrefix(r.URL.Path, "/ve") {
		a.proxy.ServeHTTP(w, r)
		return
	}
	a.handleUI(w, r)
}

func hasPathPrefix(path, prefix string) bool {
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}

func (a *App) handleAPI(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api")
	switch {
	case path == "/config" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{
			"listen_addr": a.config.ListenAddr,
			"proxy_url":   a.config.ProxyURL,
			"version":     version,
			"gateways":    a.config.Snapshot(),
		})
	case path == "/proxy" && r.Method == http.MethodPatch:
		a.patchProxy(w, r)
	case path == "/gateways" && r.Method == http.MethodPut:
		a.updateGateways(w, r)
	case strings.HasPrefix(path, "/gateways/") && r.Method == http.MethodPatch:
		a.patchGateway(w, r, strings.TrimPrefix(path, "/gateways/"))
	case path == "/metrics" && r.Method == http.MethodGet:
		filter := parseFilter(r)
		metrics, err := a.store.Metrics(r.Context(), filter)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, metrics)
	case path == "/logs" && r.Method == http.MethodGet:
		a.listLogs(w, r)
	case path == "/logs" && r.Method == http.MethodDelete:
		a.deleteLogs(w, r)
	case path == "/logs/count" && r.Method == http.MethodGet:
		a.countLogs(w, r)
	case strings.HasPrefix(path, "/logs/") && r.Method == http.MethodGet:
		id := strings.TrimPrefix(path, "/logs/")
		log, err := a.store.Get(r.Context(), id)
		if err != nil {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeJSON(w, http.StatusOK, log)
	case path == "/setup" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, setupSnippets(a.config.ListenAddr, a.config.Snapshot()))
	default:
		writeError(w, http.StatusNotFound, fmt.Errorf("unknown management endpoint %s", r.URL.Path))
	}
}

func (a *App) updateGateways(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Gateways map[string]GatewayConfig `json:"gateways"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := a.config.UpdateGateways(body.Gateways); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"gateways": a.config.Snapshot()})
}

func (a *App) patchProxy(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ProxyURL string `json:"proxy_url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := a.config.SetProxyURL(body.ProxyURL); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if a.proxy != nil {
		a.proxy.SetProxyURL(a.config.ProxyURL)
	}
	writeJSON(w, http.StatusOK, map[string]any{"proxy_url": a.config.ProxyURL})
}

// patchGateway applies a partial update to a single gateway. Only the
// provided fields change; everything else is preserved.
func (a *App) patchGateway(w http.ResponseWriter, r *http.Request, id string) {
	if id == "" || strings.ContainsAny(id, "/") {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid gateway id"))
		return
	}
	var patch GatewayPatch
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	gateway, err := a.config.PatchGateway(id, patch)
	if err != nil {
		status := http.StatusBadRequest
		if strings.HasPrefix(err.Error(), "unknown gateway") {
			status = http.StatusNotFound
		}
		writeError(w, status, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"gateway": gateway})
}

func (a *App) listLogs(w http.ResponseWriter, r *http.Request) {
	logs, err := a.store.List(r.Context(), parseFilter(r))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": logs})
}

func (a *App) deleteLogs(w http.ResponseWriter, r *http.Request) {
	var before *time.Time
	if raw := r.URL.Query().Get("before"); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		before = &parsed
	}
	count, err := a.store.Delete(r.Context(), before)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": count})
}

// countLogs reports how many logs match the same filters as /api/logs,
// letting the dashboard render total counts without fetching every row.
func (a *App) countLogs(w http.ResponseWriter, r *http.Request) {
	n, err := a.store.Count(r.Context(), parseFilter(r))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"count": n})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"type":    "proxy_error",
			"message": err.Error(),
		},
	})
}
