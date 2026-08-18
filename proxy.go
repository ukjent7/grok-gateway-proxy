package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Proxy struct {
	config *Config
	store  *Store
	logger *slog.Logger
	client *http.Client
}

type responseCapture struct {
	http.ResponseWriter
	statusCode int
	headers    http.Header
	body       bytes.Buffer
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

func cloneHeaders(src http.Header) http.Header {
	dst := make(http.Header, len(src))
	for name, values := range src {
		dst[name] = append([]string(nil), values...)
	}
	return dst
}

const maxRequestBodySize int64 = 64 << 20

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	capturedResponse := &responseCapture{ResponseWriter: w}
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
	model := ParseModel(requestBody)
	stream := requestStream(requestBody)
	logEntry.RequestBody = append([]byte(nil), requestBody...)
	logEntry.Model = model
	logEntry.Stream = stream

	gateway, subpath, ok := p.gatewayForPath(r.URL.Path)
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Errorf("unknown proxy path %s", r.URL.Path))
		logEntry.Error = "unknown proxy path"
		logEntry.StatusCode = http.StatusNotFound
		return
	}
	logEntry.GatewayID = gateway.ID
	logEntry.GatewayName = gateway.Name
	logEntry.Prefix = gateway.Prefix
	logEntry.UpstreamProtocol = gateway.Protocol
	adapter, ok := adapterFor(gateway.ID)
	if !ok {
		writeError(w, http.StatusNotImplemented, fmt.Errorf("adapter not implemented for %s", gateway.ID))
		logEntry.Error = "adapter not implemented"
		logEntry.StatusCode = http.StatusNotImplemented
		return
	}
	logEntry.IngressProtocol = protocolForPath(subpath)
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("%s requires POST", r.URL.Path))
		logEntry.Error = "method not allowed"
		logEntry.StatusCode = http.StatusMethodNotAllowed
		return
	}
	if !adapter.AcceptsPath(subpath) {
		writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("%s", adapter.RejectMessage(subpath)))
		logEntry.Error = adapter.RejectMessage(subpath)
		logEntry.StatusCode = http.StatusMethodNotAllowed
		return
	}
	if err := adapter.ValidateRequest(requestBody); err != nil {
		writeError(w, http.StatusBadRequest, err)
		logEntry.Error = err.Error()
		logEntry.StatusCode = http.StatusBadRequest
		return
	}
	if !gateway.Enabled {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("gateway %s is disabled", gateway.ID))
		logEntry.Error = "gateway disabled"
		logEntry.StatusCode = http.StatusServiceUnavailable
		return
	}

	upstreamURL, err := joinUpstreamURL(gateway.BaseURL, subpath, r.URL.RawQuery)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		logEntry.Error = err.Error()
		logEntry.StatusCode = http.StatusBadGateway
		return
	}
	logEntry.UpstreamURL = upstreamURL
	upstreamBody, err := transformRequestBody(adapter, requestBody)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		logEntry.Error = err.Error()
		logEntry.StatusCode = http.StatusBadRequest
		return
	}
	logEntry.UpstreamBody = append([]byte(nil), upstreamBody...)

	upstreamRequest, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upstreamURL, bytes.NewReader(upstreamBody))
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		logEntry.Error = err.Error()
		logEntry.StatusCode = http.StatusBadGateway
		return
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

	upstreamResponse, err := p.client.Do(upstreamRequest)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		logEntry.Error = err.Error()
		logEntry.StatusCode = http.StatusBadGateway
		return
	}
	defer upstreamResponse.Body.Close()
	logEntry.StatusCode = upstreamResponse.StatusCode
	logEntry.UpstreamResponseStatusCode = upstreamResponse.StatusCode
	logEntry.UpstreamResponseHeaders = headersJSON(upstreamResponse.Header)
	logEntry.UpstreamResponseHeadersActual = headersJSONActual(upstreamResponse.Header)
	if upstreamResponse.StatusCode >= http.StatusBadRequest {
		rawError, readErr := io.ReadAll(upstreamResponse.Body)
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
		w.WriteHeader(upstreamResponse.StatusCode)
		_, _ = w.Write(responseBody)
		return
	}

	copyResponseHeaders(w.Header(), upstreamResponse.Header)
	w.WriteHeader(upstreamResponse.StatusCode)
	eventStream := isEventStream(upstreamResponse.Header)
	var responseBody []byte
	var copyErr error
	if eventStream {
		var rawResponse bytes.Buffer
		responseReader := io.Reader(io.TeeReader(upstreamResponse.Body, &rawResponse))
		if transformer, ok := adapter.(streamTransformer); ok {
			responseReader = transformer.TransformSSE(responseReader)
		}
		responseBody, copyErr = copyAndCapture(w, responseReader, stream || eventStream)
		logEntry.UpstreamResponseBody = append([]byte(nil), rawResponse.Bytes()...)
	} else {
		var rawResponse []byte
		rawResponse, copyErr = io.ReadAll(upstreamResponse.Body)
		logEntry.UpstreamResponseBody = append([]byte(nil), rawResponse...)
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
	if err := p.store.Insert(ctx, *logEntry); err != nil {
		p.logger.Error("write request log", "error", err, "request_id", logEntry.ID)
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

func copyAndCapture(w http.ResponseWriter, reader io.Reader, streaming bool) ([]byte, error) {
	var capture bytes.Buffer
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
	var bytes [12]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return fmt.Sprintf("req-%d", time.Now().UnixNano())
	}
	return "req-" + hex.EncodeToString(bytes[:])
}
