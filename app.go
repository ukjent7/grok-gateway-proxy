package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"
)

type App struct {
	config *Config
	store  *Store
	logger *slog.Logger
	proxy  *Proxy
}

func NewApp(config *Config, store *Store, logger *slog.Logger) *App {
	app := &App{config: config, store: store, logger: logger}
	app.proxy = &Proxy{config: config, store: store, logger: logger, client: newUpstreamClient()}
	return app
}

func newUpstreamClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}}
}

func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
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
			"gateways":    a.config.Snapshot(),
		})
	case path == "/gateways" && r.Method == http.MethodPut:
		a.updateGateways(w, r)
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
	case strings.HasPrefix(path, "/logs/") && r.Method == http.MethodGet:
		id := strings.TrimPrefix(path, "/logs/")
		log, err := a.store.Get(r.Context(), id)
		if err != nil {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeJSON(w, http.StatusOK, log)
	case path == "/setup" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, setupSnippets(a.config.Snapshot()))
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
