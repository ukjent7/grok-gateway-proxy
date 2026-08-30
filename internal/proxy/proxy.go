package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"grok-gateway-proxy/internal/config"
	"grok-gateway-proxy/internal/store"
)

const (
	// maxRequestBodySize bounds the audit-captured request body. Requests
	// larger than this are rejected with 413 before any upstream call.
	maxRequestBodySize int64 = 64 << 20

	// defaultResponseBodySize is the fallback when no configurable limit is
	// set. Individual deployments can override via config.BodyCaptureLimitKB.
	defaultResponseBodySize int64 = 256 << 10

	// maxUpstreamTimeout caps the per-request timeout that a client may
	// request via the X-Proxy-Timeout header.
	maxUpstreamTimeout = 30 * time.Minute

	// maxCapturedBodySize is the safety ceiling for a single captured column
	// when body_capture_limit_kb is 0 ("capture everything"). Opting out of
	// truncation must not mean unbounded: the request side is already capped
	// by maxRequestBodySize, and without a matching ceiling here one large
	// upstream response would land in SQLite in full. This mirrors the
	// request-side limit so both directions are bounded the same way.
	maxCapturedBodySize int64 = 64 << 20
)

type Proxy struct {
	Config *config.Config
	Store  *store.Store
	Logger *slog.Logger
	// Client/DirectClient serve non-streaming requests (no transport-level
	// header cap — the request context bounds them). StreamClient and
	// StreamDirectClient serve streaming requests (capped header wait against
	// hung upstreams). Tests may set only Client/DirectClient; ClientFor falls
	// back to them when the streaming pair is absent.
	Client             *http.Client
	DirectClient       *http.Client
	StreamClient       *http.Client
	StreamDirectClient *http.Client
	clientMu           sync.RWMutex
	ResponseBodySize   int64 // override body capture limit (0 = use config or default)
}

// statusError carries an HTTP status code alongside the error message.
type statusError struct {
	status  int
	message string
}

func (e *statusError) Error() string { return e.message }

func statusOf(err error) int {
	var se *statusError
	if ok := errors.As(err, &se); ok {
		return se.status
	}
	return http.StatusBadGateway
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	bodyLimit := p.responseBodyLimit()
	capture := newResponseCapture(w, bodyLimit)
	w = capture

	logEntry := p.newAuditLog(r, started)
	w.Header().Set("X-Request-Id", logEntry.ID)
	defer p.finalizeLog(r.Context(), &logEntry, capture, started)

	requestBody, ok := p.readRequestBody(w, r, &logEntry, bodyLimit)
	if !ok {
		return
	}

	gateway, subpath, adapter, resolveErr := p.resolveGateway(r, requestBody)
	p.populateLogGateway(&logEntry, gateway, subpath, adapter)
	if resolveErr != nil {
		logEntry.Error = resolveErr.Error()
		logEntry.StatusCode = statusOf(resolveErr)
		WriteError(w, logEntry.StatusCode, resolveErr)
		return
	}

	upstreamCtx, cancel := p.upstreamContext(r, logEntry.Stream)
	defer cancel()

	upstreamRequest, err := p.buildUpstreamRequest(upstreamCtx, r, gateway, subpath, adapter, requestBody, &logEntry)
	if err != nil {
		logEntry.Error = err.Error()
		logEntry.StatusCode = statusOf(err)
		WriteError(w, logEntry.StatusCode, err)
		return
	}

	if err := p.forwardUpstreamResponse(w, &logEntry, adapter, gateway, p.ClientFor(gateway, logEntry.Stream), upstreamRequest, bodyLimit); err != nil {
		logEntry.Error = err.Error()
		if capture.statusCode == 0 {
			logEntry.StatusCode = statusOf(err)
			WriteError(w, logEntry.StatusCode, err)
		}
	}
}

