package main

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
	"time"
)

const (
	// maxRequestBodySize bounds the audit-captured request body. Requests
	// larger than this are rejected with 413 before any upstream call.
	maxRequestBodySize int64 = 64 << 20

	// maxResponseBodySize bounds the audit-captured response body. Responses
	// larger than this are still forwarded to the client in full, but only the
	// first maxResponseBodySize bytes are buffered for the log, and the log is
	// flagged with response_truncated.
	maxResponseBodySize int64 = 64 << 20

	// maxUpstreamTimeout caps the per-request timeout that a client may
	// request via the X-Proxy-Timeout header.
	maxUpstreamTimeout = 30 * time.Minute
)

type Proxy struct {
	config           *Config
	store            *Store
	logger           *slog.Logger
	client           *http.Client
	responseBodySize int64 // 0 = use maxResponseBodySize
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
	capturedResponse := newResponseCapture(w, bodyLimit)
	w = capturedResponse

	logEntry := RequestLog{
		ID:                   newRequestID(),
		StartedAt:            started,
		Method:               r.Method,
		RequestPath:          r.URL.Path,
		RequestURL:           r.URL.RequestURI(),
		RequestHeaders:       headersJSON(r.Header),
		RequestHeadersActual: headersJSONActual(r.Header),
	}
	defer func() {
		logEntry.ClientResponseStatusCode = capturedResponse.statusCode
		if logEntry.StatusCode == 0 && capturedResponse.statusCode != 0 {
			logEntry.StatusCode = capturedResponse.statusCode
		}
		if logEntry.StatusCode == 0 {
			logEntry.StatusCode = http.StatusInternalServerError
		}
		logEntry.ResponseHeaders = headersJSON(capturedResponse.headers)
		logEntry.ResponseHeadersActual = headersJSONActual(capturedResponse.headers)
		logEntry.ResponseBody = append([]byte(nil), capturedResponse.body.Bytes()...)
		logEntry.ResponseTruncated = logEntry.ResponseTruncated || capturedResponse.body.truncated
		p.finishLog(r.Context(), &logEntry, started)
	}()

	requestBody, readErr := io.ReadAll(io.LimitReader(r.Body, maxRequestBodySize+1))
	if readErr != nil {
		writeError(w, http.StatusBadRequest, readErr)
		return
	}
	if int64(len(requestBody)) > maxRequestBodySize {
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Errorf("request body exceeds %d bytes", maxRequestBodySize))
		return
	}

	logEntry.RequestBody = append([]byte(nil), requestBody...)
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
		logEntry.Error = resolveErr.Error()
		logEntry.StatusCode = statusOf(resolveErr)
		writeError(w, logEntry.StatusCode, resolveErr)
		return
	}

	// Bound the whole upstream exchange (headers + full body) by a timeout a
	// client may extend via X-Proxy-Timeout. The cancel is deferred here so the
	// deadline stays in effect for the entire Do and body read in
	// forwardUpstreamResponse, rather than being cancelled the moment
	// buildUpstreamRequest returns.
	upstreamCtx, cancel := context.WithTimeout(r.Context(), p.upstreamTimeout(r))
	defer cancel()

	upstreamRequest, err := p.buildUpstreamRequest(upstreamCtx, r, gateway, subpath, adapter, requestBody, &logEntry)
	if err != nil {
		logEntry.Error = err.Error()
		logEntry.StatusCode = statusOf(err)
		writeError(w, logEntry.StatusCode, err)
		return
	}

	if err := p.forwardUpstreamResponse(w, &logEntry, adapter, gateway.Protocol, upstreamRequest, logEntry.Stream, bodyLimit); err != nil {
		logEntry.Error = err.Error()
		// If nothing was written to the client yet (a transport-level failure
		// before any upstream headers arrived), surface a gateway error instead
		// of leaving the client with an empty default 200. If headers were
		// already sent mid-stream, keep the upstream status and just log.
		if capturedResponse.statusCode == 0 {
			logEntry.StatusCode = statusOf(err)
			writeError(w, logEntry.StatusCode, err)
		}
	}
}

