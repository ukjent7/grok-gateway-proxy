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
	maxBodyBytes            int64 = 32 << 20
	inFlightBodyBudget      int64 = 64 << 20
	bodyAdmissionTimeout          = 30 * time.Second
	defaultResponseBodySize int64 = 256 << 10
	maxUpstreamTimeout            = 30 * time.Minute
	auditWriteTimeout             = 15 * time.Second
)

type Proxy struct {
	Config             *config.Config
	Store              *store.Store
	Logger             *slog.Logger
	Client             *http.Client
	DirectClient       *http.Client
	StreamClient       *http.Client
	StreamDirectClient *http.Client
	clientMu           sync.RWMutex
	ResponseBodySize   int64
	bodies             *bodyAdmission
}

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

	release, admitErr := p.admitBody(r)
	defer release()
	if admitErr != nil {
		if errors.Is(admitErr, errAdmissionTimeout) {
			p.Logger.Warn("request gave up waiting for body budget", "error", admitErr, "path", r.URL.Path)
			w.Header().Set("Retry-After", "1")
		}
		logEntry.Error = admitErr.Error()
		logEntry.StatusCode = http.StatusServiceUnavailable
		WriteError(w, logEntry.StatusCode, admitErr)
		return
	}

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

	upstreamCtx, cancel, upstreamTimeout, timeoutOverridden := p.upstreamContext(r, logEntry.Stream)
	defer cancel()
	if timeoutOverridden {
		// 客户端可覆盖上游超时；写入审计日志，否则排障时看不出本次请求为何超时阈值不同。
		logEntry.UpstreamTimeoutMS = upstreamTimeout.Milliseconds()
	}

	upstreamRequest, err := p.buildUpstreamRequest(upstreamCtx, r, gateway, subpath, adapter, requestBody, bodyLimit, &logEntry)
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
	reject := func(status int, err error) ([]byte, bool) {
		logEntry.Error = err.Error()
		logEntry.StatusCode = status
		WriteError(w, status, err)
		return nil, false
	}
	if r.ContentLength > maxBodyBytes {
		return reject(http.StatusRequestEntityTooLarge, fmt.Errorf("request body exceeds %d bytes", maxBodyBytes))
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes+1)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return reject(http.StatusRequestEntityTooLarge, fmt.Errorf("request body exceeds %d bytes", maxBodyBytes))
		}
		return reject(http.StatusBadRequest, err)
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

func (p *Proxy) upstreamContext(r *http.Request, stream bool) (context.Context, context.CancelFunc, time.Duration, bool) {
	if stream {
		// 流式响应无超时：一旦开始吐事件，超时会在事件间隙掐断长回复。
		return r.Context(), func() {}, 0, false
	}
	t, overridden := p.upstreamTimeout(r)
	ctx, cancel := context.WithTimeout(r.Context(), t)
	return ctx, cancel, t, overridden
}

func (p *Proxy) resolveGateway(r *http.Request, body []byte) (config.GatewayConfig, string, GatewayAdapter, error) {
	gateway, subpath, ok := p.Config.MatchGateway(r.URL.Path)
	if !ok {
		return config.GatewayConfig{}, "", nil, &statusError{status: http.StatusNotFound, message: fmt.Sprintf("unknown proxy path %s", r.URL.Path)}
	}
	adapter, ok := adapterForGateway(gateway)
	if !ok {
		return gateway, subpath, nil, &statusError{status: http.StatusNotImplemented, message: fmt.Sprintf("adapter not implemented for %s", gateway.ID)}
	}
	if !gateway.Enabled {
		return gateway, subpath, adapter, &statusError{status: http.StatusServiceUnavailable, message: fmt.Sprintf("gateway %s is disabled", gateway.ID)}
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
	return gateway, subpath, adapter, nil
}

func (p *Proxy) buildUpstreamRequest(ctx context.Context, r *http.Request, gateway config.GatewayConfig, subpath string, adapter GatewayAdapter, requestBody []byte, bodyLimit int64, logEntry *store.RequestLog) (*http.Request, error) {
	if strings.TrimSpace(gateway.BaseURL) == "" {
		return nil, &statusError{status: http.StatusServiceUnavailable, message: fmt.Sprintf("gateway %q has no base URL configured; set it in the console", gateway.ID)}
	}
	upstreamPath := adapter.EndpointPath()

	if upstreamPath == "" {
		upstreamPath = subpath
	}
	upstreamURL, err := joinUpstreamURL(gateway.BaseURL, upstreamPath, r.URL.RawQuery)
	if err != nil {
		return nil, &statusError{status: http.StatusBadGateway, message: err.Error()}
	}
	upstreamBody, err := adapter.TransformRequestBody(requestBody)
	if err != nil {
		return nil, &statusError{status: http.StatusBadRequest, message: err.Error()}
	}
	if gateway.Protocol == config.ProtocolResponses {
		if sessionID := grokSessionID(r.Header); sessionID != "" {
			if newBody, changed := injectPromptCacheKey(upstreamBody, sessionID); changed {
				upstreamBody = newBody
			}
		}
	}
	logEntry.UpstreamURL = upstreamURL
	logEntry.UpstreamBody = capBody(upstreamBody, bodyLimit)

	upstreamRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(upstreamBody))
	if err != nil {
		return nil, &statusError{status: http.StatusBadGateway, message: err.Error()}
	}

	buildUpstreamHeaders(upstreamRequest.Header, r.Header, gateway, logEntry.ID, logEntry.Stream)
	upstreamRequest.Header.Set("Content-Type", "application/json")
	upstreamRequest.Header.Del("Content-Length")
	if gateway.UserAgentOverrideEnabled {
		upstreamRequest.Header.Set("User-Agent", gateway.UserAgentOverride)
	}
	logEntry.UpstreamHeaders = headersJSON(upstreamRequest.Header)
	return upstreamRequest, nil
}

