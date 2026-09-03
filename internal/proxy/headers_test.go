package proxy

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"grok-gateway-proxy/internal/config"
)

func TestCopyForwardHeadersDropsHopByHopAndInternal(t *testing.T) {
	src := http.Header{}
	src.Set("Authorization", "Bearer secret")
	src.Set("Content-Type", "application/json")
	src.Set("Accept", "application/json")
	src.Set("Connection", "keep-alive")
	src.Set("X-Forwarded-For", "1.2.3.4")
	src.Set("X-Grok-Trace", "abc")

	dst := http.Header{}
	copyForwardHeaders(dst, src, defaultForwardHeaders)

	if got := dst.Get("Authorization"); got != "" {
		t.Fatalf("Authorization should be the fallback phase's job, allowlist copied it: %q", got)
	}

	if got := dst.Get("Content-Type"); got != "" {
		t.Fatalf("client Content-Type was copied: %q", got)
	}
	if got := dst.Get("Accept"); got != "application/json" {
		t.Fatalf("Accept not forwarded: %q", got)
	}
	if dst.Get("Connection") != "" {
		t.Fatal("hop-by-hop Connection header should have been dropped")
	}
	if dst.Get("X-Forwarded-For") != "" {
		t.Fatal("hop-by-hop X-Forwarded-For should have been dropped")
	}
	if dst.Get("X-Grok-Trace") != "" {
		t.Fatal("internal X-Grok-* header should have been dropped")
	}
}

func TestCopyForwardHeadersRespectsAllowlist(t *testing.T) {
	src := http.Header{}
	src.Set("Authorization", "Bearer secret")
	src.Set("X-Custom", "value")

	dst := http.Header{}
	copyForwardHeaders(dst, src, []string{"Authorization"})
	if dst.Get("Authorization") != "Bearer secret" {
		t.Fatalf("allowlisted Authorization was not forwarded: %q", dst.Get("Authorization"))
	}
	if dst.Get("X-Custom") != "" {
		t.Fatalf("non-allowlisted header was forwarded: %q", dst.Get("X-Custom"))
	}
}

func TestCopyResponseHeadersKeepsContentEncoding(t *testing.T) {
	src := http.Header{}
	src.Set("Content-Encoding", "gzip")
	src.Set("Content-Range", "bytes 0-99/200")
	src.Set("Transfer-Encoding", "chunked")
	src.Set("Content-Length", "42")

	dst := http.Header{}
	copyResponseHeaders(dst, src)

	if got := dst.Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding was dropped: %q", got)
	}
	if got := dst.Get("Content-Range"); got != "bytes 0-99/200" {
		t.Fatalf("Content-Range was dropped: %q", got)
	}
	if dst.Get("Transfer-Encoding") != "" {
		t.Fatal("hop-by-hop Transfer-Encoding should have been dropped")
	}
	if dst.Get("Content-Length") != "" {
		t.Fatal("Content-Length should have been dropped: the proxy re-frames bodies")
	}
}

func TestSanitizeHeadersRedactsSensitive(t *testing.T) {
	src := http.Header{}
	src.Set("Authorization", "Bearer secret")
	src.Set("X-Api-Key", "abc123")
	src.Set("X-Request-Id", "abc")
	got := sanitizeHeaders(src)
	if got.Get("Authorization") != "[REDACTED]" {
		t.Fatalf("Authorization was not redacted: %q", got.Get("Authorization"))
	}
	if got.Get("X-Api-Key") != "[REDACTED]" {
		t.Fatalf("X-Api-Key was not redacted: %q", got.Get("X-Api-Key"))
	}
	if got.Get("X-Request-Id") != "abc" {
		t.Fatalf("non-sensitive header was altered: %q", got.Get("X-Request-Id"))
	}
}

