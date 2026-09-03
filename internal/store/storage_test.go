package store

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"grok-gateway-proxy/internal/config"
)

func TestMetricsUseWeightedCacheHitRate(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "proxy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	started := time.Now().UTC()
	logs := []RequestLog{
		{ID: "one", StartedAt: started, GatewayID: "oc", GatewayName: "OpenCode Zen", Prefix: "/oc", IngressProtocol: config.ProtocolResponses, UpstreamProtocol: config.ProtocolResponses, Success: true, Usage: UsageMetrics{InputTokens: 80, CacheReadTokens: 20, PromptTokens: 100, UsagePresent: true, CacheSupported: true}},
		{ID: "two", StartedAt: started.Add(time.Second), GatewayID: "oc", GatewayName: "OpenCode Zen", Prefix: "/oc", IngressProtocol: config.ProtocolResponses, UpstreamProtocol: config.ProtocolResponses, Success: true, Usage: UsageMetrics{InputTokens: 90, CacheReadTokens: 10, PromptTokens: 100, UsagePresent: true, CacheSupported: true}},
		{ID: "three", StartedAt: started.Add(2 * time.Second), GatewayID: "oc", GatewayName: "OpenCode Zen", Prefix: "/oc", IngressProtocol: config.ProtocolResponses, UpstreamProtocol: config.ProtocolResponses, Success: false, Usage: UsageMetrics{InputTokens: 200, PromptTokens: 200, UsagePresent: true}},
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

func TestMetricsIncludeWeightedCacheHitRateByGateway(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "proxy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	started := time.Now().UTC()
	for _, log := range []RequestLog{
		{ID: "oc-1", StartedAt: started, GatewayID: "oc", Usage: UsageMetrics{CacheReadTokens: 30, PromptTokens: 100, CacheSupported: true, UsagePresent: true}},
		{ID: "oc-2", StartedAt: started.Add(time.Second), GatewayID: "oc", Usage: UsageMetrics{CacheReadTokens: 10, PromptTokens: 100, CacheSupported: true, UsagePresent: true}},
		{ID: "st-1", StartedAt: started.Add(2 * time.Second), GatewayID: "st", Usage: UsageMetrics{CacheReadTokens: 75, PromptTokens: 100, CacheSupported: true, UsagePresent: true}},
	} {
		if err := store.Insert(context.Background(), log); err != nil {
			t.Fatal(err)
		}
	}

	metrics, err := store.Metrics(context.Background(), LogFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(metrics.ByGateway) != 2 {
		t.Fatalf("expected two gateway metric groups, got %+v", metrics.ByGateway)
	}
	if got := metrics.ByGateway["oc"].CacheHitRate; got == nil || math.Abs(*got-20) > 0.0001 {
		t.Fatalf("expected oc hit rate 20%%, got %v", got)
	}
	if got := metrics.ByGateway["st"].CacheHitRate; got == nil || math.Abs(*got-75) > 0.0001 {
		t.Fatalf("expected st hit rate 75%%, got %v", got)
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
		Prefix: "/st", IngressProtocol: config.ProtocolChat, UpstreamProtocol: config.ProtocolChat, Model: "demo",
		RequestPath: "/chat/completions", RequestURL: "/st/chat/completions?trace=1",
		UpstreamURL: "https://example.test/v1/chat/completions", Method: "POST", StatusCode: 200,
		ClientResponseStatusCode: 200, UpstreamResponseStatusCode: 200, Success: true, Stream: true,
		DurationMS: 42, RequestHeaders: `{"Content-Type":["application/json"]}`,
		RequestBody:     []byte(`{"client":true}`),
		UpstreamHeaders: `{"Content-Type":["application/json"]}`,
		UpstreamBody:    []byte(`{"upstream":true}`), UpstreamResponseHeaders: `{"Connection":["keep-alive"]}`,
		UpstreamResponseBody: []byte(`{"raw":true}`),
		ResponseHeaders:      `{"Content-Type":["application/json"]}`,
		ResponseBody:         []byte(`{"client":true}`), Error: "",
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

	if err := store.Insert(context.Background(), RequestLog{
		ID: "post-migration", StartedAt: time.Now().UTC(), GatewayID: "ve", GatewayName: "Vercel AI Gateway",
		Prefix: "/ve", IngressProtocol: config.ProtocolResponses, UpstreamProtocol: config.ProtocolResponses,
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

func TestMigrationsRewriteRowsBeyondFirstBatch(t *testing.T) {
	const rows = 300

	shortTimestamp := "2026-01-01T00:00:00Z"
	fullTimestamp := formatTimestamp(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))

	cases := []struct {
		name string

		timestampFor func(index int) string
	}{
		{name: "every row needs rewriting", timestampFor: func(int) string { return shortTimestamp }},
		{
			name: "only the last row needs rewriting",
			timestampFor: func(index int) string {
				if index == rows-1 {
					return shortTimestamp
				}
				return fullTimestamp
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "proxy.db")
			legacy, err := sql.Open("sqlite", dbPath)
			if err != nil {
				t.Fatal(err)
			}
			legacy.SetMaxOpenConns(1)
			if _, err := legacy.Exec(`PRAGMA journal_mode=WAL;`); err != nil {
				t.Fatal(err)
			}
			if _, err := legacy.Exec(`
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
)`); err != nil {
				t.Fatal(err)
			}
			statement, err := legacy.Prepare(`INSERT INTO request_logs (id, started_at, gateway_id, gateway_name, prefix, ingress_protocol, upstream_protocol, model, request_path, upstream_url, method, request_headers, request_body, upstream_headers, upstream_body, response_headers, response_body) VALUES (?, ?, 'ds', 'DeepSeek', '/ds', 'responses', 'responses', 'm', '/responses', 'https://example.test', 'POST', '{}', X'7b7d', '{}', X'7b7d', '{}', X'7b7d')`)
			if err != nil {
				t.Fatal(err)
			}
			for index := 0; index < rows; index++ {

				if _, err := statement.Exec(fmt.Sprintf("legacy-%03d", index), tc.timestampFor(index)); err != nil {
					t.Fatal(err)
				}
			}
			statement.Close()
			legacy.Close()

			store, err := OpenStore(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()

			var remaining int
			if err := store.db.QueryRow(`SELECT COUNT(*) FROM request_logs WHERE length(started_at) != 30`).Scan(&remaining); err != nil {
				t.Fatal(err)
			}
			if remaining != 0 {
				t.Fatalf("%d rows were still outside the fixed timestamp format after migration", remaining)
			}

			var total int
			if err := store.db.QueryRow(`SELECT COUNT(*) FROM request_logs`).Scan(&total); err != nil {
				t.Fatal(err)
			}
			if total != rows {
				t.Fatalf("row count = %d, want %d", total, rows)
			}
		})
	}
}

func TestStoreCountRespectsFilters(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "proxy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Now().UTC().Truncate(time.Nanosecond)
	logs := []RequestLog{
		{ID: "a", StartedAt: now.Add(-2 * time.Hour), GatewayID: "st", Model: "m1", StatusCode: 200, Success: true},
		{ID: "b", StartedAt: now.Add(-time.Hour), GatewayID: "st", Model: "m1", StatusCode: 200, Success: true},
		{ID: "c", StartedAt: now.Add(-time.Hour), GatewayID: "oc", Model: "m2", StatusCode: 500},
	}
	for _, log := range logs {
		if err := store.Insert(context.Background(), log); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		name   string
		filter LogFilter
		want   int64
	}{
		{name: "all", filter: LogFilter{}, want: 3},
		{name: "gateway", filter: LogFilter{GatewayID: "st"}, want: 2},
		{name: "model", filter: LogFilter{Model: "m2"}, want: 1},
		{name: "model substring", filter: LogFilter{Model: "m"}, want: 3},
		{name: "status success", filter: LogFilter{Status: "success"}, want: 2},
		{name: "status failure", filter: LogFilter{Status: "failure"}, want: 1},
		{name: "time range", filter: LogFilter{From: ptrTime(now.Add(-90 * time.Minute))}, want: 2},

		{name: "status unrecognized", filter: LogFilter{Status: "not-a-status"}, want: 0},
		{name: "status class", filter: LogFilter{Status: "5xx"}, want: 1},
		{name: "status class absent", filter: LogFilter{Status: "3xx"}, want: 0},
		{name: "status code", filter: LogFilter{Status: "200"}, want: 2},
		{name: "status SUCCESS case-insensitive", filter: LogFilter{Status: "SUCCESS"}, want: 2},
	}
	for _, tc := range cases {
		n, err := store.Count(context.Background(), tc.filter)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if n != tc.want {
			t.Errorf("%s: Count = %d, want %d", tc.name, n, tc.want)
		}
	}
}

func TestStoreCountEscapesLikeWildcards(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "proxy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Now().UTC().Truncate(time.Nanosecond)
	for _, id := range []string{"demo_x", "demoX"} {
		if err := store.Insert(context.Background(), RequestLog{ID: id, StartedAt: now, Model: id}); err != nil {
			t.Fatal(err)
		}
	}

	if n, _ := store.Count(context.Background(), LogFilter{Model: "demo_x"}); n != 1 {
		t.Fatalf("underscore not escaped: got %d, want 1", n)
	}
	if n, _ := store.Count(context.Background(), LogFilter{Model: "demo"}); n != 2 {
		t.Fatalf("substring match failed: got %d, want 2", n)
	}
}

func TestStorePruneOlderThan(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "proxy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Now().UTC().Truncate(time.Nanosecond)
	for _, entry := range []struct {
		id        string
		startedAt time.Time
	}{
		{"old", now.Add(-48 * time.Hour)},
		{"recent", now.Add(-time.Hour)},
	} {
		if err := store.Insert(context.Background(), RequestLog{ID: entry.id, StartedAt: entry.startedAt, GatewayID: "st"}); err != nil {
			t.Fatal(err)
		}
	}

	n, err := store.PruneOlderThan(context.Background(), 0)
	if err != nil || n != 0 {
		t.Fatalf("zero retention should be a no-op: n=%d err=%v", n, err)
	}
	if n, _ := store.Count(context.Background(), LogFilter{}); n != 2 {
		t.Fatalf("expected 2 rows after no-op prune, got %d", n)
	}

	n, err = store.PruneOlderThan(context.Background(), 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expected 1 row pruned, got %d", n)
	}
	if _, err := store.Get(context.Background(), "old"); err == nil {
		t.Fatal("old log should have been pruned")
	}
	if _, err := store.Get(context.Background(), "recent"); err != nil {
		t.Fatalf("recent log should have survived: %v", err)
	}
}

func TestStoreDeleteKeepsRowsAtBoundary(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "proxy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	base := time.Now().UTC().Truncate(time.Second)
	if err := store.Insert(context.Background(), RequestLog{ID: "keep", StartedAt: base}); err != nil {
		t.Fatal(err)
	}
	if err := store.Insert(context.Background(), RequestLog{ID: "drop", StartedAt: base.Add(-time.Second)}); err != nil {
		t.Fatal(err)
	}

	n, err := store.Delete(context.Background(), &base)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expected 1 row deleted, got %d", n)
	}
	if _, err := store.Get(context.Background(), "keep"); err != nil {
		t.Fatalf("boundary row should be kept: %v", err)
	}
	if _, err := store.Get(context.Background(), "drop"); err == nil {
		t.Fatal("row older than cutoff should be deleted")
	}
}

func TestStoreCheckpointWALIsNoError(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "proxy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.CheckpointWAL(context.Background()); err != nil {
		t.Fatalf("unexpected checkpoint error: %v", err)
	}
}

func TestStoreSkipsDataMigrationWhenAlreadyMigrated(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "proxy.db")
	store, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	legacy, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = legacy.Exec(`INSERT INTO request_logs (
		id, started_at, gateway_id, gateway_name, prefix, ingress_protocol, upstream_protocol,
		model, request_path, request_url, upstream_url, method, status_code, success, stream,
		duration_ms, request_headers, request_body, upstream_headers, upstream_body,
		upstream_response_headers, upstream_response_body, response_headers, response_body,
		error, input_tokens, cache_read_tokens, cache_write_tokens, prompt_tokens,
		output_tokens, reasoning_tokens, cache_supported, usage_present, cache_source
	) VALUES ('dirty', ?, 'st', 'seed', '/st', 'chat_completions', 'chat_completions',
		'm', '/chat/completions', '', 'https://example.test/v1/chat/completions', 'POST',
		200, 1, 0, 0, '{"Authorization":["Bearer secret"]}', X'7b7d', '{}', X'7b7d', '{}', X'7b7d', '{}', X'7b7d',
		'', 0, 0, 0, 0, 0, 0, 0, 0, '')`, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		legacy.Close()
		t.Fatal(err)
	}
	legacy.Close()

	store, err = OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	var headers string
	if err := store.db.QueryRow(`SELECT request_headers FROM request_logs WHERE id = 'dirty'`).Scan(&headers); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(headers, "Bearer secret") {
		t.Fatalf("已迁移库不应再被扫描脱敏: %s", headers)
	}
	var clientStatus int
	if err := store.db.QueryRow(`SELECT client_response_status_code FROM request_logs WHERE id = 'dirty'`).Scan(&clientStatus); err != nil {
		t.Fatal(err)
	}
	if clientStatus != 0 {
		t.Fatalf("已迁移库不应再被回填: client_response_status_code = %d", clientStatus)
	}
}

func TestOpenStoreScrubsLegacyHeaderCredentials(t *testing.T) {
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
    request_url TEXT NOT NULL DEFAULT '',
    upstream_url TEXT NOT NULL,
    method TEXT NOT NULL,
    status_code INTEGER NOT NULL DEFAULT 0,
    client_response_status_code INTEGER NOT NULL DEFAULT 0,
    upstream_response_status_code INTEGER NOT NULL DEFAULT 0,
    success INTEGER NOT NULL DEFAULT 0,
    stream INTEGER NOT NULL DEFAULT 0,
    duration_ms INTEGER NOT NULL DEFAULT 0,
    request_headers TEXT NOT NULL,
    request_headers_actual TEXT NOT NULL DEFAULT '',
    request_body BLOB NOT NULL,
    upstream_headers TEXT NOT NULL,
    upstream_headers_actual TEXT NOT NULL DEFAULT '',
    upstream_body BLOB NOT NULL,
    upstream_response_headers TEXT NOT NULL DEFAULT '',
    upstream_response_headers_actual TEXT NOT NULL DEFAULT '',
    upstream_response_body BLOB NOT NULL DEFAULT X'',
    response_headers TEXT NOT NULL,
    response_headers_actual TEXT NOT NULL DEFAULT '',
    response_body BLOB NOT NULL,
    response_truncated INTEGER NOT NULL DEFAULT 0,
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
INSERT INTO request_logs (id, started_at, gateway_id, gateway_name, prefix, ingress_protocol, upstream_protocol, model, request_path, upstream_url, method, status_code, success, stream, duration_ms, request_headers, request_headers_actual, request_body, upstream_headers, upstream_headers_actual, upstream_body, upstream_response_headers, upstream_response_headers_actual, upstream_response_body, response_headers, response_headers_actual, response_body, error, input_tokens, cache_read_tokens, cache_write_tokens, prompt_tokens, output_tokens, reasoning_tokens, cache_supported, usage_present, cache_source)
VALUES ('leak-1', '2026-01-01T00:00:00Z', 'oc', 'OpenCode Zen', '/oc', 'responses', 'responses', 'demo', '/responses', 'https://example.test/v1/responses', 'POST', 200, 1, 1, 10, '{"Authorization":["[REDACTED]"]}', '{"Accept":["application/json"],"Authorization":["Bearer sk-leaked-secret"]}', X'7b7d', '{"Authorization":["[REDACTED]"]}', '{"Authorization":["Bearer sk-leaked-secret"],"X-Api-Key":["abc123"]}', X'7b7d', '{}', '{}', X'7b7d', '{}', '{}', X'7b7d', '', 0, 0, 0, 0, 0, 0, 0, 0, '');
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

	var requestHeaders, requestHeadersActual, upstreamHeaders, upstreamHeadersActual string
	row := store.db.QueryRow(`SELECT request_headers, request_headers_actual, upstream_headers, upstream_headers_actual FROM request_logs WHERE id = 'leak-1'`)
	if err := row.Scan(&requestHeaders, &requestHeadersActual, &upstreamHeaders, &upstreamHeadersActual); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"request_headers":         requestHeaders,
		"request_headers_actual":  requestHeadersActual,
		"upstream_headers":        upstreamHeaders,
		"upstream_headers_actual": upstreamHeadersActual,
	} {
		if strings.Contains(value, "sk-leaked-secret") || strings.Contains(value, "abc123") {
			t.Fatalf("%s still contains credentials after scrub: %s", name, value)
		}
		if !strings.Contains(value, "[REDACTED]") {
			t.Fatalf("%s was not redacted: %s", name, value)
		}
	}
	if !strings.Contains(requestHeadersActual, `"Accept":["application/json"]`) {
		t.Fatalf("non-sensitive legacy header was lost: %s", requestHeadersActual)
	}

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var again string
	if err := store.db.QueryRow(`SELECT request_headers_actual FROM request_logs WHERE id = 'leak-1'`).Scan(&again); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(again, "sk-leaked-secret") {
		t.Fatalf("credentials reappeared after reopen: %s", again)
	}
}

func ptrTime(t time.Time) *time.Time { return &t }

func containsAll(value string, parts ...string) bool {
	for _, part := range parts {
		if !strings.Contains(value, part) {
			return false
		}
	}
	return true
}

func TestStoreCompressionRoundTrip(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "proxy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	largeBody := strings.Repeat(`{"role":"user","content":"This is a test payload that compresses well because it repeats. "}`, 100)
	log := RequestLog{
		ID:                      "compress-rt",
		StartedAt:               time.Now().UTC(),
		GatewayID:               "oc",
		RequestHeaders:          `{"Authorization":["Bearer secret"],"Content-Type":["application/json"]}`,
		UpstreamHeaders:         `{"Content-Type":["application/json"]}`,
		UpstreamResponseHeaders: `{"X-Request-Id":["abc-123"]}`,
		ResponseHeaders:         `{"Content-Type":["text/event-stream"]}`,
		RequestBody:             []byte(largeBody),
		UpstreamBody:            []byte(`{"transformed":true}`),
		UpstreamResponseBody:    []byte("data: {\"chunk\":1}\ndata: [DONE]\n"),
		ResponseBody:            []byte(`{"id":"resp-1","status":"completed"}`),
	}
	if err := store.Insert(context.Background(), log); err != nil {
		t.Fatalf("insert failed: %v", err)
	}

	got, err := store.Get(context.Background(), "compress-rt")
	if err != nil {
		t.Fatalf("get failed: %v", err)
	}
	if string(got.RequestBody) != largeBody {
		t.Fatalf("request_body round-trip mismatch: got %d bytes, want %d", len(got.RequestBody), len(largeBody))
	}
	if got.RequestHeaders != log.RequestHeaders {
		t.Fatalf("request_headers mismatch: got %q want %q", got.RequestHeaders, log.RequestHeaders)
	}
	if string(got.ResponseBody) != string(log.ResponseBody) {
		t.Fatalf("response_body round-trip mismatch")
	}
}

func TestStoreCompressedDataIsSmaller(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "proxy.db")
	store, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	payload := strings.Repeat(`{"role":"assistant","content":"Hello world! "}`, 200)
	if err := store.Insert(context.Background(), RequestLog{
		ID:          "size-check",
		StartedAt:   time.Now().UTC(),
		GatewayID:   "oc",
		RequestBody: []byte(payload),
	}); err != nil {
		t.Fatal(err)
	}

	var stored []byte
	if err := store.db.QueryRow(`SELECT request_body FROM request_logs WHERE id = 'size-check'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if len(stored) >= len(payload) {
		t.Fatalf("compressed data (%d bytes) should be smaller than raw (%d bytes)", len(stored), len(payload))
	}
}

func TestStoredBlobIsPlainGzip(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "proxy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	payload := strings.Repeat(`{"role":"user","content":"round and round it goes. "}`, 40)
	if err := store.Insert(context.Background(), RequestLog{
		ID:          "plain-gzip",
		StartedAt:   time.Now().UTC(),
		GatewayID:   "oc",
		RequestBody: []byte(payload),
	}); err != nil {
		t.Fatal(err)
	}

	var stored []byte
	if err := store.db.QueryRow(`SELECT request_body FROM request_logs WHERE id = 'plain-gzip'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored[0] != 0x1f || stored[1] != 0x8b {
		t.Fatalf("stored blob is not gzip: %x %x", stored[0], stored[1])
	}
	if stored[3]&0x04 == 0 {
		t.Fatal("FEXTRA flag not set; the marker is not in the gzip header")
	}
	xlen := int(stored[10]) | int(stored[11])<<8
	if string(stored[12:12+xlen]) != "GZ" {
		t.Fatalf("extra field = %q, want %q", string(stored[12:12+xlen]), "GZ")
	}

	reader, err := gzip.NewReader(bytes.NewReader(stored))
	if err != nil {
		t.Fatalf("stored blob is not readable by gzip.NewReader: %v", err)
	}
	defer reader.Close()
	decoded, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("decompress: %v", err)
	}
	if string(decoded) != payload {
		t.Fatalf("decoded %d bytes, want %d", len(decoded), len(payload))
	}
}

func TestStoreReadsLegacyPrefixedBlob(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "proxy.db")
	store, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	payload := `{"model":"legacy-prefix","messages":[{"role":"user","content":"hello"}]}`
	var blob bytes.Buffer
	blob.WriteString("\x1f\x8bGZ")
	writer := gzip.NewWriter(&blob)
	if _, err := writer.Write([]byte(payload)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	legacy, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = legacy.Exec(`INSERT INTO request_logs (id, started_at, gateway_id, gateway_name, prefix, ingress_protocol, upstream_protocol, model, request_path, upstream_url, method, status_code, success, stream, duration_ms, request_headers, request_body, upstream_headers, upstream_body, response_headers, response_body, error, input_tokens, cache_read_tokens, cache_write_tokens, prompt_tokens, output_tokens, reasoning_tokens, cache_supported, usage_present, cache_source) VALUES (?, ?, 'oc', 'OpenCode Zen', '/oc', 'responses', 'responses', 'legacy', '/responses', 'https://example.test/v1/responses', 'POST', 200, 1, 1, 10, '{}', ?, '{}', X'7b7d', '{}', X'7b7d', '', 0, 0, 0, 0, 0, 0, 0, 0, '')`,
		"legacy-prefix", time.Now().UTC().Format(time.RFC3339Nano), blob.Bytes())
	if err != nil {
		legacy.Close()
		t.Fatal(err)
	}
	legacy.Close()

	store, err = OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	got, err := store.Get(context.Background(), "legacy-prefix")
	if err != nil {
		t.Fatalf("get legacy row: %v", err)
	}
	if string(got.RequestBody) != payload {
		t.Fatalf("legacy body mismatch: got %q want %q", string(got.RequestBody), payload)
	}
}

func TestStoreReadsUncompressedLegacyRows(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "proxy.db")
	store, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	legacy, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	rawBody := `{"model":"legacy","messages":[{"role":"user","content":"hello"}]}`
	_, err = legacy.Exec(`INSERT INTO request_logs (id, started_at, gateway_id, gateway_name, prefix, ingress_protocol, upstream_protocol, model, request_path, upstream_url, method, status_code, success, stream, duration_ms, request_headers, request_body, upstream_headers, upstream_body, response_headers, response_body, error, input_tokens, cache_read_tokens, cache_write_tokens, prompt_tokens, output_tokens, reasoning_tokens, cache_supported, usage_present, cache_source) VALUES (?, ?, 'oc', 'OpenCode Zen', '/oc', 'responses', 'responses', 'legacy', '/responses', 'https://example.test/v1/responses', 'POST', 200, 1, 1, 10, '{}', ?, '{}', X'7b7d', '{}', X'7b7d', '', 0, 0, 0, 0, 0, 0, 0, 0, '')`,
		"legacy-raw", time.Now().UTC().Format(time.RFC3339Nano), rawBody)
	if err != nil {
		legacy.Close()
		t.Fatal(err)
	}
	legacy.Close()

	store, err = OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	got, err := store.Get(context.Background(), "legacy-raw")
	if err != nil {
		t.Fatalf("get legacy row: %v", err)
	}
	if string(got.RequestBody) != rawBody {
		t.Fatalf("legacy body mismatch: got %q want %q", string(got.RequestBody), rawBody)
	}
}

