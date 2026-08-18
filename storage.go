package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

func OpenStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	store := &Store{db: db}
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;`); err != nil {
		db.Close()
		return nil, err
	}
	if err := store.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS request_logs (
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
CREATE INDEX IF NOT EXISTS idx_request_logs_started_at ON request_logs(started_at);
CREATE INDEX IF NOT EXISTS idx_request_logs_gateway_id ON request_logs(gateway_id);
CREATE INDEX IF NOT EXISTS idx_request_logs_model ON request_logs(model);
`)
	if err != nil {
		return err
	}
	for _, column := range []struct {
		name       string
		definition string
	}{
		{name: "request_url", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "client_response_status_code", definition: "INTEGER NOT NULL DEFAULT 0"},
		{name: "upstream_response_status_code", definition: "INTEGER NOT NULL DEFAULT 0"},
		{name: "request_headers_actual", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "upstream_headers_actual", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "upstream_response_headers", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "upstream_response_headers_actual", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "upstream_response_body", definition: "BLOB NOT NULL DEFAULT X''"},
		{name: "response_headers_actual", definition: "TEXT NOT NULL DEFAULT ''"},
	} {
		if err := s.addColumnIfMissing(column.name, column.definition); err != nil {
			return err
		}
	}
	_, err = s.db.Exec(`
UPDATE request_logs
SET client_response_status_code = CASE WHEN client_response_status_code = 0 THEN status_code ELSE client_response_status_code END,
    upstream_response_status_code = CASE WHEN upstream_response_status_code = 0 THEN status_code ELSE upstream_response_status_code END,
    upstream_response_headers = CASE WHEN upstream_response_headers = '' THEN response_headers ELSE upstream_response_headers END,
    upstream_response_body = CASE WHEN length(upstream_response_body) = 0 THEN response_body ELSE upstream_response_body END
WHERE client_response_status_code = 0 OR upstream_response_status_code = 0
`)
	return err
}

func (s *Store) addColumnIfMissing(name, definition string) error {
	rows, err := s.db.Query(`PRAGMA table_info(request_logs)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var existingName, columnType string
		var notNull, pk int
		var defaultValue any
		if err := rows.Scan(&cid, &existingName, &columnType, &notNull, &defaultValue, &pk); err != nil {
			return err
		}
		if existingName == name {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = s.db.Exec(`ALTER TABLE request_logs ADD COLUMN ` + name + ` ` + definition)
	return err
}

func (s *Store) Close() error { return s.db.Close() }

var requestLogInsertColumns = []string{
	"id", "started_at", "gateway_id", "gateway_name", "prefix", "ingress_protocol",
	"upstream_protocol", "model", "request_path", "request_url", "upstream_url", "method",
	"status_code", "client_response_status_code", "upstream_response_status_code",
	"success", "stream", "duration_ms", "request_headers", "request_headers_actual",
	"request_body", "upstream_headers", "upstream_headers_actual", "upstream_body",
	"upstream_response_headers", "upstream_response_headers_actual", "upstream_response_body",
	"response_headers", "response_headers_actual", "response_body", "error",
	"input_tokens", "cache_read_tokens", "cache_write_tokens", "prompt_tokens",
	"output_tokens", "reasoning_tokens", "cache_supported", "usage_present", "cache_source",
}

var requestLogInsertSQL = buildRequestLogInsertSQL()

func buildRequestLogInsertSQL() string {
	placeholders := make([]string, len(requestLogInsertColumns))
	for i := range placeholders {
		placeholders[i] = "?"
	}
	return "INSERT INTO request_logs (" + strings.Join(requestLogInsertColumns, ", ") + ") VALUES (" + strings.Join(placeholders, ", ") + ")"
}

func (s *Store) Insert(ctx context.Context, log RequestLog) error {
	args := []any{log.ID, log.StartedAt.UTC().Format(time.RFC3339Nano), log.GatewayID,
		log.GatewayName, log.Prefix, log.IngressProtocol, log.UpstreamProtocol,
		log.Model, log.RequestPath, log.RequestURL, log.UpstreamURL, log.Method, log.StatusCode,
		log.ClientResponseStatusCode, log.UpstreamResponseStatusCode, boolInt(log.Success),
		boolInt(log.Stream), log.DurationMS, log.RequestHeaders, log.RequestHeadersActual,
		emptyBlob(log.RequestBody), log.UpstreamHeaders, log.UpstreamHeadersActual,
		emptyBlob(log.UpstreamBody), log.UpstreamResponseHeaders, log.UpstreamResponseHeadersActual,
		emptyBlob(log.UpstreamResponseBody), log.ResponseHeaders, log.ResponseHeadersActual,
		emptyBlob(log.ResponseBody), log.Error, log.Usage.InputTokens, log.Usage.CacheReadTokens,
		log.Usage.CacheWriteTokens, log.Usage.PromptTokens, log.Usage.OutputTokens,
		log.Usage.ReasoningTokens, boolInt(log.Usage.CacheSupported),
		boolInt(log.Usage.UsagePresent), log.Usage.CacheSource}
	if len(args) != len(requestLogInsertColumns) {
		return fmt.Errorf("request log insert has %d values for %d columns", len(args), len(requestLogInsertColumns))
	}
	_, err := s.db.ExecContext(ctx, requestLogInsertSQL, args...)
	return err
}

func emptyBlob(value []byte) []byte {
	if value == nil {
		return []byte{}
	}
	return value
}

func (s *Store) List(ctx context.Context, filter LogFilter) ([]RequestLog, error) {
	where, args := buildLogFilter(filter)
	query := `SELECT id, started_at, gateway_id, gateway_name, prefix,
ingress_protocol, upstream_protocol, model, request_path, upstream_url,
method, status_code, success, stream, duration_ms, error,
input_tokens, cache_read_tokens, cache_write_tokens, prompt_tokens,
output_tokens, reasoning_tokens, cache_supported, usage_present, cache_source
FROM request_logs` + where + ` ORDER BY started_at DESC LIMIT ? OFFSET ?`
	args = append(args, filter.Limit, filter.Offset)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []RequestLog
	for rows.Next() {
		log, err := scanSummary(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, log)
	}
	return result, rows.Err()
}

func (s *Store) Get(ctx context.Context, id string) (RequestLog, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT id, started_at, gateway_id, gateway_name, prefix, ingress_protocol,
upstream_protocol, model, request_path, request_url, upstream_url, method, status_code,
client_response_status_code, upstream_response_status_code, success, stream, duration_ms,
request_headers, request_headers_actual, request_body, upstream_headers, upstream_headers_actual,
upstream_body, upstream_response_headers, upstream_response_headers_actual, upstream_response_body,
response_headers, response_headers_actual, response_body, error, input_tokens,
cache_read_tokens, cache_write_tokens, prompt_tokens, output_tokens,
reasoning_tokens, cache_supported, usage_present, cache_source
FROM request_logs WHERE id = ?`, id)
	return scanFull(row)
}

