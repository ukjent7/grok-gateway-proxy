package main

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMetricsUseWeightedCacheHitRate(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "proxy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	started := time.Now().UTC()
	logs := []RequestLog{
		{ID: "one", StartedAt: started, GatewayID: "oc", GatewayName: "OpenCode Zen", Prefix: "/oc", IngressProtocol: ProtocolResponses, UpstreamProtocol: ProtocolResponses, Success: true, Usage: UsageMetrics{InputTokens: 80, CacheReadTokens: 20, PromptTokens: 100, UsagePresent: true, CacheSupported: true}},
		{ID: "two", StartedAt: started.Add(time.Second), GatewayID: "oc", GatewayName: "OpenCode Zen", Prefix: "/oc", IngressProtocol: ProtocolResponses, UpstreamProtocol: ProtocolResponses, Success: true, Usage: UsageMetrics{InputTokens: 90, CacheReadTokens: 10, PromptTokens: 100, UsagePresent: true, CacheSupported: true}},
		{ID: "three", StartedAt: started.Add(2 * time.Second), GatewayID: "oc", GatewayName: "OpenCode Zen", Prefix: "/oc", IngressProtocol: ProtocolResponses, UpstreamProtocol: ProtocolResponses, Success: false, Usage: UsageMetrics{InputTokens: 200, PromptTokens: 200, UsagePresent: true}},
	}
	for _, log := range logs {
		if err := store.Insert(context.Background(), log); err != nil {
			t.Fatal(err)
		}
	}

	metrics, err := store.Metrics(context.Background(), LogFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if metrics.Requests != 3 || metrics.Successes != 2 || metrics.PromptTokens != 400 {
		t.Fatalf("unexpected totals: %+v", metrics)
	}
	if metrics.CachePromptTokens != 200 || metrics.CacheReadTokens != 30 || metrics.CacheSupportedCalls != 2 || metrics.UsageCalls != 3 {
		t.Fatalf("unexpected cache totals: %+v", metrics)
	}
	if metrics.CacheHitRate == nil || math.Abs(*metrics.CacheHitRate-15) > 0.0001 {
		t.Fatalf("expected weighted 15%% hit rate, got %v", metrics.CacheHitRate)
	}
	if metrics.CacheCoveragePercent == nil || math.Abs(*metrics.CacheCoveragePercent-200.0/3.0) > 0.0001 {
		t.Fatalf("unexpected coverage: %v", metrics.CacheCoveragePercent)
	}
}

func TestRequestLogJSONContainsRawBodies(t *testing.T) {
	log := RequestLog{ID: "raw", RequestBody: []byte(`{"model":"demo"}`), ResponseBody: []byte("data: [DONE]\n")}
	body, err := json.Marshal(log)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if !containsAll(text, `"request_body":"{\"model\":\"demo\"}"`, `"response_body":"data: [DONE]\n"`) {
		t.Fatalf("raw bodies were not serialized as text: %s", text)
	}
}

func TestStoreInsertPersistsAllAuditFields(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "proxy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	want := RequestLog{
		ID: "audit-all-fields", StartedAt: time.Now().UTC(), GatewayID: "st", GatewayName: "SenseNova",
		Prefix: "/st", IngressProtocol: ProtocolChat, UpstreamProtocol: ProtocolChat, Model: "demo",
		RequestPath: "/chat/completions", RequestURL: "/st/chat/completions?trace=1",
		UpstreamURL: "https://example.test/v1/chat/completions", Method: "POST", StatusCode: 200,
		ClientResponseStatusCode: 200, UpstreamResponseStatusCode: 200, Success: true, Stream: true,
		DurationMS: 42, RequestHeaders: `{"Content-Type":["application/json"]}`,
		RequestHeadersActual: `{"Content-Type":["application/json"]}`, RequestBody: []byte(`{"client":true}`),
		UpstreamHeaders: `{"Content-Type":["application/json"]}`, UpstreamHeadersActual: `{"Content-Type":["application/json"]}`,
		UpstreamBody: []byte(`{"upstream":true}`), UpstreamResponseHeaders: `{"Connection":["keep-alive"]}`,
		UpstreamResponseHeadersActual: `{"Connection":["keep-alive"]}`, UpstreamResponseBody: []byte(`{"raw":true}`),
		ResponseHeaders: `{"Content-Type":["application/json"]}`, ResponseHeadersActual: `{"Content-Type":["application/json"]}`,
		ResponseBody: []byte(`{"client":true}`), Error: "",
		Usage: UsageMetrics{InputTokens: 1, CacheReadTokens: 2, CacheWriteTokens: 3, PromptTokens: 4,
			OutputTokens: 5, ReasoningTokens: 6, CacheSupported: true, UsagePresent: true, CacheSource: "test"},
	}
	if err := store.Insert(context.Background(), want); err != nil {
		t.Fatal(err)
	}

	got, err := store.Get(context.Background(), want.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RequestURL != want.RequestURL || got.ClientResponseStatusCode != want.ClientResponseStatusCode || got.UpstreamResponseStatusCode != want.UpstreamResponseStatusCode {
		t.Fatalf("audit metadata was not persisted: got=%+v", got)
	}
	checks := []struct {
		name      string
		got, want []byte
	}{
		{name: "request_body", got: got.RequestBody, want: want.RequestBody},
		{name: "upstream_body", got: got.UpstreamBody, want: want.UpstreamBody},
		{name: "upstream_response_body", got: got.UpstreamResponseBody, want: want.UpstreamResponseBody},
		{name: "response_body", got: got.ResponseBody, want: want.ResponseBody},
	}
	for _, check := range checks {
		if !bytes.Equal(check.got, check.want) {
			t.Fatalf("%s was not persisted: got=%q want=%q", check.name, check.got, check.want)
		}
	}
}

func containsAll(value string, parts ...string) bool {
	for _, part := range parts {
		if !strings.Contains(value, part) {
			return false
		}
	}
	return true
}
