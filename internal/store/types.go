package store

import (
	"encoding/json"
	"time"

	"grok-gateway-proxy/internal/config"
)

type UsageMetrics struct {
	InputTokens      int64  `json:"input_tokens"`
	CacheReadTokens  int64  `json:"cache_read_tokens"`
	CacheWriteTokens int64  `json:"cache_write_tokens"`
	PromptTokens     int64  `json:"prompt_tokens"`
	OutputTokens     int64  `json:"output_tokens"`
	ReasoningTokens  int64  `json:"reasoning_tokens"`
	CacheSupported   bool   `json:"cache_supported"`
	CacheSource      string `json:"cache_source,omitempty"`
	UsagePresent     bool   `json:"usage_present"`
}

type RequestLog struct {
	ID                         string          `json:"id"`
	StartedAt                  time.Time       `json:"started_at"`
	GatewayID                  string          `json:"gateway_id"`
	GatewayName                string          `json:"gateway_name"`
	Prefix                     string          `json:"prefix"`
	IngressProtocol            config.Protocol `json:"ingress_protocol"`
	UpstreamProtocol           config.Protocol `json:"upstream_protocol"`
	Model                      string          `json:"model"`
	RequestPath                string          `json:"request_path"`
	RequestURL                 string          `json:"request_url"`
	UpstreamURL                string          `json:"upstream_url"`
	Method                     string          `json:"method"`
	StatusCode                 int             `json:"status_code"`
	ClientResponseStatusCode   int             `json:"client_response_status_code"`
	UpstreamResponseStatusCode int             `json:"upstream_response_status_code"`
	Success                    bool            `json:"success"`
	Stream                     bool            `json:"stream"`
	DurationMS                 int64           `json:"duration_ms"`
	UpstreamTimeoutMS          int64           `json:"upstream_timeout_ms,omitempty"`
	RequestHeaders             string          `json:"request_headers"`
	RequestBody                []byte          `json:"request_body"`
	UpstreamHeaders            string          `json:"upstream_headers"`
	UpstreamBody               []byte          `json:"upstream_body"`
	UpstreamResponseHeaders    string          `json:"upstream_response_headers"`
	UpstreamResponseBody       []byte          `json:"upstream_response_body"`
	ResponseHeaders            string          `json:"response_headers"`
	ResponseBody               []byte          `json:"response_body"`
	ResponseTruncated          bool            `json:"response_truncated,omitempty"`
	Error                      string          `json:"error,omitempty"`
	Usage                      UsageMetrics    `json:"usage"`
}

type Metrics struct {
	From                 *time.Time         `json:"from,omitempty"`
	To                   *time.Time         `json:"to,omitempty"`
	GatewayID            string             `json:"gateway_id,omitempty"`
	Model                string             `json:"model,omitempty"`
	Requests             int64              `json:"requests"`
	Successes            int64              `json:"successes"`
	Failures             int64              `json:"failures"`
	InputTokens          int64              `json:"input_tokens"`
	OutputTokens         int64              `json:"output_tokens"`
	ReasoningTokens      int64              `json:"reasoning_tokens"`
	PromptTokens         int64              `json:"prompt_tokens"`
	CachePromptTokens    int64              `json:"cache_prompt_tokens"`
	CacheReadTokens      int64              `json:"cache_read_tokens"`
	CacheWriteTokens     int64              `json:"cache_write_tokens"`
	CacheHitRate         *float64           `json:"cache_hit_rate,omitempty"`
	CacheSupportedCalls  int64              `json:"cache_supported_calls"`
	UsageCalls           int64              `json:"usage_calls"`
	CacheCoveragePercent *float64           `json:"cache_coverage_percent,omitempty"`
	ByGateway            map[string]Metrics `json:"by_gateway,omitempty"`
}

func (l RequestLog) MarshalJSON() ([]byte, error) {
	type alias RequestLog
	return json.Marshal(&struct {
		RequestBody          string `json:"request_body"`
		UpstreamBody         string `json:"upstream_body"`
		UpstreamResponseBody string `json:"upstream_response_body"`
		ResponseBody         string `json:"response_body"`
		*alias
	}{
		RequestBody:          string(l.RequestBody),
		UpstreamBody:         string(l.UpstreamBody),
		UpstreamResponseBody: string(l.UpstreamResponseBody),
		ResponseBody:         string(l.ResponseBody),
		alias:                (*alias)(&l),
	})
}

type LogFilter struct {
	GatewayID string
	Model     string
	Status    string
	From      *time.Time
	To        *time.Time
	Limit     int
	Offset    int
}