func TestStoreVacuumIsNoError(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "proxy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if err := store.Insert(context.Background(), RequestLog{
		ID:        "vacuum-test",
		StartedAt: time.Now().UTC(),
		GatewayID: "oc",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Vacuum(context.Background()); err != nil {
		t.Fatalf("unexpected VACUUM error: %v", err)
	}

	count, err := store.Count(context.Background(), LogFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expected 1 row after VACUUM, got %d", count)
	}
}

func TestListEmptyResultIsNotNull(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "proxy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	logs, err := store.List(context.Background(), LogFilter{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if logs == nil {
		t.Fatal("List returned a nil slice for an empty result")
	}
	if len(logs) != 0 {
		t.Fatalf("expected no rows, got %d", len(logs))
	}
	encoded, err := json.Marshal(map[string]any{"items": logs})
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"items":[]}` {
		t.Fatalf("empty result marshaled as %s, want {\"items\":[]}", encoded)
	}
}

func TestReclaimSpaceKeepsRemainingRows(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "proxy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Now().UTC()
	for _, log := range []RequestLog{
		{ID: "old", StartedAt: now.Add(-48 * time.Hour), GatewayID: "ds"},
		{ID: "new", StartedAt: now, GatewayID: "ds"},
	} {
		if err := store.Insert(context.Background(), log); err != nil {
			t.Fatal(err)
		}
	}
	deleted, err := store.Delete(context.Background(), &[]time.Time{now.Add(-24 * time.Hour)}[0])
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("deleted = %d, want 1", deleted)
	}
	if err := store.ReclaimSpace(context.Background()); err != nil {
		t.Fatalf("ReclaimSpace: %v", err)
	}
	logs, err := store.List(context.Background(), LogFilter{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 || logs[0].ID != "new" {
		t.Fatalf("unexpected rows after reclaim: %+v", logs)
	}
}

func TestMigrationNormalizesOffsetBearingTimestamp(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "proxy.db")
	store, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Insert(context.Background(), RequestLog{
		ID:        "legacy-offset",
		StartedAt: time.Date(2026, 8, 30, 14, 5, 6, 123456789, time.UTC),
		GatewayID: "ds",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	legacy, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	if _, err := legacy.Exec(`UPDATE request_logs SET started_at = '2026-08-30T22:05:06.123456789+08:00' WHERE id = 'legacy-offset'`); err != nil {
		t.Fatal(err)
	}

	if _, err := legacy.Exec(`UPDATE proxy_meta SET value = '3' WHERE key = 'schema_version'`); err != nil {
		t.Fatal(err)
	}

	rewritten, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer rewritten.Close()

	var stored string
	if err := rewritten.db.QueryRow(`SELECT started_at FROM request_logs WHERE id = 'legacy-offset'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if want := formatTimestamp(time.Date(2026, 8, 30, 14, 5, 6, 123456789, time.UTC)); stored != want {
		t.Fatalf("started_at = %q, want the UTC fixed-width %q", stored, want)
	}
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "proxy.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestRecentByGatewayTakesNewestPerGateway(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	base := time.Now().Add(-time.Hour)
	insert := func(id, gateway string, at time.Time) {
		if err := store.Insert(ctx, RequestLog{ID: id, GatewayID: gateway, StartedAt: at, Success: true}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 5; i++ {
		insert(fmt.Sprintf("loud-%d", i), "loud", base.Add(time.Duration(i)*time.Minute))
	}
	insert("quiet-0", "quiet", base)

	recent, err := store.RecentByGateway(ctx, LogFilter{Limit: 50}, 2)
	if err != nil {
		t.Fatalf("RecentByGateway: %v", err)
	}
	if len(recent) != 2 {
		t.Fatalf("gateways = %d, want 2: %+v", len(recent), recent)
	}
	if len(recent["quiet"]) != 1 {
		t.Fatalf("quiet gateway rows = %d, want 1", len(recent["quiet"]))
	}

	if len(recent["loud"]) != 2 || recent["loud"][0].ID != "loud-4" || recent["loud"][1].ID != "loud-3" {
		t.Fatalf("loud rows are not the newest two, newest first: %+v", recent["loud"])
	}
}

func TestRecentByGatewayHonoursFilters(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	cutoff := time.Now().Add(-time.Hour)
	for _, log := range []RequestLog{
		{ID: "old", GatewayID: "ds", StartedAt: cutoff.Add(-2 * time.Hour), Model: "m1"},
		{ID: "new", GatewayID: "ds", StartedAt: cutoff, Model: "m1", Success: true},
		{ID: "other-model", GatewayID: "ds", StartedAt: time.Now(), Model: "m2"},
	} {
		if err := store.Insert(ctx, log); err != nil {
			t.Fatal(err)
		}
	}
	from := cutoff.Add(-time.Minute)
	recent, err := store.RecentByGateway(ctx, LogFilter{From: &from, Model: "m1"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(recent["ds"]) != 1 || recent["ds"][0].ID != "new" {
		t.Fatalf("filters were ignored: %+v", recent["ds"])
	}
}

func distinguishableRow(id string) RequestLog {
	return RequestLog{
		ID: id, StartedAt: time.Now().UTC().Add(-3 * time.Second),
		GatewayID: "st", GatewayName: "SenseNova", Prefix: "/st",
		IngressProtocol: config.ProtocolChat, UpstreamProtocol: config.ProtocolResponses,
		Model: "grok-4", RequestPath: "/st/chat/completions",
		RequestURL:  "https://client.test/st/chat/completions?trace=1",
		UpstreamURL: "https://upstream.test/v1/chat/completions",
		Method:      "POST", StatusCode: 200, ClientResponseStatusCode: 201,
		UpstreamResponseStatusCode: 202, Success: true, Stream: true, DurationMS: 42,
		Error: "not empty", ResponseTruncated: true,
		RequestHeaders:          `{"request-headers":"1"}`,
		RequestBody:             []byte("request-body"),
		UpstreamHeaders:         `{"upstream-headers":"2"}`,
		UpstreamBody:            []byte("upstream-body"),
		UpstreamResponseHeaders: `{"upstream-response-headers":"3"}`,
		UpstreamResponseBody:    []byte("upstream-response-body"),
		ResponseHeaders:         `{"response-headers":"4"}`,
		ResponseBody:            []byte("response-body"),
		Usage: UsageMetrics{InputTokens: 1, CacheReadTokens: 2, CacheWriteTokens: 3,
			PromptTokens: 4, OutputTokens: 5, ReasoningTokens: 6,
			CacheSupported: true, UsagePresent: true, CacheSource: "usage-source"},
	}
}

func TestRequestLogColumnLayout(t *testing.T) {
	seen := map[string]bool{}
	for _, column := range requestLogColumns {
		if seen[column] {
			t.Fatalf("column %q appears twice, so no prefix of the full read is a valid summary", column)
		}
		seen[column] = true
	}
	if got := requestLogColumns[summaryColumns : summaryColumns+len(detailScalarColumns)]; !slices.Equal(got, detailScalarColumns) {
		t.Fatalf("detail columns are not the block right after the summary prefix: %v", got)
	}
	if got := requestLogColumns[len(requestLogColumns)-len(capturedColumns):]; !slices.Equal(got, capturedColumns) {
		t.Fatalf("gzipped payloads are not the tail of the column order: %v", got)
	}
	var scan requestLogScan
	if got, want := len(scan.targets()), len(requestLogColumns); got != want {
		t.Fatalf("scan targets = %d, want one per column (%d)", got, want)
	}
}

func TestInsertValuesCoverEveryColumn(t *testing.T) {
	values, err := insertValues(distinguishableRow("insert-values"))
	if err != nil {
		t.Fatal(err)
	}
	for _, column := range requestLogColumns {
		if _, ok := values[column]; !ok {
			t.Fatalf("insertValues has no value for %q", column)
		}
	}
	if got, want := len(values), len(requestLogColumns); got != want {
		t.Fatalf("insertValues returns %d columns, requestLogColumns stores %d", got, want)
	}
}

func TestCapturedValuesKeyTheCapturedColumns(t *testing.T) {
	values := capturedValues(distinguishableRow("captured-values"))
	if got, want := len(values), len(capturedColumns); got != want {
		t.Fatalf("capturedValues returns %d columns, capturedColumns has %d", got, want)
	}
	for _, column := range capturedColumns {
		if _, ok := values[column]; !ok {
			t.Fatalf("capturedColumns stores %q but capturedValues has no value for it", column)
		}
	}
	for column := range values {
		if !slices.Contains(capturedColumns, column) {
			t.Fatalf("capturedValues writes %q, which is not a captured column", column)
		}
	}
}

func TestMetricsAggregateColumnsMatchScanTargets(t *testing.T) {
	if got, want := len(metricsAggregates), len((&metricsAggregate{}).scanArgs()); got != want {
		t.Fatalf("metrics SELECT has %d columns, scanArgs has %d destinations", got, want)
	}
}

func TestStoredRowRoundTripsByColumn(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	want := distinguishableRow("round-trip")
	if err := store.Insert(ctx, want); err != nil {
		t.Fatal(err)
	}

	full, err := store.Get(ctx, want.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !full.StartedAt.Equal(want.StartedAt) {
		t.Fatalf("started_at round-tripped as %v, want %v", full.StartedAt, want.StartedAt)
	}
	full.StartedAt = want.StartedAt
	if !reflect.DeepEqual(full, want) {
		t.Fatalf("stored row did not round-trip by column:\n got=%+v\nwant=%+v", full, want)
	}

	summaries, err := store.List(ctx, LogFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 {
		t.Fatalf("summaries = %d, want 1", len(summaries))
	}
	summary := summaries[0]
	if summary.RequestBody != nil || summary.RequestHeaders != "" || summary.ResponseTruncated {
		t.Fatalf("summary read selected detail columns: %+v", summary)
	}
	withoutDetail := full
	withoutDetail.RequestURL = ""
	withoutDetail.ClientResponseStatusCode = 0
	withoutDetail.UpstreamResponseStatusCode = 0
	withoutDetail.ResponseTruncated = false
	withoutDetail.RequestHeaders = ""
	withoutDetail.RequestBody = nil
	withoutDetail.UpstreamHeaders = ""
	withoutDetail.UpstreamBody = nil
	withoutDetail.UpstreamResponseHeaders = ""
	withoutDetail.UpstreamResponseBody = nil
	withoutDetail.ResponseHeaders = ""
	withoutDetail.ResponseBody = nil
	if !reflect.DeepEqual(summary, withoutDetail) {
		t.Fatalf("summary is not a strict prefix of the full read:\n summary=%+v\n full-as-summary=%+v", summary, withoutDetail)
	}
}