func (p *Proxy) newAuditLog(r *http.Request, started time.Time) store.RequestLog {
	return store.RequestLog{
		ID:             newRequestID(),
		StartedAt:      started,
		Method:         r.Method,
		RequestPath:    r.URL.Path,
		RequestURL:     r.URL.RequestURI(),
		RequestHeaders: headersJSON(r.Header),
	}
}

func (p *Proxy) finalizeLog(ctx context.Context, logEntry *store.RequestLog, capture *responseCapture, started time.Time) {
	logEntry.ClientResponseStatusCode = capture.statusCode
	if logEntry.StatusCode == 0 && capture.statusCode != 0 {
		logEntry.StatusCode = capture.statusCode
	}
	if logEntry.StatusCode == 0 {
		logEntry.StatusCode = http.StatusInternalServerError
	}
	logEntry.ResponseHeaders = headersJSON(capture.headers)
	logEntry.ResponseBody = append([]byte(nil), capture.body.Bytes()...)
	logEntry.ResponseTruncated = logEntry.ResponseTruncated || capture.body.truncated
	p.finishLog(ctx, logEntry, started)
}

func (p *Proxy) readRequestBody(w http.ResponseWriter, r *http.Request, logEntry *store.RequestLog, bodyLimit int64) ([]byte, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBodySize+1))
	if err != nil {
		// The other error paths record what went wrong; a read failure must
		// not leave an audit row that only shows a bare 400.
		logEntry.Error = err.Error()
		logEntry.StatusCode = http.StatusBadRequest
		WriteError(w, http.StatusBadRequest, err)
		return nil, false
	}
	if int64(len(body)) > maxRequestBodySize {
		err := fmt.Errorf("request body exceeds %d bytes", maxRequestBodySize)
		logEntry.Error = err.Error()
		logEntry.StatusCode = http.StatusRequestEntityTooLarge
		WriteError(w, http.StatusRequestEntityTooLarge, err)
		return nil, false
	}
	logEntry.RequestBody = capBody(body, bodyLimit)
	logEntry.Model = ParseModel(body)
	logEntry.Stream = requestStream(body)
	return body, true
}

func (p *Proxy) populateLogGateway(logEntry *store.RequestLog, gateway config.GatewayConfig, subpath string, adapter GatewayAdapter) {
	if gateway.ID != "" {
		logEntry.GatewayID = gateway.ID
		logEntry.GatewayName = gateway.Name
		logEntry.Prefix = gateway.Prefix
		logEntry.UpstreamProtocol = gateway.Protocol
	}
	if adapter != nil {
		logEntry.IngressProtocol = protocolForPath(subpath)
	}
}

func (p *Proxy) upstreamContext(r *http.Request, stream bool) (context.Context, context.CancelFunc) {
	if stream {
		// Streams deliberately get no hard deadline: reasoning models can run
		// far past the non-streaming timeout. What bounds a stalled upstream is
		// the request context cancelling on client disconnect, plus the
		// transport's ResponseHeaderTimeout for an upstream that never sends
		// headers at all. A mid-stream stall with the client still attached is
		// not bounded, so a hung upstream keeps its goroutine until the client
		// goes away.
		return r.Context(), func() {}
	}
	return context.WithTimeout(r.Context(), p.upstreamTimeout(r))
}