func (s *Store) Metrics(ctx context.Context, filter LogFilter) (Metrics, error) {
	where, args := buildLogFilter(filter)
	row := s.db.QueryRowContext(ctx, `
SELECT COUNT(*),
       COALESCE(SUM(CASE WHEN success = 1 THEN 1 ELSE 0 END), 0),
       COALESCE(SUM(input_tokens), 0),
       COALESCE(SUM(prompt_tokens), 0),
       COALESCE(SUM(CASE WHEN cache_supported = 1 THEN prompt_tokens ELSE 0 END), 0),
       COALESCE(SUM(CASE WHEN cache_supported = 1 THEN cache_read_tokens ELSE 0 END), 0),
       COALESCE(SUM(CASE WHEN cache_supported = 1 THEN cache_write_tokens ELSE 0 END), 0),
       COALESCE(SUM(output_tokens), 0),
       COALESCE(SUM(reasoning_tokens), 0),
       COALESCE(SUM(CASE WHEN cache_supported = 1 THEN 1 ELSE 0 END), 0),
       COALESCE(SUM(CASE WHEN usage_present = 1 THEN 1 ELSE 0 END), 0)
FROM request_logs`+where, args...)
	var result Metrics
	var requests, successes, cacheSupported, usageCalls int64
	if err := row.Scan(&requests, &successes, &result.InputTokens,
		&result.PromptTokens, &result.CachePromptTokens, &result.CacheReadTokens, &result.CacheWriteTokens,
		&result.OutputTokens, &result.ReasoningTokens, &cacheSupported, &usageCalls); err != nil {
		return Metrics{}, err
	}
	result.Requests = requests
	result.Successes = successes
	result.Failures = requests - successes
	result.CacheSupportedCalls = cacheSupported
	result.UsageCalls = usageCalls
	if usageCalls > 0 {
		coverage := float64(cacheSupported) / float64(usageCalls) * 100
		result.CacheCoveragePercent = &coverage
	}
	if cacheSupported > 0 && result.CachePromptTokens > 0 {
		rate := float64(result.CacheReadTokens) / float64(result.CachePromptTokens) * 100
		result.CacheHitRate = &rate
	}
	result.From = filter.From
	result.To = filter.To
	result.GatewayID = filter.GatewayID
	result.Model = filter.Model
	return result, nil
}