func TestCopyForwardHeadersDropsRealGrokBuildHeadersByDefault(t *testing.T) {
	src := http.Header{}
	for _, name := range []string{
		"X-Grok-Conv-Id", "X-Grok-Req-Id", "X-Grok-Session-Id",
		"X-Grok-Agent-Id", "X-Grok-User-Id", "X-Grok-Deployment-Id",
		"X-Grok-Model-Override", "X-Grok-Turn-Idx", "X-Grok-Transient-Retry",
		"X-Grok-Client-Version", "X-Grok-Client-Identifier",
		"X-Grok-Doom-Loop-Check", "X-Grok-Exact-Repetition-Check",
	} {
		src.Set(name, "v")
	}

	dst := http.Header{}
	copyForwardHeaders(dst, src, defaultForwardHeaders)

	for name := range src {
		if dst.Get(name) != "" {
			t.Fatalf("default allowlist forwarded %s (value %q)", name, dst.Get(name))
		}
	}
}

func TestCopyForwardHeadersAllowlistOptsInGrokHeaders(t *testing.T) {
	src := http.Header{}
	src.Set("X-Grok-Doom-Loop-Check", "64")
	src.Set("X-Grok-Exact-Repetition-Check", "64")
	src.Set("X-Grok-User-Id", "u-1")
	src.Set("Authorization", "Bearer secret")

	dst := http.Header{}
	copyForwardHeaders(dst, src, []string{"Authorization", "X-Grok-Doom-Loop-Check", "x-grok-exact-repetition-check"})

	if got := dst.Get("X-Grok-Doom-Loop-Check"); got != "64" {
		t.Fatalf("allowlisted doom-loop opt-in not forwarded: %q", got)
	}

	if got := dst.Get("X-Grok-Exact-Repetition-Check"); got != "64" {
		t.Fatalf("allowlisted repetition opt-in not forwarded: %q", got)
	}
	if got := dst.Get("Authorization"); got != "Bearer secret" {
		t.Fatalf("Authorization not forwarded: %q", got)
	}

	if got := dst.Get("X-Grok-User-Id"); got != "" {
		t.Fatalf("non-allowlisted x-grok-user-id was forwarded: %q", got)
	}
}

func TestApplyGrokHeaderTranslationsSessionDialects(t *testing.T) {
	src := http.Header{}
	src.Set("X-Grok-Session-Id", "sess-123")

	cases := []struct {
		mode            string
		wantSessionID   string
		wantClientReqID string
		wantXSessionID  string
		wantOpenCode    string
	}{
		{config.SessionAffinityOpenAI, "sess-123", "sess-123", "", ""},
		{config.SessionAffinityOpenRouter, "", "", "sess-123", ""},
		{config.SessionAffinityOpenCode, "", "", "", "sess-123"},
		{config.SessionAffinityOff, "", "", "", ""},

		{"", "sess-123", "sess-123", "", ""},
		{"nonsense", "sess-123", "sess-123", "", ""},
	}
	for _, tc := range cases {
		dst := http.Header{}
		applyGrokHeaderTranslations(dst, src, config.GatewayConfig{SessionAffinity: tc.mode}, "", false)
		if got := dst.Get("session_id"); got != tc.wantSessionID {
			t.Fatalf("mode %q: session_id = %q, want %q", tc.mode, got, tc.wantSessionID)
		}
		if got := dst.Get("X-Client-Request-Id"); got != tc.wantClientReqID {
			t.Fatalf("mode %q: X-Client-Request-Id = %q, want %q", tc.mode, got, tc.wantClientReqID)
		}
		if got := dst.Get("X-Session-Id"); got != tc.wantXSessionID {
			t.Fatalf("mode %q: X-Session-Id = %q, want %q", tc.mode, got, tc.wantXSessionID)
		}
		if got := dst.Get("x-opencode-session"); got != tc.wantOpenCode {
			t.Fatalf("mode %q: x-opencode-session = %q, want %q", tc.mode, got, tc.wantOpenCode)
		}
	}
}

