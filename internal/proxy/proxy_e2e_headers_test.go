package proxy

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"grok-gateway-proxy/internal/config"
	"grok-gateway-proxy/internal/store"
)

// 端到端行为模拟：完整走一遍 ServeHTTP，验证模拟 Grok Build 客户端的请求
// 到达上游时的头与正文字节。覆盖 header 清洗/翻译与 prompt_cache_key 保序注入。
func TestProxyEndToEndHeaderAndBodySimulation(t *testing.T) {
	type captured struct {
		header http.Header
		body   []byte
	}

	cases := []struct {
		name      string
		gatewayID string
		// 上游应看到（或不应看到）的头
		wantHeader   func(t *testing.T, h http.Header)
		wantBodyFunc func(t *testing.T, body []byte)
	}{
		{
			name:      "std responses",
			gatewayID: "std",
			wantHeader: func(t *testing.T, h http.Header) {
				// 客户端的 "gzip, br, deflate" 必须被剥离。上游看到的是 Go transport
				// 自动注入的 "gzip"（只协商它能透明解压的编码），客户端的 br/deflate
				// 一旦透传，响应体将是 transport 无法解码的压缩字节。
				if got := h.Get("Accept-Encoding"); got != "gzip" {
					t.Fatalf("Accept-Encoding = %q, want transport-injected %q", got, "gzip")
				}
				if got := h.Get("Content-Encoding"); got != "" {
					t.Fatalf("Content-Encoding leaked to upstream: %q", got)
				}
				// W3C 标准头应转发
				if got := h.Get("Traceparent"); got != "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01" {
					t.Fatalf("Traceparent not forwarded: %q", got)
				}
				// Responses 协议的会话亲和头
				if got := h.Get("session_id"); got != "sess-grok-1" {
					t.Fatalf("session_id = %q", got)
				}
				if got := h.Get("X-Client-Request-Id"); got != "sess-grok-1" {
					t.Fatalf("X-Client-Request-Id = %q", got)
				}
				if got := h.Get("X-Session-Affinity"); got != "" {
					t.Fatalf("Responses gateway must not send X-Session-Affinity, got %q", got)
				}
				// 内部头不得外泄
				for _, name := range []string{"X-Grok-Session-Id", "X-Grok-Conv-Id", "X-Grok-Req-Id", "X-Grok-Model-Override"} {
					if got := h.Get(name); got != "" {
						t.Fatalf("internal %s leaked: %q", name, got)
					}
				}
				// 审计 ID 翻译
				if h.Get("X-Request-Id") == "" {
					t.Fatal("X-Request-Id missing")
				}
				if got := h.Get("X-Correlation-Id"); got != "req-grok-9" {
					t.Fatalf("X-Correlation-Id = %q, want grok req id", got)
				}
			},
			wantBodyFunc: func(t *testing.T, body []byte) {
				// 键序逐字节保留（清洗只删 tools 里的 x_search 条目），prompt_cache_key 只在末尾追加
				want := `{"model":"m","stream":true,"input":[{"type":"message","role":"user"}],"tools":[{"type":"function","name":"f"}],"prompt_cache_key":"sess-grok-1"}`
				if string(body) != want {
					t.Fatalf("body not order-preserving:\n got: %s\nwant: %s", body, want)
				}
				// 非标准工具已被清洗
				if strings.Contains(string(body), "x_search") {
					t.Fatalf("x_search leaked: %s", body)
				}
			},
		},
		{
			name:      "anth messages",
			gatewayID: "anth",
			wantHeader: func(t *testing.T, h http.Header) {
				// Anthropic 流式请求也带 text/event-stream，必须被覆盖
				if got := h.Get("Accept"); got != "application/json" {
					t.Fatalf("Anthropic Accept = %q, want application/json", got)
				}
				if got := h.Get("Anthropic-Version"); got != "2023-06-01" {
					t.Fatalf("Anthropic-Version = %q", got)
				}
				if got := h.Get("Anthropic-Beta"); got != "" {
					t.Fatalf("Anthropic-Beta must not be forced, got %q", got)
				}
				// Anthropic 只认 x-session-affinity
				if got := h.Get("X-Session-Affinity"); got != "sess-grok-1" {
					t.Fatalf("X-Session-Affinity = %q", got)
				}
				if got := h.Get("session_id"); got != "" {
					t.Fatalf("Anthropic gateway must not send session_id, got %q", got)
				}
				if got := h.Get("X-Api-Key"); got != "sk-live-1" {
					t.Fatalf("Bearer must be translated to X-Api-Key, got %q", got)
				}
			},
		},
		{
			name:      "oaic chat",
			gatewayID: "oaic",
			wantHeader: func(t *testing.T, h http.Header) {
				// Chat Completions：三件套
				if got := h.Get("session_id"); got != "sess-grok-1" {
					t.Fatalf("session_id = %q", got)
				}
				if got := h.Get("X-Client-Request-Id"); got != "sess-grok-1" {
					t.Fatalf("X-Client-Request-Id = %q", got)
				}
				if got := h.Get("X-Session-Affinity"); got != "sess-grok-1" {
					t.Fatalf("X-Session-Affinity = %q", got)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var upstreamCapture captured
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				upstreamCapture = captured{header: r.Header.Clone(), body: body}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("data: {\"type\":\"response.completed\"}\n\ndata: [DONE]\n\n"))
			}))
			defer upstream.Close()

			st, err := store.OpenStore(t.TempDir() + "/proxy.db")
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()

			cfg := config.DefaultConfig(t.TempDir() + "/config.json")
			gateway := cfg.Gateways[tc.gatewayID]
			gateway.BaseURL = upstream.URL
			cfg.Gateways[tc.gatewayID] = gateway

			p := &Proxy{Config: cfg, Store: st, Logger: slog.Default(), Client: upstream.Client()}

			// 模拟 Grok Build 实际发送的头（sampler/client.rs）与 Responses 正文。
			// 三种后端的头集合相同，差别只在路径与正文格式。
			clientBody := `{"model":"m","stream":true,"input":[{"type":"message","role":"user"}],"tools":[{"type":"x_search"},{"type":"function","name":"f"}]}`
			req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/"+tc.gatewayID+"/responses", strings.NewReader(clientBody))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept-Encoding", "gzip, br, deflate")
			req.Header.Set("Content-Encoding", "gzip")
			req.Header.Set("Authorization", "Bearer sk-live-1")
			req.Header.Set("User-Agent", "grok-shell/1.0")
			req.Header.Set("Traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
			req.Header.Set("X-Grok-Session-Id", "sess-grok-1")
			req.Header.Set("X-Grok-Conv-Id", "conv-grok-1")
			req.Header.Set("X-Grok-Req-Id", "req-grok-9")
			req.Header.Set("X-Grok-Model-Override", "m")
			req.Header.Set("X-Grok-Turn-Idx", "3")

			// /anth 与 /oaic 只收各自的路径
			switch tc.gatewayID {
			case "anth":
				req.URL.Path = "/anth/v1/messages"
			case "oaic":
				req.URL.Path = "/oaic/chat/completions"
			}

			recorder := httptest.NewRecorder()
			p.ServeHTTP(recorder, req)

			if recorder.Code != http.StatusOK {
				t.Fatalf("proxy returned %d: %s", recorder.Code, recorder.Body.String())
			}
			tc.wantHeader(t, upstreamCapture.header)
			if tc.wantBodyFunc != nil {
				tc.wantBodyFunc(t, upstreamCapture.body)
			}
		})
	}
}
