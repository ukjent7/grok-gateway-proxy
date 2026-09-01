package proxy

import (
	"crypto/tls"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"grok-gateway-proxy/internal/config"
	"grok-gateway-proxy/internal/store"
)

const streamResponseHeaderTimeout = 120 * time.Second

func NewUpstreamClient(proxyURL string) *http.Client {
	return newUpstreamClient(proxyURL, streamResponseHeaderTimeout)
}

func newSyncUpstreamClient(proxyURL string) *http.Client {
	return newUpstreamClient(proxyURL, 0)
}

func newUpstreamClient(proxyURL string, responseHeaderTimeout time.Duration) *http.Client {
	transport := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: responseHeaderTimeout,
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

func NewProxy(cfg *config.Config, st *store.Store, logger *slog.Logger) *Proxy {
	syncProxy, syncDirect, streamProxy, streamDirect := buildUpstreamClients(cfg.ProxyURL())
	return &Proxy{
		Config:             cfg,
		Store:              st,
		Logger:             logger,
		Client:             syncProxy,
		DirectClient:       syncDirect,
		StreamClient:       streamProxy,
		StreamDirectClient: streamDirect,
		bodies:             newBodyAdmission(inFlightBodyBudget),
	}
}
