package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"grok-gateway-proxy/internal/config"
	"grok-gateway-proxy/internal/redact"
)

var hopByHopHeaders = map[string]bool{
	"connection":          true,
	"keep-alive":          true,
	"proxy-authenticate":  true,
	"proxy-authorization": true,
	"te":                  true,
	"trailer":             true,
	"transfer-encoding":   true,
	"upgrade":             true,
	"host":                true,
	"content-length":      true,
}

// alwaysStripRequestHeaders 是在任何情况下都不会转发给上游的请求头。
//
// 与 hopByHopHeaders 的区别：hop-by-hop 头是连接级的（响应方向同样要剥离），
// 这里的头是端到端语义的，只因为本代理必须读取并改写正文才不能透传。
//
// Accept-Encoding / Content-Encoding 必须剥离：代理要对响应正文做 JSON 重写与
// SSE 事件过滤，必须拿到明文。Go 的 http.Transport 只在请求未自带 Accept-Encoding
// 时才自动追加 "gzip" 并透明解压；一旦请求携带该头（例如客户端的
// "gzip, br, deflate"），Transport 就原样透传，且 brotli 它根本不会解码。
// 结果是 TransformResponseBody / TransformSSE 收到压缩字节，SSE 事件被静默丢弃。
// 因此这里主动剥离，把压缩协商交回 Transport（它会用自己能解码的 gzip）。
//
// 该集合优先于 forward_headers：即使用户在控制台把 Accept-Encoding 加进白名单，
// 也会被丢弃，否则一次配置失误就会静默损坏所有响应。
var alwaysStripRequestHeaders = map[string]bool{
	"accept-encoding":  true,
	"content-encoding": true,
	"content-length":   true,
	"host":             true,
}

// defaultForwardHeaders 是未配置 forward_headers 时使用的默认值。
//
// 收录原则：标准头、或有明确标准对应物、且转发对上游有收益。
// Traceparent / Tracestate 是 W3C Trace Context 标准（客户端经 OTel 注入），
// 转发后可打通端到端链路，无需翻译，故收录。
var defaultForwardHeaders = []string{
	"Accept",
	"User-Agent",
	"X-Request-Id",
	"X-Correlation-Id",
	"X-Client-Request-Id",
	"X-Session-Id",
	"session_id",
	"x-opencode-session",
	"Traceparent",
	"Tracestate",
}

func copyForwardHeaders(dst http.Header, src http.Header, allowlist []string) {
	allowed := make(map[string]bool, len(allowlist))
	for _, name := range allowlist {
		allowed[strings.ToLower(name)] = true
	}
	for name, values := range src {
		lower := strings.ToLower(name)
		if hopByHopHeaders[lower] || alwaysStripRequestHeaders[lower] || !allowed[lower] {
			continue
		}
		for _, value := range values {
			dst.Add(name, value)
		}
	}
}

// stripUnforwardableRequestHeaders 兜底清理：即便 allowlist 里显式写了这些头也不转发。
func stripUnforwardableRequestHeaders(dst http.Header) {
	for name := range dst {
		if alwaysStripRequestHeaders[strings.ToLower(name)] {
			dst.Del(name)
		}
	}
}

func grokSessionID(src http.Header) string {
	if v := strings.TrimSpace(src.Get("X-Grok-Session-Id")); v != "" {
		return v
	}
	return strings.TrimSpace(src.Get("X-Grok-Conv-Id"))
}

func grokRequestID(src http.Header) string {
	return strings.TrimSpace(src.Get("X-Grok-Req-Id"))
}

func clampPromptCacheKey(s string) string {
	if s == "" {
		return s
	}
	runes := []rune(s)
	if len(runes) <= 64 {
		return s
	}
	return string(runes[:64])
}