// resolveGateway maps the inbound path to a gateway + adapter and validates
// the request (method, subpath, body). Returns a *statusError on failure.
func (p *Proxy) resolveGateway(r *http.Request, body []byte) (config.GatewayConfig, string, GatewayAdapter, error) {
	gateway, subpath, ok := p.gatewayForPath(r.URL.Path)
	if !ok {
		return config.GatewayConfig{}, "", nil, &statusError{status: http.StatusNotFound, message: fmt.Sprintf("unknown proxy path %s", r.URL.Path)}
	}
	adapter, ok := adapterFor(gateway.ID)
	if !ok {
		return gateway, subpath, nil, &statusError{status: http.StatusNotImplemented, message: fmt.Sprintf("adapter not implemented for %s", gateway.ID)}
	}
	if r.Method != http.MethodPost {
		return gateway, subpath, adapter, &statusError{status: http.StatusMethodNotAllowed, message: fmt.Sprintf("%s requires POST", r.URL.Path)}
	}
	if !adapter.AcceptsPath(subpath) {
		return gateway, subpath, adapter, &statusError{status: http.StatusMethodNotAllowed, message: adapter.RejectMessage(subpath)}
	}
	if err := adapter.ValidateRequest(body); err != nil {
		return gateway, subpath, adapter, &statusError{status: http.StatusBadRequest, message: err.Error()}
	}
	if !gateway.Enabled {
		return gateway, subpath, adapter, &statusError{status: http.StatusServiceUnavailable, message: fmt.Sprintf("gateway %s is disabled", gateway.ID)}
	}
	return gateway, subpath, adapter, nil
}

// buildUpstreamRequest assembles the upstream HTTP request: URL, headers,
// body transformation, and records them in the log entry.
func (p *Proxy) buildUpstreamRequest(ctx context.Context, r *http.Request, gateway config.GatewayConfig, subpath string, adapter GatewayAdapter, requestBody []byte, logEntry *store.RequestLog) (*http.Request, error) {
	if strings.TrimSpace(gateway.BaseURL) == "" {
		return nil, &statusError{status: http.StatusServiceUnavailable, message: fmt.Sprintf("gateway %q has no base URL configured; set it in the console", gateway.ID)}
	}
	upstreamURL, err := joinUpstreamURL(gateway.BaseURL, subpath, r.URL.RawQuery)
	if err != nil {
		return nil, &statusError{status: http.StatusBadGateway, message: err.Error()}
	}
	upstreamBody, err := transformRequestBody(adapter, requestBody)
	if err != nil {
		return nil, &statusError{status: http.StatusBadRequest, message: err.Error()}
	}
	logEntry.UpstreamURL = upstreamURL
	logEntry.UpstreamBody = capBody(upstreamBody, p.responseBodyLimit())

	// The caller supplies the context that bounds the whole exchange: a
	// deadline-bounded one for non-streaming requests, the raw request
	// context for streams. Either way it stays in effect until the upstream
	// response body has been fully read.
	upstreamRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(upstreamBody))
	if err != nil {
		return nil, &statusError{status: http.StatusBadGateway, message: err.Error()}
	}

	allowlist := gateway.ForwardHeaders
	if len(allowlist) == 0 {
		allowlist = defaultForwardHeaders
	}
	copyForwardHeaders(upstreamRequest.Header, r.Header, allowlist)
	upstreamRequest.Header.Set("Content-Type", "application/json")
	upstreamRequest.Header.Del("Content-Length")
	if gateway.UserAgentOverrideEnabled {
		upstreamRequest.Header.Set("User-Agent", gateway.UserAgentOverride)
	}
	logEntry.UpstreamHeaders = headersJSON(upstreamRequest.Header)
	return upstreamRequest, nil
}

// SetProxyURL replaces the global proxy clients so an address saved in the UI
// applies to subsequent requests without restarting the process.
func (p *Proxy) SetProxyURL(proxyURL string) {
	next := newSyncUpstreamClient(proxyURL)
	nextDirect := newSyncUpstreamClient("")
	nextStream := NewUpstreamClient(proxyURL)
	nextStreamDirect := NewUpstreamClient("")
	p.clientMu.Lock()
	old := []*http.Client{p.Client, p.DirectClient, p.StreamClient, p.StreamDirectClient}
	p.Client, p.DirectClient = next, nextDirect
	p.StreamClient, p.StreamDirectClient = nextStream, nextStreamDirect
	p.clientMu.Unlock()
	for _, client := range old {
		if client != nil {
			client.CloseIdleConnections()
		}
	}
}