func (s *Store) Delete(ctx context.Context, before *time.Time) (int64, error) {
	var result sql.Result
	var err error
	if before == nil {
		result, err = s.db.ExecContext(ctx, `DELETE FROM request_logs`)
	} else {
		result, err = s.db.ExecContext(ctx, `DELETE FROM request_logs WHERE started_at < ?`, before.UTC().Format(time.RFC3339Nano))
	}
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func buildLogFilter(filter LogFilter) (string, []any) {
	conditions := []string{"1=1"}
	var args []any
	if filter.GatewayID != "" {
		conditions = append(conditions, "gateway_id = ?")
		args = append(args, filter.GatewayID)
	}
	if filter.Model != "" {
		conditions = append(conditions, "model = ?")
		args = append(args, filter.Model)
	}
	if filter.Status != "" {
		if strings.HasSuffix(filter.Status, "xx") && len(filter.Status) == 3 {
			prefix := strings.TrimSuffix(filter.Status, "xx")
			if _, err := strconv.Atoi(prefix); err == nil {
				conditions = append(conditions, "status_code >= ? AND status_code < ?")
				base, _ := strconv.Atoi(prefix)
				args = append(args, base*100, (base+1)*100)
			}
		} else if status, err := strconv.Atoi(filter.Status); err == nil {
			conditions = append(conditions, "status_code = ?")
			args = append(args, status)
		}
	}
	if filter.From != nil {
		conditions = append(conditions, "started_at >= ?")
		args = append(args, filter.From.UTC().Format(time.RFC3339Nano))
	}
	if filter.To != nil {
		conditions = append(conditions, "started_at <= ?")
		args = append(args, filter.To.UTC().Format(time.RFC3339Nano))
	}
	return " WHERE " + strings.Join(conditions, " AND "), args
}

type scanner interface{ Scan(...any) error }

func scanSummary(row scanner) (RequestLog, error) {
	var log RequestLog
	var started string
	var success, stream, cacheSupported, usagePresent int
	var ingress, upstream string
	if err := row.Scan(&log.ID, &started, &log.GatewayID, &log.GatewayName,
		&log.Prefix, &ingress, &upstream, &log.Model, &log.RequestPath,
		&log.UpstreamURL, &log.Method, &log.StatusCode, &success, &stream,
		&log.DurationMS, &log.Error, &log.Usage.InputTokens,
		&log.Usage.CacheReadTokens, &log.Usage.CacheWriteTokens,
		&log.Usage.PromptTokens, &log.Usage.OutputTokens,
		&log.Usage.ReasoningTokens, &cacheSupported, &usagePresent,
		&log.Usage.CacheSource); err != nil {
		return RequestLog{}, err
	}
	log.StartedAt, _ = time.Parse(time.RFC3339Nano, started)
	log.IngressProtocol = Protocol(ingress)
	log.UpstreamProtocol = Protocol(upstream)
	log.Success = success != 0
	log.Stream = stream != 0
	log.Usage.CacheSupported = cacheSupported != 0
	log.Usage.UsagePresent = usagePresent != 0
	return log, nil
}

func scanFull(row scanner) (RequestLog, error) {
	var log RequestLog
	var started string
	var success, stream, cacheSupported, usagePresent int
	var ingress, upstream string
	if err := row.Scan(&log.ID, &started, &log.GatewayID, &log.GatewayName,
		&log.Prefix, &ingress, &upstream, &log.Model, &log.RequestPath,
		&log.RequestURL, &log.UpstreamURL, &log.Method, &log.StatusCode,
		&log.ClientResponseStatusCode, &log.UpstreamResponseStatusCode, &success, &stream,
		&log.DurationMS, &log.RequestHeaders, &log.RequestHeadersActual, &log.RequestBody,
		&log.UpstreamHeaders, &log.UpstreamHeadersActual, &log.UpstreamBody,
		&log.UpstreamResponseHeaders, &log.UpstreamResponseHeadersActual,
		&log.UpstreamResponseBody, &log.ResponseHeaders, &log.ResponseHeadersActual,
		&log.ResponseBody, &log.Error, &log.Usage.InputTokens,
		&log.Usage.CacheReadTokens, &log.Usage.CacheWriteTokens,
		&log.Usage.PromptTokens, &log.Usage.OutputTokens,
		&log.Usage.ReasoningTokens, &cacheSupported, &usagePresent,
		&log.Usage.CacheSource); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return RequestLog{}, fmt.Errorf("log not found: %w", err)
		}
		return RequestLog{}, err
	}
	log.StartedAt, _ = time.Parse(time.RFC3339Nano, started)
	log.IngressProtocol = Protocol(ingress)
	log.UpstreamProtocol = Protocol(upstream)
	log.Success = success != 0
	log.Stream = stream != 0
	log.Usage.CacheSupported = cacheSupported != 0
	log.Usage.UsagePresent = usagePresent != 0
	return log, nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