// injectPromptCacheKey 把会话 ID 作为 prompt_cache_key 注入请求体，用于上游的提示缓存路由。
//
// 与会话亲和头（session_id / x-client-request-id）是两套独立机制：亲和头由
// session_affinity 控制，prompt_cache_key 由缓存保留策略控制，两者互不影响，故同时发送。
//
// 采用保序插入而非 map 反序列化：map[string]json.RawMessage 重新序列化会把所有顶层键
// 按字母序重排，使上游收到的正文与客户端发出的字节完全不同，破坏"无扩展请求逐字节透传"
// 的承诺，也让审计日志里的 body 与客户端原始 body 无法对照。
//
// 返回 (新 body, 是否修改)。未修改时返回原 slice。
func injectPromptCacheKey(body []byte, sessionID string) ([]byte, bool) {
	sessionID = clampPromptCacheKey(strings.TrimSpace(sessionID))
	if sessionID == "" {
		return body, false
	}
	if len(bytes.TrimSpace(body)) == 0 || !bytes.Contains(body, []byte("{")) {
		return body, false
	}
	encoded, err := json.Marshal(sessionID)
	if err != nil {
		return body, false
	}
	return insertTopLevelJSONMember(body, "prompt_cache_key", encoded)
}

func buildUpstreamHeaders(dst http.Header, src http.Header, gateway config.GatewayConfig, requestID string, stream bool) {
	allowlist := gateway.ForwardHeaders
	if len(allowlist) == 0 {
		allowlist = defaultForwardHeaders
	}
	copyForwardHeaders(dst, src, allowlist)
	applyGrokHeaderTranslations(dst, src, gateway, requestID, stream)
	// 必须在所有写入之后执行：翻译阶段只写白名单外的标准头，不会引入这些头，
	// 但放在最后可以确保无论 allowlist 怎么配置都不会把压缩协商类头带上上游。
	stripUnforwardableRequestHeaders(dst)
}

func applyGrokHeaderTranslations(dst http.Header, src http.Header, gateway config.GatewayConfig, requestID string, stream bool) {

	applySessionAffinity(dst, gateway.Protocol, gateway.EffectiveSessionAffinity(), grokSessionID(src))

	if isOpenCodeBaseURL(gateway.BaseURL) || gateway.EffectiveSessionAffinity() == config.SessionAffinityOpenCode {
		applyOpenCodeSession(dst, src)
	}

	if reqID := grokRequestID(src); reqID != "" && dst.Get("X-Correlation-Id") == "" {
		dst.Set("X-Correlation-Id", reqID)
	}
	if requestID != "" && dst.Get("X-Request-Id") == "" {
		dst.Set("X-Request-Id", requestID)
	}

	if dst.Get("Authorization") == "" {

		if v := strings.TrimSpace(src.Get("Authorization")); v != "" {
			dst.Set("Authorization", v)
		}
	}
	if dst.Get("X-Api-Key") == "" {
		if v := strings.TrimSpace(src.Get("X-Api-Key")); v != "" {
			dst.Set("X-Api-Key", v)
		}
	}

	if gateway.Protocol == config.ProtocolAnthropic && dst.Get("X-Api-Key") == "" {
		if auth := strings.TrimSpace(dst.Get("Authorization")); auth != "" {
			lower := strings.ToLower(auth)
			if strings.HasPrefix(lower, "bearer ") {
				if token := strings.TrimSpace(auth[len("bearer "):]); token != "" {
					dst.Set("X-Api-Key", token)
				}
			}
		}
	}

	if dst.Get("User-Agent") == "" {
		if v := strings.TrimSpace(src.Get("User-Agent")); v != "" {
			dst.Set("User-Agent", v)
		} else if v := strings.TrimSpace(src.Get("X-Grok-Client-Identifier")); v != "" {
			if ver := strings.TrimSpace(src.Get("X-Grok-Client-Version")); ver != "" {
				dst.Set("User-Agent", v+"/"+ver)
			} else {
				dst.Set("User-Agent", v)
			}
		} else {
			dst.Set("User-Agent", config.DefaultUserAgentOverride)
		}
	}
	// Accept 按协议分派，而不是"客户端优先、缺省才合成"。
	//
	// Anthropic Messages API 不支持用 Accept 选择 SSE：流式与否由正文的 stream 字段决定，
	// 客户端库对 Messages 端点一律发送 accept: application/json（含流式请求）。
	// Grok Build 的 Anthropic 后端却会带 accept: text/event-stream（它对所有后端统一处理），
	// 若原样转发，上游可能返回 406 或忽略。因此这里无条件覆盖。
	//
	// 其余协议（Responses / Chat Completions）客户端的 Accept 是准确的，保留原值，缺失时才合成。
	if gateway.Protocol == config.ProtocolAnthropic {
		dst.Set("Accept", "application/json")
	} else if dst.Get("Accept") == "" {
		if stream {
			dst.Set("Accept", "text/event-stream")
		} else {
			dst.Set("Accept", "application/json")
		}
	}

	if gateway.Protocol == config.ProtocolAnthropic {
		for _, name := range []string{"Anthropic-Version", "Anthropic-Beta", "anthropic-dangerous-direct-browser-access", "anthropic-beta"} {
			if dst.Get(name) == "" {
				if v := strings.TrimSpace(src.Get(name)); v != "" {
					dst.Set(name, v)
				}
			}
		}
	}

	if gateway.Protocol == config.ProtocolAnthropic {
		if dst.Get("Anthropic-Version") == "" {
			dst.Set("Anthropic-Version", "2023-06-01")
		}
		if dst.Get("anthropic-dangerous-direct-browser-access") == "" {
			dst.Set("anthropic-dangerous-direct-browser-access", "true")
		}
		// 不再无条件写入 Anthropic-Beta。
		//
		// beta 开关会改变上游行为（interleaved-thinking 会切换推理模式），客户端库是
		// 按请求内容条件累加的（OAuth 身份、细粒度工具流、交错思考、服务端回退…），
		// 从不无条件强制。代理无法从请求体可靠推断应当开启哪些 beta，
		// 强制开启等于替上游做决定；需要时由调用方自行通过 forward_headers 带上。
	} else {
		dst.Del("Anthropic-Version")
		dst.Del("Anthropic-Beta")
		dst.Del("anthropic-dangerous-direct-browser-access")
		dst.Del("anthropic-beta")
	}
}

