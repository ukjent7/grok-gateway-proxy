package proxy

import (
	"crypto/tls"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"grok-gateway-proxy/internal/config"
	"grok-gateway-proxy/internal/store"
)

// NewUpstreamClient builds an HTTP client for upstream calls, optionally
// configured to route through a proxy URL.
func NewUpstreamClient(proxyURL string) *http.Client {
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

// NewProxy creates a Proxy with the configured and direct HTTP clients.
func NewProxy(cfg *config.Config, st *store.Store, logger *slog.Logger) *Proxy {
	return &Proxy{
		Config:       cfg,
		Store:        st,
		Logger:       logger,
		Client:       NewUpstreamClient(cfg.ProxyURL()),
		DirectClient: NewUpstreamClient(""),
	}
}

// WriteJSON encodes value as JSON and writes it with the given status.
func WriteJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// WriteError writes a structured error response.
func WriteError(w http.ResponseWriter, status int, err error) {
	WriteJSON(w, status, map[string]any{
		"error": map[string]any{
			"type":    "proxy_error",
			"message": err.Error(),
		},
	})
}
