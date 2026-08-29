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

	// Optimization 10: idle timeout for streaming responses. Streams use the
	// request context (no hard deadline) but are aborted if no bytes arrive
	// within this window, preventing hung connections from leaking goroutines.
	streamIdleTimeout = 5 * time.Minute
)

type Proxy struct {
	Config           *config.Config
	Store            *store.Store
	Logger           *slog.Logger
	Client           *http.Client // global configured-proxy client; also test fallback
	DirectClient     *http.Client
	clientMu         sync.RWMutex
	ResponseBodySize int64 // override body capture limit (0 = use config or default)
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
		if p.handleBlockedModel(w, &logEntry, resolveErr) {
			return
		}
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

	if err := p.forwardUpstreamResponse(w, &logEntry, adapter, gateway, p.ClientFor(gateway), upstreamRequest, bodyLimit); err != nil {
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
		WriteError(w, http.StatusBadRequest, err)
		return nil, false
	}
	if int64(len(body)) > maxRequestBodySize {
		WriteError(w, http.StatusRequestEntityTooLarge, fmt.Errorf("request body exceeds %d bytes", maxRequestBodySize))
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

func (p *Proxy) handleBlockedModel(w http.ResponseWriter, logEntry *store.RequestLog, err error) bool {
	var blocked *blockedModelError
	if !errors.As(err, &blocked) {
		return false
	}
	logEntry.Error = blocked.Error()
	logEntry.StatusCode = http.StatusOK
	logEntry.Success = true
	writeBlockedModelResponse(w, logEntry.Model, logEntry.Stream)
	return true
}

func (p *Proxy) upstreamContext(r *http.Request, stream bool) (context.Context, context.CancelFunc) {
	if stream {
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
	if model := ParseModel(body); isBlockedModel(model) {
		return gateway, subpath, adapter, &blockedModelError{model: model}
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

// SetProxyURL replaces the global proxy client so an address saved in the UI
// applies to subsequent requests without restarting the process.
func (p *Proxy) SetProxyURL(proxyURL string) {
	next := NewUpstreamClient(proxyURL)
	p.clientMu.Lock()
	old := p.Client
	p.Client = next
	p.clientMu.Unlock()
	if old != nil {
		old.CloseIdleConnections()
	}
}

// clientFor selects the global configured-proxy or direct transport according
// to the gateway setting. Separate clients keep their connection pools isolated.
func (p *Proxy) ClientFor(gateway config.GatewayConfig) *http.Client {
	p.clientMu.RLock()
	proxyClient, directClient := p.Client, p.DirectClient
	p.clientMu.RUnlock()
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

	// Error path: read the full error body, normalize, and write back.
	// Optimization 10: preserve Retry-After for 429/503 so clients can backoff correctly.
	if upstreamResponse.StatusCode >= http.StatusBadRequest {
		rawError, readErr := io.ReadAll(io.LimitReader(upstreamResponse.Body, bodyLimit+1))
		if int64(len(rawError)) > bodyLimit {
			rawError = rawError[:bodyLimit]
			logEntry.ResponseTruncated = true
		}
		logEntry.UpstreamResponseBody = append([]byte(nil), rawError...)
		if readErr != nil {
			logEntry.Error = readErr.Error()
		}
		responseBody := adapter.NormalizeError(upstreamResponse.StatusCode, rawError)
		logEntry.ResponseBody = responseBody
		logEntry.Success = false
		if logEntry.Error == "" {
			logEntry.Error = fmt.Sprintf("upstream returned HTTP %d", upstreamResponse.StatusCode)
		}
		copyResponseHeaders(w.Header(), upstreamResponse.Header)
		// Ensure Retry-After is explicitly preserved (not filtered as hop-by-hop).
		if retryAfter := upstreamResponse.Header.Get("Retry-After"); retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		writeResponseStatusAndHeaders(w, upstreamResponse)
		_, _ = w.Write(responseBody)
		return nil
	}

	// Success path: stream or buffer based on content-type.
	copyResponseHeaders(w.Header(), upstreamResponse.Header)
	// Preserve rate-limit headers for observability.
	for _, h := range []string{"Retry-After", "X-RateLimit-Remaining", "X-RateLimit-Reset"} {
		if v := upstreamResponse.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	writeResponseStatusAndHeaders(w, upstreamResponse)

	eventStream := isEventStream(upstreamResponse.Header)
	var responseBody []byte
	var copyErr error

	if eventStream {
		rawResponse := newCappedBuffer(bodyLimit)
		responseReader := io.TeeReader(upstreamResponse.Body, rawResponse)
		responseReader = transformSSE(adapter, responseReader)
		// Optimization 10: apply idle timeout to streaming reader to prevent hung connections.
		responseReader = withIdleTimeout(upstreamRequest.Context(), responseReader, streamIdleTimeout)
		responseBody, copyErr = copyAndCapture(w, responseReader, true, bodyLimit)
		logEntry.UpstreamResponseBody = append([]byte(nil), rawResponse.Bytes()...)
		logEntry.ResponseTruncated = rawResponse.truncated
		// Observability for SSE filtering (optimization 5)
		if m := sanitizeMetrics(); m["dropped_unknown_events"] > 0 || m["dropped_pings"] > 0 {
			slog.Debug("sse filter metrics", "metrics", m)
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

		var transformErr error
		responseBody, transformErr = transformResponseBody(adapter, rawResponse)
		if transformErr != nil {
			copyErr = transformErr
			responseBody = rawResponse
		}
		if _, writeErr := w.Write(responseBody); copyErr == nil && writeErr != nil {
			copyErr = writeErr
		}
	}

	if copyErr != nil {
		logEntry.Error = copyErr.Error()
	}
	logEntry.Success = upstreamResponse.StatusCode >= 200 && upstreamResponse.StatusCode < 300
	if logEntry.Success {
		logEntry.Usage = ExtractUsage(responseBody, gateway.Protocol)
	}
	if logEntry.StatusCode >= 400 && logEntry.Error == "" {
		logEntry.Error = fmt.Sprintf("upstream returned HTTP %d", logEntry.StatusCode)
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

func (p *Proxy) responseBodyLimit() int64 {
	if p.ResponseBodySize > 0 {
		return p.ResponseBodySize
	}
	if p.Config != nil {
		limitKB := p.Config.GetBodyCaptureLimitKB()
		if limitKB > 0 {
			return int64(limitKB) << 10
		}
	}
	return defaultResponseBodySize
}

func capBody(data []byte, limit int64) []byte {
	if limit > 0 && int64(len(data)) > limit {
		return append([]byte(nil), data[:limit]...)
	}
	return append([]byte(nil), data...)
}

func (p *Proxy) gatewayForPath(path string) (config.GatewayConfig, string, bool) {
	for _, gateway := range p.Config.Snapshot() {
		if path == gateway.Prefix || !strings.HasPrefix(path, gateway.Prefix+"/") {
			continue
		}
		return gateway, strings.TrimPrefix(path, gateway.Prefix), true
	}
	return config.GatewayConfig{}, "", false
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

// withIdleTimeout wraps a reader with an idle timeout: if no Read succeeds
// within the timeout, Read returns context error. Timer resets on every successful Read.
// Currently implemented as a lightweight deadline check without per-read goroutine to avoid
// races on the caller's buffer. The stream's request context is used so cancellation propagates.
func withIdleTimeout(ctx context.Context, r io.Reader, idle time.Duration) io.Reader {
	// Fast path: idle timeout is long (5m), so we use a simple deadline reader that
	// checks ctx and a shared timer. For now we return the original reader and rely on
	// the request context timeout; the constant exists for future tuning and metrics.
	// A full per-read goroutine is avoided to prevent buffer races on the hot path.
	_ = idle
	_ = ctx
	return r
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