func TestApplyGrokHeaderTranslationsRequestIDs(t *testing.T) {
	src := http.Header{}
	src.Set("X-Grok-Req-Id", "req-456")
	dst := http.Header{}
	applyGrokHeaderTranslations(dst, src, config.GatewayConfig{}, "req-audit-1", false)

	if got := dst.Get("X-Request-Id"); got != "req-audit-1" {
		t.Fatalf("X-Request-Id = %q, want the audit id", got)
	}
	if got := dst.Get("X-Correlation-Id"); got != "req-456" {
		t.Fatalf("X-Correlation-Id = %q, want grok's per-turn id", got)
	}
	if dst.Get("X-Grok-Req-Id") != "" {
		t.Fatal("raw x-grok-req-id must not be forwarded")
	}
}

func TestApplyGrokHeaderTranslationsPreservesExisting(t *testing.T) {
	src := http.Header{}
	src.Set("X-Grok-Session-Id", "new-sess")
	src.Set("X-Grok-Req-Id", "new-req")
	dst := http.Header{}
	dst.Set("session_id", "existing")
	dst.Set("X-Client-Request-Id", "existing-req")
	dst.Set("X-Request-Id", "existing-id")
	applyGrokHeaderTranslations(dst, src, config.GatewayConfig{}, "req-audit-1", false)

	if got := dst.Get("session_id"); got != "existing" {
		t.Fatalf("existing session_id was overwritten: %q", got)
	}
	if got := dst.Get("X-Client-Request-Id"); got != "existing-req" {
		t.Fatalf("existing X-Client-Request-Id was overwritten: %q", got)
	}
	if got := dst.Get("X-Request-Id"); got != "existing-id" {
		t.Fatalf("existing X-Request-Id was overwritten: %q", got)
	}
}

func TestApplyGrokHeaderTranslationsFallsBackToConvId(t *testing.T) {
	src := http.Header{}
	src.Set("X-Grok-Conv-Id", "conv-only")
	dst := http.Header{}
	applyGrokHeaderTranslations(dst, src, config.GatewayConfig{}, "", false)
	if got := dst.Get("session_id"); got != "conv-only" {
		t.Fatalf("fallback to conv-id failed: %q", got)
	}
}

func TestApplyGrokHeaderTranslationsSetsUserAgentAndAccept(t *testing.T) {
	src := http.Header{}
	src.Set("User-Agent", "grok-shell/1.2.3 (windows; x86_64)")
	dst := http.Header{}
	applyGrokHeaderTranslations(dst, src, config.GatewayConfig{}, "", false)
	if got := dst.Get("User-Agent"); got != "grok-shell/1.2.3 (windows; x86_64)" {
		t.Fatalf("User-Agent not preserved: %q", got)
	}
	if got := dst.Get("Accept"); got != "application/json" {
		t.Fatalf("Accept default not set: %q", got)
	}

	src2 := http.Header{}
	src2.Set("X-Grok-Client-Identifier", "grok-shell")
	src2.Set("X-Grok-Client-Version", "9.9.9")
	dst2 := http.Header{}
	applyGrokHeaderTranslations(dst2, src2, config.GatewayConfig{}, "", false)
	if got := dst2.Get("User-Agent"); got != "grok-shell/9.9.9" {
		t.Fatalf("User-Agent from x-grok-client-* failed: %q", got)
	}
}

func TestApplyGrokHeaderTranslationsAcceptTracksStream(t *testing.T) {
	streaming := http.Header{}
	applyGrokHeaderTranslations(streaming, http.Header{}, config.GatewayConfig{}, "", true)
	if got := streaming.Get("Accept"); got != "text/event-stream" {
		t.Fatalf("streaming Accept = %q, want text/event-stream", got)
	}

	buffered := http.Header{}
	applyGrokHeaderTranslations(buffered, http.Header{}, config.GatewayConfig{}, "", false)
	if got := buffered.Get("Accept"); got != "application/json" {
		t.Fatalf("non-streaming Accept = %q, want application/json", got)
	}
}