// clientFor selects the transport pair matching the gateway's proxy setting
// and the request mode. Streaming and non-streaming requests use different
// transports because their header-wait bounds differ; see client.go.
func (p *Proxy) ClientFor(gateway config.GatewayConfig, stream bool) *http.Client {
	p.clientMu.RLock()
	proxyClient, directClient := p.Client, p.DirectClient
	streamProxyClient, streamDirectClient := p.StreamClient, p.StreamDirectClient
	p.clientMu.RUnlock()
	if stream {
		if gateway.UseProxy && streamProxyClient != nil {
			return streamProxyClient
		}
		if !gateway.UseProxy && streamDirectClient != nil {
			return streamDirectClient
		}
	}
	if gateway.UseProxy && proxyClient != nil {
		return proxyClient
	}
	if !gateway.UseProxy && directClient != nil {
		return directClient
	}
	if proxyClient != nil {
		return proxyClient
	}
	return http.DefaultClient
}

// forwardUpstreamResponse performs the upstream HTTP call and writes the
// response back to the client, filling in the response-side audit fields.
func (p *Proxy) forwardUpstreamResponse(w http.ResponseWriter, logEntry *store.RequestLog, adapter GatewayAdapter, gateway config.GatewayConfig, client *http.Client, upstreamRequest *http.Request, bodyLimit int64) error {
	upstreamResponse, err := client.Do(upstreamRequest)
	if err != nil {
		// Transport-level failure (DNS, connect, timeout) before any upstream
		// headers arrived: surface a gateway error instead of a raw error.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return &statusError{status: http.StatusGatewayTimeout, message: err.Error()}
		}
		return &statusError{status: http.StatusBadGateway, message: err.Error()}
	}
	defer upstreamResponse.Body.Close()

	logEntry.StatusCode = upstreamResponse.StatusCode
	logEntry.UpstreamResponseStatusCode = upstreamResponse.StatusCode
	logEntry.UpstreamResponseHeaders = headersJSON(upstreamResponse.Header)

	// Error path: forward the upstream error body verbatim. Clients key their
	// retry / rate-limit / request-error handling off `error.type` and
	// `error.code`, so the proxy must not replace them with its own envelope;
	// only the audit copy is capped, never the bytes the client receives.
	// Retry-After reaches the client via copyResponseHeaders below: it is not
	// a hop-by-hop header, so no special handling is needed for clients to
	// back off correctly.
	if upstreamResponse.StatusCode >= http.StatusBadRequest {
		rawError, readErr := io.ReadAll(upstreamResponse.Body)
		logEntry.ResponseBody = capBody(rawError, bodyLimit)
		// A limit of zero means "capture everything", so nothing is truncated
		// no matter how large the body: compare only when a cap is in effect.
		logEntry.ResponseTruncated = bodyLimit > 0 && int64(len(rawError)) > bodyLimit
		logEntry.UpstreamResponseBody = logEntry.ResponseBody
		if readErr != nil {
			logEntry.Error = readErr.Error()
		}
		logEntry.Success = false
		if logEntry.Error == "" {
			logEntry.Error = fmt.Sprintf("upstream returned HTTP %d", upstreamResponse.StatusCode)
		}
		copyResponseHeaders(w.Header(), upstreamResponse.Header)
		// Trust the upstream's own Content-Type (an HTML edge page must not be
		// announced as JSON); fall back to JSON only when it sent none.
		if w.Header().Get("Content-Type") == "" {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
		}
		writeResponseStatusAndHeaders(w, upstreamResponse)
		_, _ = w.Write(rawError)
		return nil
	}

	// Success path: stream or buffer based on content-type. Reached only when
	// the upstream answered below 400 — the error branch above returns — so
	// logEntry.StatusCode needs no further status-to-error mapping here.
	copyResponseHeaders(w.Header(), upstreamResponse.Header)
	writeResponseStatusAndHeaders(w, upstreamResponse)

	eventStream := isEventStream(upstreamResponse.Header)
	var copyErr error
	var usage store.UsageMetrics

	if eventStream {
		rawResponse := newCappedBuffer(bodyLimit)
		responseReader := io.TeeReader(upstreamResponse.Body, rawResponse)
		responseReader = transformSSE(adapter, responseReader)
		// Metering is fed from the live stream: the capture below is capped, so
		// a stream longer than the capture limit would lose the terminal
		// usage-bearing event and be billed as zero tokens.
		tracker := newUsageTracker(gateway.Protocol)
		filterBefore := sanitizeMetrics()
		copyErr = copyStream(w, responseReader, tracker)
		usage = tracker.usage()
		logEntry.UpstreamResponseBody = append([]byte(nil), rawResponse.Bytes()...)
		logEntry.ResponseTruncated = rawResponse.truncated
		// Observability for SSE filtering (optimization 5)
		if delta := sanitizeMetricsDelta(filterBefore); len(delta) > 0 {
			slog.Debug("sse filter metrics", "metrics", delta)
		}
	} else {
		// Non-streaming: read the full body so the client always receives
		// the complete (possibly transformed) response. Only the audit
		// capture is capped.
		rawCapture := newCappedBufferWithHint(bodyLimit, upstreamResponse.Header.Get("Content-Length"))
		rawResponse, readErr := io.ReadAll(io.TeeReader(upstreamResponse.Body, rawCapture))
		if readErr != nil {
			copyErr = readErr
		}
		logEntry.UpstreamResponseBody = append([]byte(nil), rawCapture.Bytes()...)
		logEntry.ResponseTruncated = rawCapture.truncated

		responseBody, transformErr := transformResponseBody(adapter, rawResponse)
		if transformErr != nil {
			copyErr = transformErr
			responseBody = rawResponse
		}
		if _, writeErr := w.Write(responseBody); copyErr == nil && writeErr != nil {
			copyErr = writeErr
		}
		// Non-streaming: responseBody is the complete body (not a capped
		// capture), so the buffered scan sees the usage object.
		usage = ExtractUsage(responseBody, gateway.Protocol)
	}

	if copyErr != nil {
		logEntry.Error = copyErr.Error()
	}
	// A stream that broke mid-flight — or a body the client never received in
	// full — is not a success, however the upstream answered: recording it as
	// one counts a truncated answer as a whole one in the dashboard's success
	// rate and token totals.
	logEntry.Success = copyErr == nil &&
		upstreamResponse.StatusCode >= 200 && upstreamResponse.StatusCode < 300
	if logEntry.Success {
		logEntry.Usage = usage
	}
	return copyErr
}