// applySessionAffinity 把 Grok 的会话 ID 翻译成上游生态认识的会话亲和头。
//
// 各协议的"标准"会话头并不相同，实测参考实现按协议分派：
//   - Responses   ：session_id + x-client-request-id
//   - Chat Completions：session_id + x-client-request-id + x-session-affinity
//   - Anthropic   ：仅 x-session-affinity
//
// x-client-request-id 承载会话 ID 而非每请求 ID，是 OpenAI/Codex 兼容端点的既有约定
// （不是笔误）：它们成对接收 session_id 与 x-client-request-id 用于缓存路由。
// 每请求 ID 走 X-Request-Id / X-Correlation-Id，与会话头互不干扰。
//
// 这些头与正文的 prompt_cache_key 是两套独立机制（亲和头管路由、prompt_cache_key 管
// 缓存保留），因此同时发送，不视为重复。
func applySessionAffinity(dst http.Header, protocol config.Protocol, mode, sessionID string) {
	if sessionID == "" {
		return
	}
	switch mode {
	case config.SessionAffinityOpenRouter:
		if dst.Get("X-Session-Id") == "" {
			dst.Set("X-Session-Id", sessionID)
		}
	case config.SessionAffinityOpenCode:
		if dst.Get("x-opencode-session") == "" {
			dst.Set("x-opencode-session", sessionID)
		}
	case config.SessionAffinityOff:
		return
	case config.SessionAffinityOpenAI:
		fallthrough
	default:
		switch protocol {
		case config.ProtocolAnthropic:
			// Anthropic 生态只用 x-session-affinity；session_id / x-client-request-id
			// 对它无意义，发了只是噪声。
			if dst.Get("X-Session-Affinity") == "" {
				dst.Set("X-Session-Affinity", sessionID)
			}
		case config.ProtocolChat, config.ProtocolOpenAICompatible:
			if dst.Get("session_id") == "" {
				dst.Set("session_id", sessionID)
			}
			if dst.Get("X-Client-Request-Id") == "" {
				dst.Set("X-Client-Request-Id", sessionID)
			}
			if dst.Get("X-Session-Affinity") == "" {
				dst.Set("X-Session-Affinity", sessionID)
			}
		default:
			// Responses（含 DeepSeek 与自定义网关）
			if dst.Get("session_id") == "" {
				dst.Set("session_id", sessionID)
			}
			if dst.Get("X-Client-Request-Id") == "" {
				dst.Set("X-Client-Request-Id", sessionID)
			}
		}
	}
}