func buildUpstreamClients(proxyURL string) (syncProxy, syncDirect, streamProxy, streamDirect *http.Client) {
	return newSyncUpstreamClient(proxyURL), newSyncUpstreamClient(""), NewUpstreamClient(proxyURL), NewUpstreamClient("")
}

func (p *Proxy) SetProxyURL(proxyURL string) {
	syncProxy, syncDirect, streamProxy, streamDirect := buildUpstreamClients(proxyURL)
	p.clientMu.Lock()
	old := []*http.Client{p.Client, p.DirectClient, p.StreamClient, p.StreamDirectClient}
	p.Client, p.DirectClient = syncProxy, syncDirect
	p.StreamClient, p.StreamDirectClient = streamProxy, streamDirect
	p.clientMu.Unlock()
	for _, client := range old {
		if client != nil {
			client.CloseIdleConnections()
		}
	}
}

type clientKey struct {
	useProxy bool
	stream   bool
}

func (p *Proxy) ClientFor(gateway config.GatewayConfig, stream bool) *http.Client {
	p.clientMu.RLock()
	defer p.clientMu.RUnlock()
	table := map[clientKey]*http.Client{
		{true, false}:  p.Client,
		{false, false}: p.DirectClient,
		{true, true}:   p.StreamClient,
		{false, true}:  p.StreamDirectClient,
	}
	if c := table[clientKey{gateway.UseProxy, stream}]; c != nil {
		return c
	}
	if stream {
		if c := table[clientKey{gateway.UseProxy, false}]; c != nil {
			return c
		}
	}
	if p.Client != nil {
		return p.Client
	}
	return http.DefaultClient
}

func (p *Proxy) forwardUpstreamResponse(w http.ResponseWriter, logEntry *store.RequestLog, adapter GatewayAdapter, gateway config.GatewayConfig, client *http.Client, upstreamRequest *http.Request, bodyLimit int64) error {
	upstreamResponse, err := client.Do(upstreamRequest)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return &statusError{status: http.StatusGatewayTimeout, message: err.Error()}
		}
		return &statusError{status: http.StatusBadGateway, message: err.Error()}
	}
	defer upstreamResponse.Body.Close()

	logEntry.StatusCode = upstreamResponse.StatusCode
	logEntry.UpstreamResponseStatusCode = upstreamResponse.StatusCode
	logEntry.UpstreamResponseHeaders = headersJSON(upstreamResponse.Header)

	if upstreamResponse.StatusCode >= http.StatusBadRequest {
		rawError, readErr := io.ReadAll(upstreamResponse.Body)
		logEntry.ResponseBody = capBody(rawError, bodyLimit)
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
		if w.Header().Get("Content-Type") == "" {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
		}
		writeResponseStatusAndHeaders(w, upstreamResponse)
		_, _ = w.Write(rawError)
		return nil
	}

	copyResponseHeaders(w.Header(), upstreamResponse.Header)
	writeResponseStatusAndHeaders(w, upstreamResponse)

	eventStream := isEventStream(upstreamResponse.Header)
	var copyErr error
	var usage store.UsageMetrics

	if eventStream {
		rawResponse := newCappedBuffer(bodyLimit)
		responseReader := io.TeeReader(upstreamResponse.Body, rawResponse)
		responseReader = adapter.TransformSSE(responseReader)
		tracker := newUsageTracker(gateway.Protocol)
		copyErr = copyStream(w, responseReader, tracker)
		usage = tracker.usage()
		logEntry.UpstreamResponseBody = append([]byte(nil), rawResponse.Bytes()...)
		logEntry.ResponseTruncated = rawResponse.truncated
		if reporter, ok := responseReader.(streamStatsReporter); ok {
			if stats := reporter.stats(); !stats.isZero() {
				// 丢弃统计用 Info 级别：未知事件被静默丢掉是最难排障的故障形态，
				// 上游新增事件类型时必须能在默认日志级别看到计数。
				slog.Info("sse filter stats", "request_id", logEntry.ID, "stats", stats.String())
			}
		}
	} else {
		rawCapture := newCappedBufferWithHint(bodyLimit, upstreamResponse.Header.Get("Content-Length"))
		rawResponse, readErr := io.ReadAll(io.TeeReader(upstreamResponse.Body, rawCapture))
		if readErr != nil {
			copyErr = readErr
		}
		logEntry.UpstreamResponseBody = append([]byte(nil), rawCapture.Bytes()...)
		logEntry.ResponseTruncated = rawCapture.truncated

		responseBody, transformErr := adapter.TransformResponseBody(rawResponse)
		if transformErr != nil {
			copyErr = transformErr
			responseBody = rawResponse
		}
		if _, writeErr := w.Write(responseBody); copyErr == nil && writeErr != nil {
			copyErr = writeErr
		}
		usage = ExtractUsage(responseBody, gateway.Protocol)
	}

	if copyErr != nil {
		logEntry.Error = copyErr.Error()
	}
	logEntry.Success = copyErr == nil &&
		upstreamResponse.StatusCode >= 200 && upstreamResponse.StatusCode < 300
	if logEntry.Success {
		logEntry.Usage = usage
	}
	return copyErr
}