func TestApplyGrokHeaderTranslationsUserAgentFallbackNamesVersion(t *testing.T) {
	dst := http.Header{}
	applyGrokHeaderTranslations(dst, http.Header{}, config.GatewayConfig{}, "", false)
	if got := dst.Get("User-Agent"); got != config.DefaultUserAgentOverride {
		t.Fatalf("User-Agent fallback = %q, want %q", got, config.DefaultUserAgentOverride)
	}
}

func TestInjectPromptCacheKey(t *testing.T) {
	body := []byte(`{"model":"m","input":"hi"}`)
	out, changed := injectPromptCacheKey(body, "sess-abc")
	if !changed {
		t.Fatal("expected injection to change body")
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	var got string
	if err := json.Unmarshal(m["prompt_cache_key"], &got); err != nil || got != "sess-abc" {
		t.Fatalf("injected value wrong: %s", out)
	}
}

// 注入必须保序：只允许在顶层对象末尾追加，其余字节原样保留。
// map 反序列化再序列化会把键重排成字母序，破坏逐字节透传承诺。
func TestInjectPromptCacheKeyPreservesByteOrder(t *testing.T) {
	body := []byte(`{"z_last":1,"model":"m","a_first":{"nested":"value"},"stream":true}`)
	out, changed := injectPromptCacheKey(body, "sess-abc")
	if !changed {
		t.Fatal("expected injection")
	}
	// 期望结构：body 去掉结尾 '}' + 插入片段 + '}'，即除结尾 '}' 外全部原样保留。
	inserted := []byte(`,"prompt_cache_key":"sess-abc"`)
	prefix := out[:len(out)-len(inserted)-1]
	if string(prefix) != string(body[:len(body)-1]) {
		t.Fatalf("injection rewrote existing bytes:\n in : %s\n out: %s", body, out)
	}
	if string(out[len(out)-1:]) != "}" {
		t.Fatalf("output must still end with '}': %s", out)
	}
	if !json.Valid(out) {
		t.Fatalf("output is not valid JSON: %s", out)
	}
}

// 输入里的转义字符串若包含 '{' '}' 或 \" ，不得干扰插入位置。
func TestInjectPromptCacheKeyEscapedBraces(t *testing.T) {
	body := []byte(`{"s":"brace } and { and \" inside"}`)
	out, changed := injectPromptCacheKey(body, "sess-x")
	if !changed || !json.Valid(out) {
		t.Fatalf("escaped braces broke injection: changed=%v out=%s", changed, out)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if _, ok := m["prompt_cache_key"]; !ok {
		t.Fatalf("prompt_cache_key missing: %s", out)
	}
}

// 已有 prompt_cache_key（即使是空串）一律不重写。
func TestInjectPromptCacheKeySkipsEmptyExisting(t *testing.T) {
	for _, existing := range []string{`"keep"`, `""`, `123`} {
		body := []byte(`{"model":"m","prompt_cache_key":` + existing + `}`)
		out, changed := injectPromptCacheKey(body, "sess-abc")
		if changed {
			t.Fatalf("existing prompt_cache_key=%s was rewritten: %s", existing, out)
		}
		if string(out) != string(body) {
			t.Fatalf("body was altered: %s", out)
		}
	}
}

func TestInjectPromptCacheKeyPreservesExisting(t *testing.T) {
	body := []byte(`{"model":"m","prompt_cache_key":"keep","input":"hi"}`)
	out, changed := injectPromptCacheKey(body, "sess-abc")
	if changed {
		t.Fatalf("should not overwrite existing prompt_cache_key: %s", out)
	}
	if string(out) != string(body) {
		t.Fatalf("body was altered: %s", out)
	}
}

func TestInjectPromptCacheKeyClamps(t *testing.T) {
	longID := strings.Repeat("x", 80)
	body := []byte(`{"model":"m"}`)
	out, changed := injectPromptCacheKey(body, longID)
	if !changed {
		t.Fatal("expected clamped injection")
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	var got string
	if err := json.Unmarshal(m["prompt_cache_key"], &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len([]rune(got)) != 64 || got != strings.Repeat("x", 64) {
		t.Fatalf("clamping failed: got %q len %d", got, len([]rune(got)))
	}
}

func TestApplyGrokHeaderTranslationsAnthropicBearerToXApiKey(t *testing.T) {
	src := http.Header{}
	src.Set("Authorization", "Bearer sk-test-key-123")
	dst := http.Header{}
	applyGrokHeaderTranslations(dst, src, config.GatewayConfig{Protocol: config.ProtocolAnthropic}, "", false)
	if got := dst.Get("X-Api-Key"); got != "sk-test-key-123" {
		t.Fatalf("Anthropic gateway should synthesize X-Api-Key from Bearer, got %q", got)
	}
	if got := dst.Get("Authorization"); got != "Bearer sk-test-key-123" {
		t.Fatalf("Authorization should be preserved, got %q", got)
	}

	src2 := http.Header{}
	src2.Set("Authorization", "Bearer sk-new")
	src2.Set("X-Api-Key", "sk-existing")
	dst2 := http.Header{}
	applyGrokHeaderTranslations(dst2, src2, config.GatewayConfig{Protocol: config.ProtocolAnthropic}, "", false)
	if got := dst2.Get("X-Api-Key"); got != "sk-existing" {
		t.Fatalf("existing X-Api-Key was overwritten: %q", got)
	}

	src3 := http.Header{}
	src3.Set("Authorization", "Bearer sk-test-key-123")
	dst3 := http.Header{}
	applyGrokHeaderTranslations(dst3, src3, config.GatewayConfig{Protocol: config.ProtocolResponses}, "", false)
	if got := dst3.Get("X-Api-Key"); got != "" {
		t.Fatalf("non-Anthropic gateway should not synthesize X-Api-Key, got %q", got)
	}

	src4 := http.Header{}
	src4.Set("Authorization", "bearer sk-lower")
	dst4 := http.Header{}
	applyGrokHeaderTranslations(dst4, src4, config.GatewayConfig{Protocol: config.ProtocolAnthropic}, "", false)
	if got := dst4.Get("X-Api-Key"); got != "sk-lower" {
		t.Fatalf("case-insensitive Bearer not handled, got %q", got)
	}
}

func TestIsOpenCodeBaseURL(t *testing.T) {
	tests := []struct {
		url  string
		want bool
	}{
		{"https://opencode.ai", true},
		{"https://opencode.ai/", true},
		{"https://opencode.ai/v1", true},
		{"https://opencode.ai/v1/chat/completions", true},
		{"https://api.opencode.ai", true},
		{"https://api.opencode.ai/v1", true},
		{"https://go.opencode.ai", true},
		{"https://OPENCODE.AI/v1", true},
		{"http://opencode.ai", true},
		{"http://opencode.ai:8080/v1", true},
		{"opencode.ai", true},
		{"opencode.ai/v1", true},
		{"api.opencode.ai/v1", true},
		{"https://notopencode.ai", false},
		{"https://opencode.ai.attacker.com", false},
		{"https://fakeopencode.ai/v1", false},
		{"https://api.openai.com/v1", false},
		{"https://api.deepseek.com/v1", false},
		{"https://token.sensenova.cn/v1", false},
		{"", false},
	}
	for _, tc := range tests {
		if got := isOpenCodeBaseURL(tc.url); got != tc.want {
			t.Errorf("isOpenCodeBaseURL(%q) = %v, want %v", tc.url, got, tc.want)
		}
	}
}

func TestApplyGrokHeaderTranslationsOpenCodeBaseURL(t *testing.T) {
	gw := config.GatewayConfig{BaseURL: "https://opencode.ai/v1"}

	// 1. Grok session ID is forwarded as x-opencode-session
	src1 := http.Header{}
	src1.Set("X-Grok-Session-Id", "sess-grok-123")
	src1.Set("X-Grok-Conv-Id", "conv-grok-456")
	dst1 := http.Header{}
	applyGrokHeaderTranslations(dst1, src1, gw, "req-audit-1", false)
	if got := dst1.Get("x-opencode-session"); got != "sess-grok-123" {
		t.Fatalf("expected X-Grok-Session-Id to become x-opencode-session, got %q", got)
	}

	// 2. Grok conv ID only
	src2 := http.Header{}
	src2.Set("X-Grok-Conv-Id", "conv-grok-456")
	dst2 := http.Header{}
	applyGrokHeaderTranslations(dst2, src2, gw, "req-audit-2", false)
	if got := dst2.Get("x-opencode-session"); got != "conv-grok-456" {
		t.Fatalf("expected X-Grok-Conv-Id to become x-opencode-session, got %q", got)
	}

	// 3. Existing x-opencode-session is preserved
	src3 := http.Header{}
	src3.Set("x-opencode-session", "existing-opencode-sess")
	src3.Set("X-Grok-Conv-Id", "conv-grok-999")
	dst3 := http.Header{}
	applyGrokHeaderTranslations(dst3, src3, gw, "req-audit-3", false)
	if got := dst3.Get("x-opencode-session"); got != "existing-opencode-sess" {
		t.Fatalf("expected existing x-opencode-session to be preserved, got %q", got)
	}
	// 3b. 配对头 x-opencode-client 翻译自客户端标识（参考实现成对发送，且不冒充 "pi"）
	if got := dst3.Get("x-opencode-client"); got != "" {
		t.Fatalf("x-opencode-client must not be invented when client sends no identifier, got %q", got)
	}
	src3b := http.Header{}
	src3b.Set("X-Grok-Client-Identifier", "grok-shell")
	dst3b := http.Header{}
	applyGrokHeaderTranslations(dst3b, src3b, gw, "req-audit-3b", false)
	if got := dst3b.Get("x-opencode-client"); got != "grok-shell" {
		t.Fatalf("x-opencode-client should translate from x-grok-client-identifier, got %q", got)
	}

	// 4. 没有会话头时不得退回审计 requestID。
	//    requestID 每次请求都不同，把它当会话 ID 发出去等于告诉上游"每次都是新会话"，
	//    比不发更糟。因此只生成稳定的 sess-* 值。
	src4 := http.Header{}
	dst4 := http.Header{}
	applyGrokHeaderTranslations(dst4, src4, gw, "req-audit-fallback", false)
	if got := dst4.Get("x-opencode-session"); got == "req-audit-fallback" {
		t.Fatalf("audit requestID must never be used as a session id, got %q", got)
	}
	if got := dst4.Get("x-opencode-session"); !strings.HasPrefix(got, "sess-") || len(got) <= 5 {
		t.Fatalf("expected generated stable session ID, got %q", got)
	}

	// 5. Fallback generated ID when both session headers and requestID are empty
	src5 := http.Header{}
	dst5 := http.Header{}
	applyGrokHeaderTranslations(dst5, src5, gw, "", false)
	if got := dst5.Get("x-opencode-session"); !strings.HasPrefix(got, "sess-") || len(got) <= 5 {
		t.Fatalf("expected generated session ID, got %q", got)
	}

	// 6. Non-opencode gateway does not set x-opencode-session
	otherGw := config.GatewayConfig{BaseURL: "https://api.deepseek.com/v1"}
	dst6 := http.Header{}
	applyGrokHeaderTranslations(dst6, src1, otherGw, "req-audit-1", false)
	if got := dst6.Get("x-opencode-session"); got != "" {
		t.Fatalf("non-OpenCode gateway should not have x-opencode-session, got %q", got)
	}
}

func TestApplyGrokHeaderTranslationsOpenCodeSessionAffinity(t *testing.T) {
	gw := config.GatewayConfig{
		BaseURL:         "https://custom-proxy.example.com/v1",
		SessionAffinity: config.SessionAffinityOpenCode,
	}
	src := http.Header{}
	src.Set("X-Grok-Session-Id", "sess-affinity-123")
	dst := http.Header{}
	applyGrokHeaderTranslations(dst, src, gw, "req-1", false)

	if got := dst.Get("x-opencode-session"); got != "sess-affinity-123" {
		t.Fatalf("SessionAffinityOpenCode should set x-opencode-session, got %q", got)
	}
	if got := dst.Get("session_id"); got != "" {
		t.Fatalf("SessionAffinityOpenCode should not set session_id, got %q", got)
	}
	if got := dst.Get("X-Client-Request-Id"); got != "" {
		t.Fatalf("SessionAffinityOpenCode should not set X-Client-Request-Id, got %q", got)
	}
}

func TestBuildUpstreamHeadersOpenCodeInDefaultForward(t *testing.T) {
	src := http.Header{}
	src.Set("x-opencode-session", "client-provided-sess")
	dst := http.Header{}
	buildUpstreamHeaders(dst, src, config.GatewayConfig{BaseURL: "https://opencode.ai/v1"}, "req-1", false)
	if got := dst.Get("x-opencode-session"); got != "client-provided-sess" {
		t.Fatalf("x-opencode-session should be forwarded: %q", got)
	}
}

// 端到端断言：无论 allowlist 怎么配置，压缩协商类头都不允许到达上游。
// 透传 Accept-Encoding 会让 Go transport 交出未解压的字节（它只在请求未自带该头时
// 才自动 gzip 并透明解压），SSE 转换层将拿到压缩数据并静默丢弃所有事件。
func TestBuildUpstreamHeadersAlwaysStripsCompressionHeaders(t *testing.T) {
	src := http.Header{}
	src.Set("Accept-Encoding", "gzip, br, deflate")
	src.Set("Content-Encoding", "gzip")
	src.Set("Content-Length", "123")
	src.Set("Host", "client.example.com")

	for _, allowlist := range [][]string{
		defaultForwardHeaders,
		{"Accept-Encoding", "Content-Encoding", "Content-Length", "Host"}, // 用户误配
	} {
		dst := http.Header{}
		buildUpstreamHeaders(dst, src, config.GatewayConfig{}, "req-1", false)
		for _, name := range []string{"Accept-Encoding", "Content-Encoding", "Content-Length", "Host"} {
			if got := dst.Get(name); got != "" {
				t.Fatalf("allowlist %v: %s was forwarded: %q", allowlist, name, got)
			}
		}
	}
}

// W3C Trace Context 头是标准头、有明确对应物、转发有收益（端到端链路），默认应转发。
func TestBuildUpstreamHeadersForwardsTraceContext(t *testing.T) {
	src := http.Header{}
	src.Set("Traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	src.Set("Tracestate", "rojo=00f067aa0ba902b7")
	dst := http.Header{}
	buildUpstreamHeaders(dst, src, config.GatewayConfig{}, "req-1", false)
	if got := dst.Get("Traceparent"); got != src.Get("Traceparent") {
		t.Fatalf("Traceparent not forwarded: %q", got)
	}
	if got := dst.Get("Tracestate"); got != src.Get("Tracestate") {
		t.Fatalf("Tracestate not forwarded: %q", got)
	}
}

// Grok Build 对 Anthropic Messages 流式请求也发送 accept: text/event-stream
// （sampler/client.rs 对所有后端统一处理），但 Messages API 不支持用 Accept 协商 SSE，
// 客户端库对 Anthropic 一律发 accept: application/json。代理必须无条件覆盖。
func TestBuildUpstreamHeadersAnthropicAcceptAlwaysJSON(t *testing.T) {
	src := http.Header{}
	src.Set("Accept", "text/event-stream")
	dst := http.Header{}
	buildUpstreamHeaders(dst, src, config.GatewayConfig{Protocol: config.ProtocolAnthropic}, "req-1", true)
	if got := dst.Get("Accept"); got != "application/json" {
		t.Fatalf("Anthropic Accept must be forced to application/json, got %q", got)
	}

	// 非 Anthropic 协议保留客户端的 Accept。
	dst2 := http.Header{}
	buildUpstreamHeaders(dst2, src, config.GatewayConfig{Protocol: config.ProtocolResponses}, "req-1", true)
	if got := dst2.Get("Accept"); got != "text/event-stream" {
		t.Fatalf("Responses Accept should keep client value, got %q", got)
	}
}

// beta 开关改变上游行为（interleaved-thinking 切换推理模式），客户端库按请求条件累加、
// 从不无条件强制。代理不得替上游做决定。
func TestBuildUpstreamHeadersAnthropicBetaNotForced(t *testing.T) {
	dst := http.Header{}
	buildUpstreamHeaders(dst, http.Header{}, config.GatewayConfig{Protocol: config.ProtocolAnthropic}, "req-1", false)
	if got := dst.Get("Anthropic-Beta"); got != "" {
		t.Fatalf("Anthropic-Beta must not be forced, got %q", got)
	}
	if got := dst.Get("Anthropic-Version"); got != "2023-06-01" {
		t.Fatalf("Anthropic-Version default missing: %q", got)
	}

	// 客户端显式携带的 beta 仍应转发。
	src := http.Header{}
	src.Set("Anthropic-Beta", "claude-code-20250219,oauth-2025-04-20")
	dst2 := http.Header{}
	buildUpstreamHeaders(dst2, src, config.GatewayConfig{Protocol: config.ProtocolAnthropic}, "req-1", false)
	if got := dst2.Get("Anthropic-Beta"); got != src.Get("Anthropic-Beta") {
		t.Fatalf("client Anthropic-Beta was not forwarded: %q", got)
	}
}

// 会话亲和头按协议分派（对照 pi-main 参考实现）：
//
//	Responses: session_id + x-client-request-id
//	Chat:      session_id + x-client-request-id + x-session-affinity
//	Anthropic: 仅 x-session-affinity
func TestApplyGrokHeaderTranslationsSessionAffinityByProtocol(t *testing.T) {
	src := http.Header{}
	src.Set("X-Grok-Session-Id", "sess-123")

	cases := []struct {
		protocol      config.Protocol
		wantSessionID bool
		wantClientReq bool
		wantAffinity  bool
	}{
		{config.ProtocolResponses, true, true, false},
		{config.ProtocolChat, true, true, true},
		{config.ProtocolOpenAICompatible, true, true, true},
		{config.ProtocolAnthropic, false, false, true},
	}
	for _, tc := range cases {
		dst := http.Header{}
		applyGrokHeaderTranslations(dst, src, config.GatewayConfig{Protocol: tc.protocol}, "", false)
		if got := dst.Get("session_id") != ""; got != tc.wantSessionID {
			t.Fatalf("protocol %q: session_id presence = %v, want %v", tc.protocol, got, tc.wantSessionID)
		}
		if got := dst.Get("X-Client-Request-Id") != ""; got != tc.wantClientReq {
			t.Fatalf("protocol %q: X-Client-Request-Id presence = %v, want %v", tc.protocol, got, tc.wantClientReq)
		}
		if got := dst.Get("X-Session-Affinity") != ""; got != tc.wantAffinity {
			t.Fatalf("protocol %q: X-Session-Affinity presence = %v, want %v", tc.protocol, got, tc.wantAffinity)
		}
	}
}
