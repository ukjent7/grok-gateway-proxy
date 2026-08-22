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

type blockedModelError struct {
	model string
}

func (e *blockedModelError) Error() string {
	return fmt.Sprintf("model %q is blocked by the proxy", e.model)
}

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
	capturedResponse := newResponseCapture(w, bodyLimit)
	w = capturedResponse

	logEntry := store.RequestLog{
		ID:             newRequestID(),
		StartedAt:      started,
		Method:         r.Method,
		RequestPath:    r.URL.Path,
		RequestURL:     r.URL.RequestURI(),
		RequestHeaders: headersJSON(r.Header),
	}
	// Expose the request id to the client for log correlation without
	// altering the request or response body.
	w.Header().Set("X-Request-Id", logEntry.ID)
	defer func() {
		logEntry.ClientResponseStatusCode = capturedResponse.statusCode
		if logEntry.StatusCode == 0 && capturedResponse.statusCode != 0 {
			logEntry.StatusCode = capturedResponse.statusCode
		}
		if logEntry.StatusCode == 0 {
			logEntry.StatusCode = http.StatusInternalServerError
		}
		logEntry.ResponseHeaders = headersJSON(capturedResponse.headers)
		logEntry.ResponseBody = append([]byte(nil), capturedResponse.body.Bytes()...)
		logEntry.ResponseTruncated = logEntry.ResponseTruncated || capturedResponse.body.truncated
		p.finishLog(r.Context(), &logEntry, started)
	}()

	requestBody, readErr := io.ReadAll(io.LimitReader(r.Body, maxRequestBodySize+1))
	if readErr != nil {
		WriteError(w, http.StatusBadRequest, readErr)
		return
	}
	if int64(len(requestBody)) > maxRequestBodySize {
		WriteError(w, http.StatusRequestEntityTooLarge, fmt.Errorf("request body exceeds %d bytes", maxRequestBodySize))
		return
	}

	logEntry.RequestBody = capBody(requestBody, bodyLimit)
	logEntry.Model = ParseModel(requestBody)
	logEntry.Stream = requestStream(requestBody)

	gateway, subpath, adapter, resolveErr := p.resolveGateway(r, requestBody)
	if gateway.ID != "" {
		logEntry.GatewayID = gateway.ID
		logEntry.GatewayName = gateway.Name
		logEntry.Prefix = gateway.Prefix
		logEntry.UpstreamProtocol = gateway.Protocol
	}
	if adapter != nil {
		logEntry.IngressProtocol = protocolForPath(subpath)
	}
	if resolveErr != nil {
		var blocked *blockedModelError
		if errors.As(resolveErr, &blocked) {
			logEntry.Error = blocked.Error()
			logEntry.StatusCode = http.StatusOK
			logEntry.Success = true
			writeBlockedModelResponse(w, logEntry.Model, logEntry.Stream)
			return
		}
		logEntry.Error = resolveErr.Error()
		logEntry.StatusCode = statusOf(resolveErr)
		WriteError(w, logEntry.StatusCode, resolveErr)
		return
	}

	// Non-streaming responses are bounded by a total deadline (overridable via
	// X-Proxy-Timeout). Streaming responses are deliberately NOT given a total
	// deadline: the client enforces a 300s idle timeout (only while the stream
	// is silent), so a total cap here would truncate long, still-active streams
	// mid-response and make the client synthesize a retriable "No
	// ResponseCompleted" error. For streams we rely on client disconnect
	// (r.Context() cancel) plus the transport's ResponseHeaderTimeout for the
	// first byte. cancel is deferred so the deadline (non-stream) stays in
	// effect for the entire Do and body read in forwardUpstreamResponse, rather
	// than being cancelled the moment buildUpstreamRequest returns.
	upstreamCtx := r.Context()
	var cancel context.CancelFunc
	if logEntry.Stream {
		cancel = func() {}
	} else {
		upstreamCtx, cancel = context.WithTimeout(r.Context(), p.upstreamTimeout(r))
	}
	defer cancel()

	upstreamRequest, err := p.buildUpstreamRequest(upstreamCtx, r, gateway, subpath, adapter, requestBody, &logEntry)
	if err != nil {
		logEntry.Error = err.Error()
		logEntry.StatusCode = statusOf(err)
		WriteError(w, logEntry.StatusCode, err)
		return
	}

	if err := p.forwardUpstreamResponse(w, &logEntry, adapter, gateway, p.ClientFor(gateway), upstreamRequest, logEntry.Stream, bodyLimit); err != nil {
		logEntry.Error = err.Error()
		// If nothing was written to the client yet (a transport-level failure
		// before any upstream headers arrived), surface a gateway error instead
		// of leaving the client with an empty default 200. If headers were
		// already sent mid-stream, keep the upstream status and just log.
		if capturedResponse.statusCode == 0 {
			logEntry.StatusCode = statusOf(err)
			WriteError(w, logEntry.StatusCode, err)
		}
	}
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
	fxMode := gateway.ID == "ve" && gateway.FXDisguiseEnabled
	var upstreamURL string
	var upstreamBody []byte
	var err error
	if fxMode {
		// FX disguise mode: switch to the v3 language-model endpoint and
		// convert the Responses body into the v3 payload with the promo
		// headers injected.
		upstreamURL = vercelFXUpstreamURL(gateway.BaseURL)
		ua := gateway.FXDisguiseUserAgent
		if ua == "" {
			ua = "fx/0.0.3"
		}
		upstreamBody, err = convertResponsesToV3(requestBody, ua)
		if err != nil {
			return nil, &statusError{status: http.StatusBadRequest, message: err.Error()}
		}
	} else {
		upstreamURL, err = joinUpstreamURL(gateway.BaseURL, subpath, r.URL.RawQuery)
		if err != nil {
			return nil, &statusError{status: http.StatusBadGateway, message: err.Error()}
		}
		upstreamBody, err = transformRequestBody(adapter, logEntry.Model, requestBody)
		if err != nil {
			return nil, &statusError{status: http.StatusBadRequest, message: err.Error()}
		}
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
	if fxMode {
		sessionID := fxSessionID(r, requestBody)
		ua := gateway.FXDisguiseUserAgent
		if ua == "" {
			ua = "fx/0.0.3"
		}
		for name, value := range fxDisguiseHeaders(ua, logEntry.Model, sessionID) {
			upstreamRequest.Header.Set(name, value)
		}
	} else if gateway.UserAgentOverrideEnabled {
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
func (p *Proxy) forwardUpstreamResponse(w http.ResponseWriter, logEntry *store.RequestLog, adapter GatewayAdapter, gateway config.GatewayConfig, client *http.Client, upstreamRequest *http.Request, stream bool, bodyLimit int64) error {
	protocol := gateway.Protocol
	fxMode := gateway.ID == "ve" && gateway.FXDisguiseEnabled
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
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		writeResponseStatusAndHeaders(w, upstreamResponse)
		_, _ = w.Write(responseBody)
		return nil
	}

	// Success path: stream or buffer based on content-type.
	copyResponseHeaders(w.Header(), upstreamResponse.Header)
	if fxMode && !stream {
		// The v3 endpoint always streams SSE; a non-streaming client expects a
		// JSON Responses object instead.
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
	}
	writeResponseStatusAndHeaders(w, upstreamResponse)

	eventStream := isEventStream(upstreamResponse.Header)
	var responseBody []byte
	var copyErr error

	if fxMode {
		// FX disguise mode: the upstream speaks the v3 language-model SSE
		// protocol. Convert it back to the Responses protocol the client
		// expects (SSE for streaming, assembled JSON otherwise).
		rawResponse := newCappedBuffer(bodyLimit)
		responseReader := io.TeeReader(upstreamResponse.Body, rawResponse)
		if stream {
			responseReader = newVercelFXSSEReader(responseReader, logEntry.Model)
			responseBody, copyErr = copyAndCapture(w, responseReader, true, bodyLimit)
		} else {
			raw, readErr := io.ReadAll(responseReader)
			if readErr != nil {
				copyErr = readErr
			} else {
				var transformErr error
				responseBody, transformErr = vercelFXSSEToResponses(logEntry.Model, bytes.NewReader(raw))
				if transformErr != nil {
					copyErr = transformErr
					responseBody = raw
				}
			}
			if _, writeErr := w.Write(responseBody); copyErr == nil && writeErr != nil {
				copyErr = writeErr
			}
		}
		logEntry.UpstreamResponseBody = append([]byte(nil), rawResponse.Bytes()...)
		logEntry.ResponseTruncated = rawResponse.truncated
	} else if eventStream {
		rawResponse := newCappedBuffer(bodyLimit)
		responseReader := io.TeeReader(upstreamResponse.Body, rawResponse)
		responseReader = transformSSE(adapter, logEntry.Model, responseReader)
		responseBody, copyErr = copyAndCapture(w, responseReader, true, bodyLimit)
		logEntry.UpstreamResponseBody = append([]byte(nil), rawResponse.Bytes()...)
		logEntry.ResponseTruncated = rawResponse.truncated
	} else {
		// Non-streaming: read the full body so the client always receives
		// the complete (possibly transformed) response. Only the audit
		// capture is capped.
		rawCapture := newCappedBuffer(bodyLimit)
		rawResponse, readErr := io.ReadAll(io.TeeReader(upstreamResponse.Body, rawCapture))
		if readErr != nil {
			copyErr = readErr
		}
		logEntry.UpstreamResponseBody = append([]byte(nil), rawCapture.Bytes()...)
		logEntry.ResponseTruncated = rawCapture.truncated

		var transformErr error
		responseBody, transformErr = transformResponseBody(adapter, logEntry.Model, rawResponse)
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
		if fxMode {
			// Keep cache support semantics from the original v3 usage. The
			// Responses envelope intentionally contains cached_tokens: 0 even
			// when the upstream omitted cache fields for strict clients, so
			// extracting from responseBody would falsely report 0% cache hits.
			logEntry.Usage = extractFXUsage(logEntry.UpstreamResponseBody)
		} else {
			logEntry.Usage = ExtractUsage(responseBody, protocol)
		}
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

// --- Response capture ---

// responseCapture wraps the client-facing ResponseWriter to snapshot the
// status, headers, and (capped) body the client actually receives.
type responseCapture struct {
	http.ResponseWriter
	statusCode int
	headers    http.Header
	body       *cappedBuffer
}

func newResponseCapture(w http.ResponseWriter, limit int64) *responseCapture {
	return &responseCapture{ResponseWriter: w, body: newCappedBuffer(limit)}
}

func (w *responseCapture) WriteHeader(statusCode int) {
	if w.statusCode != 0 {
		return
	}
	w.statusCode = statusCode
	w.headers = cloneHeaders(w.Header())
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *responseCapture) Write(data []byte) (int, error) {
	if w.statusCode == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(data)
	if n > 0 {
		_, _ = w.body.Write(data[:n])
	}
	return n, err
}

func (w *responseCapture) Flush() {
	if w.statusCode == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// --- Capped buffer ---

// cappedBuffer is an io.Writer that stops accepting bytes once limit is
// reached, so a TeeReader can snapshot the upstream stream without unbounded
// buffering. Writes beyond the limit are reported as written so the
// TeeReader keeps passing the full stream through.
type cappedBuffer struct {
	buf       bytes.Buffer
	limit     int64
	truncated bool
}

func newCappedBuffer(limit int64) *cappedBuffer {
	c := &cappedBuffer{limit: limit}
	const preAlloc = 64 * 1024
	if limit > 0 && limit < preAlloc {
		c.buf.Grow(int(limit))
	} else if limit >= preAlloc {
		c.buf.Grow(preAlloc)
	}
	return c
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if c.truncated {
		return len(p), nil
	}
	remaining := c.limit - int64(c.buf.Len())
	if remaining <= 0 {
		c.truncated = true
		return len(p), nil
	}
	if int64(len(p)) > remaining {
		_, _ = c.buf.Write(p[:remaining])
		c.truncated = true
	} else {
		_, _ = c.buf.Write(p)
	}
	return len(p), nil
}

func (c *cappedBuffer) Bytes() []byte { return c.buf.Bytes() }

// --- Helpers ---

func cloneHeaders(src http.Header) http.Header {
	dst := make(http.Header, len(src))
	for name, values := range src {
		dst[name] = append([]string(nil), values...)
	}
	return dst
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

func isBlockedModel(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	return model == "grok" || strings.HasPrefix(model, "grok-")
}

func writeBlockedModelResponse(w http.ResponseWriter, model string, stream bool) {
	responseID := "resp-blocked-" + strings.TrimPrefix(newRequestID(), "req-")
	response := map[string]any{
		"id":     responseID,
		"object": "response",
		"status": "completed",
		"model":  model,
		"output": []any{},
	}

	if !stream {
		WriteJSON(w, http.StatusOK, response)
		return
	}

	created := map[string]any{
		"type":            "response.created",
		"sequence_number": 0,
		"response":        response,
	}
	completed := map[string]any{
		"type":            "response.completed",
		"sequence_number": 1,
		"response":        response,
	}
	createdBody, _ := json.Marshal(created)
	completedBody, _ := json.Marshal(completed)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, "event: response.created\ndata: %s\n\n", createdBody)
	_, _ = fmt.Fprintf(w, "event: response.completed\ndata: %s\n\n", completedBody)
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
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

func copyAndCapture(w http.ResponseWriter, reader io.Reader, streaming bool, limit int64) ([]byte, error) {
	capture := newCappedBuffer(limit)
	buf := make([]byte, 32*1024)
	flusher, canFlush := w.(http.Flusher)
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if _, writeErr := w.Write(chunk); writeErr != nil {
				return capture.Bytes(), writeErr
			}
			_, _ = capture.Write(chunk)
			if streaming && canFlush {
				flusher.Flush()
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return capture.Bytes(), nil
			}
			return capture.Bytes(), err
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

// fxSessionID derives a stable session identifier for Vercel AI Gateway FX
// mode. Vercel uses X-Session-Id / X-Session-Affinity for cache affinity: it
// routes the request to a backend node that holds the cached prompt prefix. A
// random per-request ID defeats this, so the prompt is re-tokenized on every
// call and cache hits collapse.
//
// The real fx client generates one session ID per conversation at startup and
// reuses it for every turn. The proxy is stateless, so it derives a stable ID
// from the request body's prompt_cache_key (set by the client to the
// conversation ID and kept constant across turns). When that field is absent,
// it falls back to x-session-id / x-session-affinity / x-client-request-id —
// standard gateway headers the original fx client also honors — and only
// generates a random ID as a last resort.
func fxSessionID(r *http.Request, requestBody []byte) string {
	var root struct {
		PromptCacheKey string `json:"prompt_cache_key"`
	}
	if json.Unmarshal(requestBody, &root) == nil {
		if key := strings.TrimSpace(root.PromptCacheKey); key != "" {
			return key
		}
	}
	for _, h := range []string{"x-session-id", "x-session-affinity", "x-client-request-id"} {
		if v := r.Header.Get(h); v != "" {
			return v
		}
	}
	return "pi-" + fxHex(8)
}