// upstreamTimeout returns the effective per-request timeout: the configured
// default, optionally overridden (and capped) by an X-Proxy-Timeout header.
func (p *Proxy) upstreamTimeout(r *http.Request) time.Duration {
	t := config.DefaultUpstreamTimeout
	if p.Config != nil {
		t = p.Config.GetUpstreamTimeout()
	}
	if v := r.Header.Get("X-Proxy-Timeout"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 && d <= maxUpstreamTimeout {
			t = d
		}
	}
	return t
}

// responseBodyLimit returns the per-column capture cap in bytes. A configured
// limit of zero means "capture everything" as far as the caller is concerned,
// so it must not be replaced by the default (which is what "unset" falls back
// to). It is still bounded by maxCapturedBodySize: that is what keeps an
// opt-out from turning into an unbounded write to SQLite, and it is the only
// difference from the configured value — everything below the ceiling is
// stored whole, exactly as a bare "no truncation" would.
func (p *Proxy) responseBodyLimit() int64 {
	if p.ResponseBodySize > 0 {
		return p.ResponseBodySize
	}
	if p.Config != nil {
		if limit := int64(p.Config.GetBodyCaptureLimitKB()) << 10; limit > 0 {
			return limit
		}
		return maxCapturedBodySize
	}
	return defaultResponseBodySize
}

