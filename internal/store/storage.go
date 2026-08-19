package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"grok-gateway-proxy/internal/config"

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
    request_body BLOB NOT NULL,
    upstream_headers TEXT NOT NULL,
    upstream_body BLOB NOT NULL,
    upstream_response_headers TEXT NOT NULL DEFAULT '',
    upstream_response_body BLOB NOT NULL DEFAULT X'',
    response_headers TEXT NOT NULL,
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
		{name: "upstream_response_headers", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "upstream_response_body", definition: "BLOB NOT NULL DEFAULT X''"},
		{name: "response_truncated", definition: "INTEGER NOT NULL DEFAULT 0"},
	} {
		if err := s.addColumnIfMissing(column.name, column.definition); err != nil {
			return err
		}
	}
	// One-time data migrations, guarded by a version marker so that the
	// full-table scans only run when upgrading from an older build, not on
	// every startup.
	return s.migrateDataIfNeeded()
}

const currentSchemaVersion = 2

// migrateDataIfNeeded runs one-time data migrations for existing databases:
// backfilling columns added after the initial schema, and scrubbing
// credentials that may have been stored before write-time header
// sanitization existed. The schema_version marker in proxy_meta makes each
// migration run exactly once per database.
func (s *Store) migrateDataIfNeeded() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS proxy_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		return err
	}
	var version int
	err := s.db.QueryRow(`SELECT value FROM proxy_meta WHERE key = 'schema_version'`).Scan(&version)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if version >= currentSchemaVersion {
		return nil
	}

	// Backfill rows written by older builds that predate the extra columns.
	if _, err := s.db.Exec(`
UPDATE request_logs
SET client_response_status_code = CASE WHEN client_response_status_code = 0 THEN status_code ELSE client_response_status_code END,
    upstream_response_status_code = CASE WHEN upstream_response_status_code = 0 THEN status_code ELSE upstream_response_status_code END,
    upstream_response_headers = CASE WHEN upstream_response_headers = '' THEN response_headers ELSE upstream_response_headers END,
    upstream_response_body = CASE WHEN length(upstream_response_body) = 0 THEN response_body ELSE upstream_response_body END
WHERE client_response_status_code = 0 OR upstream_response_status_code = 0
`); err != nil {
		return err
	}
	// Scrub credentials stored before write-time sanitization existed.
	if err := s.scrubStoredHeaderCredentials(); err != nil {
		return err
	}
	// Reconcile FX usage written by older builds. Responses conversion adds
	// cached_tokens: 0 for strict clients, which must not be mistaken for an
	// upstream cache field in historical metrics.
	if err := s.reconcileFXUsageLogs(); err != nil {
		return err
	}
	_, err = s.db.Exec(
		`INSERT INTO proxy_meta (key, value) VALUES ('schema_version', ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		strconv.Itoa(currentSchemaVersion),
	)
	return err
}

func (s *Store) reconcileFXUsageLogs() error {
	rows, err := s.db.Query(`
SELECT id, upstream_response_body
FROM request_logs
WHERE gateway_id = 've'
  AND upstream_url LIKE '%/v3/ai/language-model%'
  AND length(upstream_response_body) > 0`)
	if err != nil {
		return err
	}
	defer rows.Close()

	type usageUpdate struct {
		id    string
		usage UsageMetrics
	}
	var updates []usageUpdate
	for rows.Next() {
		var id string
		var body []byte
		if err := rows.Scan(&id, &body); err != nil {
			return err
		}
		usage := extractFXUsage(body)
		if usage.UsagePresent {
			updates = append(updates, usageUpdate{id: id, usage: usage})
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, update := range updates {
		_, err := s.db.Exec(`
UPDATE request_logs
SET input_tokens = ?, cache_read_tokens = ?, cache_write_tokens = ?,
    prompt_tokens = ?, output_tokens = ?, reasoning_tokens = ?,
    cache_supported = ?, usage_present = ?, cache_source = ?
WHERE id = ?`,
			update.usage.InputTokens, update.usage.CacheReadTokens,
			update.usage.CacheWriteTokens, update.usage.PromptTokens,
			update.usage.OutputTokens, update.usage.ReasoningTokens,
			boolInt(update.usage.CacheSupported), boolInt(update.usage.UsagePresent),
			update.usage.CacheSource, update.id)
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) addColumnIfMissing(name, definition string) error {
	exists, err := s.columnExists(name)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	_, err = s.db.Exec(`ALTER TABLE request_logs ADD COLUMN ` + name + ` ` + definition)
	return err
}

func (s *Store) columnExists(name string) (bool, error) {
	rows, err := s.db.Query(`PRAGMA table_info(request_logs)`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var existingName, columnType string
		var notNull, pk int
		var defaultValue any
		if err := rows.Scan(&cid, &existingName, &columnType, &notNull, &defaultValue, &pk); err != nil {
			return false, err
		}
		if existingName == name {
			return true, nil
		}
	}
	return false, rows.Err()
}

// storedHeaderColumns lists every stored header field that may contain
// credentials. Current columns are sanitized on write; the legacy *_actual
// columns predate that, so both are scrubbed during migration.
var storedHeaderColumns = []string{
	"request_headers", "upstream_headers", "upstream_response_headers", "response_headers",
	"request_headers_actual", "upstream_headers_actual", "upstream_response_headers_actual", "response_headers_actual",
}

// scrubStoredHeaderCredentials replaces sensitive header values (Authorization,
// API keys, tokens, ...) in every stored header field with "[REDACTED]". Rows
// whose values are unchanged are left untouched; the legacy *_actual columns
// may not exist on fresh databases and are skipped.
func (s *Store) scrubStoredHeaderCredentials() error {
	for _, column := range storedHeaderColumns {
		exists, err := s.columnExists(column)
		if err != nil {
			return err
		}
		if !exists {
			continue
		}
		rows, err := s.db.Query(`SELECT id, ` + column + ` FROM request_logs WHERE ` + column + ` != ''`)
		if err != nil {
			return err
		}
		type pendingUpdate struct {
			id    string
			value string
		}
		var updates []pendingUpdate
		for rows.Next() {
			var id, raw string
			if err := rows.Scan(&id, &raw); err != nil {
				rows.Close()
				return err
			}
			if redacted := redactStoredHeaders(raw); redacted != raw {
				updates = append(updates, pendingUpdate{id: id, value: redacted})
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, update := range updates {
			if _, err := s.db.Exec(`UPDATE request_logs SET `+column+` = ? WHERE id = ?`, update.value, update.id); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) Close() error { return s.db.Close() }

var requestLogInsertColumns = []string{
	"id", "started_at", "gateway_id", "gateway_name", "prefix", "ingress_protocol",
	"upstream_protocol", "model", "request_path", "request_url", "upstream_url", "method",
	"status_code", "client_response_status_code", "upstream_response_status_code",
	"success", "stream", "duration_ms", "request_headers",
	"request_body", "upstream_headers", "upstream_body",
	"upstream_response_headers", "upstream_response_body",
	"response_headers", "response_body", "response_truncated", "error",
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
		boolInt(log.Stream), log.DurationMS, log.RequestHeaders,
		emptyBlob(log.RequestBody), log.UpstreamHeaders,
		emptyBlob(log.UpstreamBody), log.UpstreamResponseHeaders,
		emptyBlob(log.UpstreamResponseBody), log.ResponseHeaders,
		emptyBlob(log.ResponseBody), boolInt(log.ResponseTruncated), log.Error, log.Usage.InputTokens, log.Usage.CacheReadTokens,
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
request_headers, request_body, upstream_headers,
upstream_body, upstream_response_headers, upstream_response_body,
response_headers, response_body, response_truncated, error, input_tokens,
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
	var aggregate metricsAggregate
	if err := scanMetricsAggregate(row, &aggregate); err != nil {
		return Metrics{}, err
	}
	result := aggregate.metrics()
	result.From = filter.From
	result.To = filter.To
	result.GatewayID = filter.GatewayID
	result.Model = filter.Model

	// The unfiltered overview gets one weighted aggregate per gateway in the
	// same time/model window. Filtered metrics remain a single aggregate.
	if filter.GatewayID == "" {
		byGateway, err := s.metricsByGateway(ctx, filter)
		if err != nil {
			return Metrics{}, err
		}
		result.ByGateway = byGateway
	}
	return result, nil
}

type metricsAggregate struct {
	requests, successes                                  int64
	inputTokens, promptTokens                            int64
	cachePromptTokens, cacheReadTokens, cacheWriteTokens int64
	outputTokens, reasoningTokens                        int64
	cacheSupported, usageCalls                           int64
}

func (a *metricsAggregate) scanArgs() []any {
	return []any{
		&a.requests, &a.successes, &a.inputTokens, &a.promptTokens,
		&a.cachePromptTokens, &a.cacheReadTokens, &a.cacheWriteTokens,
		&a.outputTokens, &a.reasoningTokens, &a.cacheSupported, &a.usageCalls,
	}
}

func scanMetricsAggregate(row scanner, aggregate *metricsAggregate) error {
	return row.Scan(aggregate.scanArgs()...)
}

func (a metricsAggregate) metrics() Metrics {
	result := Metrics{
		Requests:            a.requests,
		Successes:           a.successes,
		Failures:            a.requests - a.successes,
		InputTokens:         a.inputTokens,
		PromptTokens:        a.promptTokens,
		CachePromptTokens:   a.cachePromptTokens,
		CacheReadTokens:     a.cacheReadTokens,
		CacheWriteTokens:    a.cacheWriteTokens,
		OutputTokens:        a.outputTokens,
		ReasoningTokens:     a.reasoningTokens,
		CacheSupportedCalls: a.cacheSupported,
		UsageCalls:          a.usageCalls,
	}
	if a.usageCalls > 0 {
		coverage := float64(a.cacheSupported) / float64(a.usageCalls) * 100
		result.CacheCoveragePercent = &coverage
	}
	if a.cachePromptTokens > 0 {
		rate := float64(a.cacheReadTokens) / float64(a.cachePromptTokens) * 100
		result.CacheHitRate = &rate
	}
	return result
}

func (s *Store) metricsByGateway(ctx context.Context, filter LogFilter) (map[string]Metrics, error) {
	where, args := buildLogFilter(filter)
	rows, err := s.db.QueryContext(ctx, `
SELECT gateway_id,
       COUNT(*),
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
FROM request_logs`+where+` GROUP BY gateway_id ORDER BY gateway_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[string]Metrics)
	for rows.Next() {
		var gatewayID string
		var aggregate metricsAggregate
		rowArgs := append([]any{&gatewayID}, aggregate.scanArgs()...)
		if err := rows.Scan(rowArgs...); err != nil {
			return nil, err
		}
		metrics := aggregate.metrics()
		metrics.From = filter.From
		metrics.To = filter.To
		metrics.GatewayID = gatewayID
		metrics.Model = filter.Model
		result[gatewayID] = metrics
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// PruneOlderThan deletes logs older than the given retention window.
// A retention of zero or less disables pruning.
func (s *Store) PruneOlderThan(ctx context.Context, retention time.Duration) (int64, error) {
	if retention <= 0 {
		return 0, nil
	}
	cutoff := time.Now().Add(-retention)
	return s.Delete(ctx, &cutoff)
}

// CheckpointWAL forces a WAL checkpoint to keep the -wal file from growing
// unbounded. Safe to call periodically.
func (s *Store) CheckpointWAL(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, "PRAGMA wal_checkpoint(PASSIVE)")
	return err
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

// Count returns the total number of stored logs matching the filter.
func (s *Store) Count(ctx context.Context, filter LogFilter) (int64, error) {
	where, args := buildLogFilter(filter)
	row := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM request_logs`+where, args...)
	var n int64
	err := row.Scan(&n)
	if err != nil {
		return 0, err
	}
	return n, nil
}

func buildLogFilter(filter LogFilter) (string, []any) {
	conditions := []string{"1=1"}
	var args []any
	if filter.GatewayID != "" {
		conditions = append(conditions, "gateway_id = ?")
		args = append(args, filter.GatewayID)
	}
	if filter.Model != "" {
		// 前端把模型筛选当作"搜索"用，所以用 LIKE 子串匹配并对通配符转义，
		// 否则输入 grok-4 匹配不到 grok-4.6。
		escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(filter.Model)
		conditions = append(conditions, "model LIKE ? ESCAPE '\\'")
		args = append(args, "%"+escaped+"%")
	}
	if filter.Status != "" {
		switch strings.ToLower(filter.Status) {
		case "success":
			conditions = append(conditions, "success = 1")
		case "failure":
			conditions = append(conditions, "success = 0")
		default:
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
	log.IngressProtocol = config.Protocol(ingress)
	log.UpstreamProtocol = config.Protocol(upstream)
	log.Success = success != 0
	log.Stream = stream != 0
	log.Usage.CacheSupported = cacheSupported != 0
	log.Usage.UsagePresent = usagePresent != 0
	return log, nil
}

func scanFull(row scanner) (RequestLog, error) {
	var log RequestLog
	var started string
	var success, stream, cacheSupported, usagePresent, responseTruncated int
	var ingress, upstream string
	if err := row.Scan(&log.ID, &started, &log.GatewayID, &log.GatewayName,
		&log.Prefix, &ingress, &upstream, &log.Model, &log.RequestPath,
		&log.RequestURL, &log.UpstreamURL, &log.Method, &log.StatusCode,
		&log.ClientResponseStatusCode, &log.UpstreamResponseStatusCode, &success, &stream,
		&log.DurationMS, &log.RequestHeaders, &log.RequestBody,
		&log.UpstreamHeaders, &log.UpstreamBody,
		&log.UpstreamResponseHeaders,
		&log.UpstreamResponseBody, &log.ResponseHeaders,
		&log.ResponseBody, &responseTruncated, &log.Error, &log.Usage.InputTokens,
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
	log.IngressProtocol = config.Protocol(ingress)
	log.UpstreamProtocol = config.Protocol(upstream)
	log.Success = success != 0
	log.Stream = stream != 0
	log.Usage.CacheSupported = cacheSupported != 0
	log.Usage.UsagePresent = usagePresent != 0
	log.ResponseTruncated = responseTruncated != 0
	return log, nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