// resolveGateway maps the inbound path to a gateway + adapter and validates
// the request (method, subpath, body). Returns a *statusError on failure.
func (p *Proxy) resolveGateway(r *http.Request, body []byte) (GatewayConfig, string, GatewayAdapter, error) {
	gateway, subpath, ok := p.gatewayForPath(r.URL.Path)
	if !ok {
		return GatewayConfig{}, "", nil, &statusError{status: http.StatusNotFound, message: fmt.Sprintf("unknown proxy path %s", r.URL.Path)}
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
func (p *Proxy) buildUpstreamRequest(ctx context.Context, r *http.Request, gateway GatewayConfig, subpath string, adapter GatewayAdapter, requestBody []byte, logEntry *RequestLog) (*http.Request, error) {
	upstreamURL, err := joinUpstreamURL(gateway.BaseURL, subpath, r.URL.RawQuery)
	if err != nil {
		return nil, &statusError{status: http.StatusBadGateway, message: err.Error()}
	}
	logEntry.UpstreamURL = upstreamURL

	upstreamBody, err := transformRequestBody(adapter, requestBody)
	if err != nil {
		return nil, &statusError{status: http.StatusBadRequest, message: err.Error()}
	}
	logEntry.UpstreamBody = append([]byte(nil), upstreamBody...)

	// The caller supplies an already timeout-bounded context, so the deadline
	// survives until the upstream response body has been fully read.
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
	logEntry.UpstreamHeadersActual = headersJSONActual(upstreamRequest.Header)
	return upstreamRequest, nil
}

// forwardUpstreamResponse performs the upstream HTTP call and writes the
// response back to the client, filling in the response-side audit fields.
func (p *Proxy) forwardUpstreamResponse(w http.ResponseWriter, logEntry *RequestLog, adapter GatewayAdapter, protocol Protocol, upstreamRequest *http.Request, stream bool, bodyLimit int64) error {
	upstreamResponse, err := p.client.Do(upstreamRequest)
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
	logEntry.UpstreamResponseHeadersActual = headersJSONActual(upstreamResponse.Header)

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
	writeResponseStatusAndHeaders(w, upstreamResponse)

	eventStream := isEventStream(upstreamResponse.Header)
	var responseBody []byte
	var copyErr error

	if eventStream {
		rawResponse := newCappedBuffer(bodyLimit)
		responseReader := io.Reader(io.TeeReader(upstreamResponse.Body, rawResponse))
		if transformer, ok := adapter.(streamTransformer); ok {
			responseReader = transformer.TransformSSE(responseReader)
		}
		responseBody, copyErr = copyAndCapture(w, responseReader, true, bodyLimit)
		logEntry.UpstreamResponseBody = append([]byte(nil), rawResponse.Bytes()...)
		logEntry.ResponseTruncated = rawResponse.truncated
	} else {
		// Non-streaming: read the full body so the client always receives
		// the complete (possibly transformed) response. Only the audit
		// capture is capped.
		rawCapture := newCappedBuffer(bodyLimit)
		rawResponse, copyErr := io.ReadAll(io.TeeReader(upstreamResponse.Body, rawCapture))
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
		logEntry.Usage = ExtractUsage(responseBody, protocol)
	}
	if logEntry.StatusCode >= 400 && logEntry.Error == "" {
		logEntry.Error = fmt.Sprintf("upstream returned HTTP %d", logEntry.StatusCode)
	}
	return copyErr
}

// upstreamTimeout returns the effective per-request timeout: the configured
// default, optionally overridden (and capped) by an X-Proxy-Timeout header.
func (p *Proxy) upstreamTimeout(r *http.Request) time.Duration {
	t := p.config.UpstreamTimeout
	if t <= 0 {
		t = defaultUpstreamTimeout
	}
	if v := r.Header.Get("X-Proxy-Timeout"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 && d <= maxUpstreamTimeout {
			t = d
		}
	}
	return t
}

func (p *Proxy) responseBodyLimit() int64 {
	if p.responseBodySize > 0 {
		return p.responseBodySize
	}
	return maxResponseBodySize
}

func (p *Proxy) gatewayForPath(path string) (GatewayConfig, string, bool) {
	for _, gateway := range p.config.Snapshot() {
		if path == gateway.Prefix || !strings.HasPrefix(path, gateway.Prefix+"/") {
			continue
		}
		return gateway, strings.TrimPrefix(path, gateway.Prefix), true
	}
	return GatewayConfig{}, "", false
}

func (p *Proxy) finishLog(ctx context.Context, logEntry *RequestLog, started time.Time) {
	ctx = context.WithoutCancel(ctx)
	logEntry.DurationMS = time.Since(started).Milliseconds()
	if logEntry.StatusCode == 0 {
		logEntry.StatusCode = http.StatusInternalServerError
	}
	p.logger.Info("request completed",
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
	if err := p.store.Insert(ctx, *logEntry); err != nil {
		p.logger.Error("write request log", "error", err, "request_id", logEntry.ID)
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
	w.headers = cloneHeaders(w.ResponseWriter.Header())
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

func protocolForPath(path string) Protocol {
	if path == "/responses" {
		return ProtocolResponses
	}
	return ProtocolChat
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
			if err == io.EOF {
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