func isOpenCodeBaseURL(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		host := raw
		if idx := strings.Index(host, "://"); idx != -1 {
			host = host[idx+3:]
		}
		if idx := strings.Index(host, "/"); idx != -1 {
			host = host[:idx]
		}
		if idx := strings.Index(host, ":"); idx != -1 {
			host = host[:idx]
		}
		host = strings.ToLower(strings.TrimSpace(host))
		return host == "opencode.ai" || strings.HasSuffix(host, ".opencode.ai")
	}
	host := strings.ToLower(u.Hostname())
	return host == "opencode.ai" || strings.HasSuffix(host, ".opencode.ai")
}

func getHeaderCaseInsensitive(h http.Header, key string) string {
	if h == nil {
		return ""
	}
	if v := strings.TrimSpace(h.Get(key)); v != "" {
		return v
	}
	lowerKey := strings.ToLower(key)
	for k, vals := range h {
		if strings.ToLower(k) == lowerKey {
			for _, val := range vals {
				if trimmed := strings.TrimSpace(val); trimmed != "" {
					return trimmed
				}
			}
		}
	}
	return ""
}

// resolveOpenCodeSessionID 逐级降级取一个会话 ID。
//
// 刻意不把审计 requestID 作为兜底：requestID 每次请求都不同，把它当会话 ID 发出去等于
// 告诉上游"每次都是新会话"，比不发更糟（不发时上游至少还能按连接/密钥自行亲和）。
// 因此兜底只有一档：生成一个稳定的 sess-* 值。
func resolveOpenCodeSessionID(src http.Header) string {
	if v := getHeaderCaseInsensitive(src, "x-opencode-session"); v != "" {
		return v
	}
	if v := grokSessionID(src); v != "" {
		return v
	}
	if v := getHeaderCaseInsensitive(src, "X-Session-Id"); v != "" {
		return v
	}
	if v := getHeaderCaseInsensitive(src, "session_id"); v != "" {
		return v
	}
	if v := getHeaderCaseInsensitive(src, "X-Client-Request-Id"); v != "" {
		return v
	}
	return "sess-" + newRequestID()
}

func applyOpenCodeSession(dst http.Header, src http.Header) {
	if getHeaderCaseInsensitive(dst, "x-opencode-session") != "" {
		return
	}
	sessionID := resolveOpenCodeSessionID(src)
	if sessionID != "" {
		dst.Set("x-opencode-session", sessionID)
	}
	// x-opencode-client 与 x-opencode-session 是配对归因头（参考实现总是成对发送）。
	// 客户端不发这两个头，代理在此补全；取值翻译自 x-grok-client-identifier
	// （客户端默认 grok-shell），而不是照抄参考实现的 "pi"——那会冒充别的客户端。
	if getHeaderCaseInsensitive(dst, "x-opencode-client") != "" {
		return
	}
	if v := strings.TrimSpace(src.Get("X-Grok-Client-Identifier")); v != "" {
		dst.Set("x-opencode-client", v)
	}
}

func sanitizeHeaders(src http.Header) http.Header {
	dst := make(http.Header, len(src))
	for name, values := range src {
		for _, value := range values {
			if redact.SensitiveHeader(name) {
				dst.Add(name, "[REDACTED]")
			} else {
				dst.Add(name, value)
			}
		}
	}
	return dst
}

func headersJSON(headers http.Header) string {
	return marshalJSON(sanitizeHeaders(headers))
}

func marshalJSON(value any) string {
	b, err := json.Marshal(value)
	if err != nil {
		return "{}"
	}
	return string(b)
}