func (p *Proxy) upstreamTimeout(r *http.Request) (time.Duration, bool) {
	t := config.DefaultUpstreamTimeout
	if p.Config != nil {
		t = p.Config.GetUpstreamTimeout()
	}
	if v := r.Header.Get("X-Proxy-Timeout"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 && d <= maxUpstreamTimeout {
			return d, true
		}
	}
	return t, false
}

func (p *Proxy) responseBodyLimit() int64 {
	return resolveBodyLimit(p.ResponseBodySize, p.Config)
}

func resolveBodyLimit(override int64, cfg *config.Config) int64 {
	if override > 0 {
		return override
	}
	if cfg == nil {
		return defaultResponseBodySize
	}
	if limit := int64(cfg.GetBodyCaptureLimitKB()) << 10; limit > 0 {
		return limit
	}
	return maxBodyBytes
}

func capBody(data []byte, limit int64) []byte {
	if limit > 0 && int64(len(data)) > limit {
		return append([]byte(nil), data[:limit]...)
	}
	return append([]byte(nil), data...)
}

func (p *Proxy) finishLog(ctx context.Context, logEntry *store.RequestLog, started time.Time) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), auditWriteTimeout)
	defer cancel()
	logEntry.DurationMS = time.Since(started).Milliseconds()
	if logEntry.StatusCode == 0 {
		logEntry.StatusCode = http.StatusInternalServerError
	}
	if p.Logger != nil {
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
	}
	if p.Store != nil {
		if err := p.Store.Insert(ctx, *logEntry); err != nil && p.Logger != nil {
			p.Logger.Error("write request log", "error", err, "request_id", logEntry.ID)
		}
	}
}

func writeResponseStatusAndHeaders(w http.ResponseWriter, resp *http.Response) {
	w.WriteHeader(resp.StatusCode)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func joinUpstreamURL(base, path, rawQuery string) (string, error) {
	baseTrim := strings.TrimRight(base, "/")
	parsedBase, err := url.Parse(baseTrim)
	if err != nil || parsedBase.Scheme == "" || parsedBase.Host == "" {
		return "", fmt.Errorf("invalid upstream URL: %s", base)
	}
	basePath := parsedBase.Path
	effectivePath := path
	if basePath != "" && effectivePath != "" {
		trimmedBase := strings.Trim(basePath, "/")
		trimmedPath := strings.Trim(effectivePath, "/")
		var baseSegs []string
		var pathSegs []string
		if trimmedBase != "" {
			baseSegs = strings.Split(trimmedBase, "/")
		}
		if trimmedPath != "" {
			pathSegs = strings.Split(trimmedPath, "/")
		}
		max := len(baseSegs)
		if len(pathSegs) < max {
			max = len(pathSegs)
		}
		overlap := 0
		for n := max; n > 0; n-- {
			matched := true
			for i := 0; i < n; i++ {
				if baseSegs[len(baseSegs)-n+i] != pathSegs[i] {
					matched = false
					break
				}
			}
			if matched {
				overlap = n
				break
			}
		}
		if overlap > 0 {
			remaining := pathSegs[overlap:]
			if len(remaining) == 0 {
				effectivePath = ""
			} else {
				effectivePath = "/" + strings.Join(remaining, "/")
			}
		}
	}
	parsedBase.Path = strings.TrimRight(basePath, "/") + effectivePath
	parsedBase.RawQuery = rawQuery
	return parsedBase.String(), nil
}

func protocolForPath(path string) config.Protocol {
	switch path {
	case "/responses":
		return config.ProtocolResponses
	case "/chat/completions":
		return config.ProtocolChat
	case "/v1/messages", "/messages":
		return config.ProtocolAnthropic
	default:
		return config.ProtocolChat
	}
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