func capBody(data []byte, limit int64) []byte {
	if limit > 0 && int64(len(data)) > limit {
		return append([]byte(nil), data[:limit]...)
	}
	return append([]byte(nil), data...)
}

// gatewayForPath resolves a request path to its gateway. The longest matching
// prefix wins: prefixes are not guaranteed to be disjoint, and Snapshot
// returns a map, so returning the first match would make routing depend on
// Go's randomized map iteration order.
func (p *Proxy) gatewayForPath(path string) (config.GatewayConfig, string, bool) {
	var (
		best    config.GatewayConfig
		bestLen = -1
	)
	for _, gateway := range p.Config.Snapshot() {
		if path != gateway.Prefix && !strings.HasPrefix(path, gateway.Prefix+"/") {
			continue
		}
		// Ties (two gateways configured with the same prefix) are broken by ID
		// so the result stays deterministic across restarts.
		if len(gateway.Prefix) > bestLen ||
			(len(gateway.Prefix) == bestLen && gateway.ID < best.ID) {
			best, bestLen = gateway, len(gateway.Prefix)
		}
	}
	if bestLen < 0 {
		return config.GatewayConfig{}, "", false
	}
	return best, strings.TrimPrefix(path, best.Prefix), true
}

func (p *Proxy) finishLog(ctx context.Context, logEntry *store.RequestLog, started time.Time) {
	ctx = context.WithoutCancel(ctx)
	logEntry.DurationMS = time.Since(started).Milliseconds()
	if logEntry.StatusCode == 0 {
		logEntry.StatusCode = http.StatusInternalServerError
	}
	p.Logger.Info("request completed",
		"request_id", logEntry.ID,
		"gateway", logEntry.GatewayID,
		"model", logEntry.Model,
		"method", logEntry.Method,
		"path", logEntry.RequestPath,
		"status", logEntry.StatusCode,
		"success", logEntry.Success,
		"stream", logEntry.Stream,
		"duration_ms", logEntry.DurationMS,
		"input_tokens", logEntry.Usage.InputTokens,
		"output_tokens", logEntry.Usage.OutputTokens,
	)
	if err := p.Store.Insert(ctx, *logEntry); err != nil {
		p.Logger.Error("write request log", "error", err, "request_id", logEntry.ID)
	}
}

// writeResponseStatusAndHeaders writes the status code and already-copied
// response headers, then flushes so headers reach the client immediately.
// For SSE streams this ensures the client starts receiving as soon as the
// first upstream headers arrive, rather than waiting for the full body.
func writeResponseStatusAndHeaders(w http.ResponseWriter, resp *http.Response) {
	w.WriteHeader(resp.StatusCode)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func joinUpstreamURL(base, path, rawQuery string) (string, error) {
	parsed, err := url.Parse(strings.TrimRight(base, "/") + path)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid upstream URL: %s", base)
	}
	parsed.RawQuery = rawQuery
	return parsed.String(), nil
}

func protocolForPath(path string) config.Protocol {
	if path == "/responses" {
		return config.ProtocolResponses
	}
	return config.ProtocolChat
}

func requestStream(body []byte) bool {
	var request struct {
		Stream bool `json:"stream"`
	}
	return json.Unmarshal(body, &request) == nil && request.Stream
}

func isEventStream(headers http.Header) bool {
	return strings.Contains(strings.ToLower(headers.Get("Content-Type")), "text/event-stream")
}

func copyResponseHeaders(dst, src http.Header) {
	for name, values := range src {
		lower := strings.ToLower(name)
		if hopByHopHeaders[lower] || lower == "content-length" {
			continue
		}
		for _, value := range values {
			dst.Add(name, value)
		}
	}
}

func newRequestID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("req-%d", time.Now().UnixNano())
	}
	return "req-" + hex.EncodeToString(b[:])
}
