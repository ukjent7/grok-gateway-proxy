package store

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

// Model filter is a substring match with LIKE wildcards escaped, so a literal
// underscore in the query matches itself and not any single character.
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
	// A literal underscore must not act as a single-character wildcard.
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

	// Retention of zero or less disables pruning.
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

// Rows exactly at the cutoff are kept: Delete uses a strict less-than.
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

// 已带 schema_version 标记的库再次打开时，不应重复执行全表回填/脱敏扫描：
// 手工写入的"脏"数据（缺失的派生状态码、未脱敏凭据）必须保持原样。
func TestStoreSkipsDataMigrationWhenAlreadyMigrated(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "proxy.db")
	store, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// 手工写入一行模拟"旧构建遗留"的脏数据。
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

	// 重新打开：已有版本标记，数据迁移必须被跳过。
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

// Databases written by an older proxy stored raw headers in the *_actual
// columns; opening them must scrub credentials from every stored header field.
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

	// Reopening must be idempotent: no error and no data corruption.
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
