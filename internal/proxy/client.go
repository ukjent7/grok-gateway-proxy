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

// streamResponseHeaderTimeout bounds how long a streaming request waits for
// the upstream's first response header. Streaming upstreams send headers as
// soon as the request is accepted, so a long wait here means a hung upstream,
// not a slow model; without this bound a stream that never starts would hold
// its goroutine until the client disconnects.
const streamResponseHeaderTimeout = 120 * time.Second

// NewUpstreamClient builds an HTTP client for streaming upstream calls,
// optionally configured to route through a proxy URL. For non-streaming
// requests use newSyncUpstreamClient instead: a transport-level header cap
// below that request's context deadline would silently shrink the budget.
func NewUpstreamClient(proxyURL string) *http.Client {
	return newUpstreamClient(proxyURL, streamResponseHeaderTimeout)
}

// newSyncUpstreamClient builds the client used for non-streaming requests.
// Those carry their own context deadline (the configured upstream timeout, up
// to maxUpstreamTimeout via X-Proxy-Timeout), so no separate header timeout
// applies — and one must not: a non-streaming upstream sends its headers only
// once the full response is computed, so a reasoning model can legitimately
// take longer than any fixed header cap.
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

// NewProxy creates a Proxy with configured-proxy and direct HTTP clients for
// each request mode. Streaming and non-streaming requests use separate
// transports because their header-wait bounds differ (see
// streamResponseHeaderTimeout); separate proxy/direct clients keep their
// connection pools isolated.
func NewProxy(cfg *config.Config, st *store.Store, logger *slog.Logger) *Proxy {
	return &Proxy{
		Config:             cfg,
		Store:              st,
		Logger:             logger,
		Client:             newSyncUpstreamClient(cfg.ProxyURL()),
		DirectClient:       newSyncUpstreamClient(""),
		StreamClient:       NewUpstreamClient(cfg.ProxyURL()),
		StreamDirectClient: NewUpstreamClient(""),
	}
}
