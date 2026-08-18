package main

import (
	"bytes"
	"context"
	"database/sql"
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

// A database created by an older proxy build (missing the later-added columns)
// must be migrated on open without losing existing rows.
func TestOpenStoreMigratesLegacySchema(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "proxy.db")
	legacy, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = legacy.Exec(`
CREATE TABLE request_logs (
    id TEXT PRIMARY KEY,
    started_at TEXT NOT NULL,
    gateway_id TEXT NOT NULL,
    gateway_name TEXT NOT NULL,
    prefix TEXT NOT NULL,
    ingress_protocol TEXT NOT NULL,
    upstream_protocol TEXT NOT NULL,
    model TEXT NOT NULL,
    request_path TEXT NOT NULL,
    upstream_url TEXT NOT NULL,
    method TEXT NOT NULL,
    status_code INTEGER NOT NULL DEFAULT 0,
    success INTEGER NOT NULL DEFAULT 0,
    stream INTEGER NOT NULL DEFAULT 0,
    duration_ms INTEGER NOT NULL DEFAULT 0,
    request_headers TEXT NOT NULL,
    request_body BLOB NOT NULL,
    upstream_headers TEXT NOT NULL,
    upstream_body BLOB NOT NULL,
    response_headers TEXT NOT NULL,
    response_body BLOB NOT NULL,
    error TEXT NOT NULL DEFAULT '',
    input_tokens INTEGER NOT NULL DEFAULT 0,
    cache_read_tokens INTEGER NOT NULL DEFAULT 0,
    cache_write_tokens INTEGER NOT NULL DEFAULT 0,
    prompt_tokens INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    reasoning_tokens INTEGER NOT NULL DEFAULT 0,
    cache_supported INTEGER NOT NULL DEFAULT 0,
    usage_present INTEGER NOT NULL DEFAULT 0,
    cache_source TEXT NOT NULL DEFAULT ''
);
INSERT INTO request_logs (id, started_at, gateway_id, gateway_name, prefix, ingress_protocol, upstream_protocol, model, request_path, upstream_url, method, status_code, success, stream, duration_ms, request_headers, request_body, upstream_headers, upstream_body, response_headers, response_body, error, input_tokens, cache_read_tokens, cache_write_tokens, prompt_tokens, output_tokens, reasoning_tokens, cache_supported, usage_present, cache_source)
VALUES ('legacy-1', '2026-01-01T00:00:00Z', 've', 'Vercel AI Gateway', '/ve', 'responses', 'responses', 'demo', '/responses', 'https://example.test/v1/responses', 'POST', 200, 1, 1, 10, '{}', X'7b7d', '{}', X'7b7d', '{}', X'7b7d', '', 0, 0, 0, 0, 0, 0, 0, 0, '');
`)
	if err != nil {
		t.Fatal(err)
	}
	legacy.Close()

	store, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	got, err := store.Get(context.Background(), "legacy-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.GatewayID != "ve" || got.StatusCode != 200 {
		t.Fatalf("legacy row was not preserved: %+v", got)
	}
	if got.ResponseTruncated {
		t.Fatal("legacy row must default to response_truncated=false")
	}

	// The new column must be usable for inserts after migration.
	if err := store.Insert(context.Background(), RequestLog{
		ID: "post-migration", StartedAt: time.Now().UTC(), GatewayID: "ve", GatewayName: "Vercel AI Gateway",
		Prefix: "/ve", IngressProtocol: ProtocolResponses, UpstreamProtocol: ProtocolResponses,
		ResponseTruncated: true,
	}); err != nil {
		t.Fatal(err)
	}
	inserted, err := store.Get(context.Background(), "post-migration")
	if err != nil {
		t.Fatal(err)
	}
	if !inserted.ResponseTruncated {
		t.Fatal("response_truncated flag was not persisted after migration")
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
