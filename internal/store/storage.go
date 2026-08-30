package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"grok-gateway-proxy/internal/config"

	_ "modernc.org/sqlite" // pure-Go SQLite driver
)

// dbTimeFormat guarantees fixed-width (30 chars) UTC timestamps with 9-digit
// nanosecond precision, making them lexicographically monotonic for SQLite
// string comparisons (<, <=, >, >=) and ORDER BY.
const dbTimeFormat = "2006-01-02T15:04:05.000000000Z"

func formatTimestamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(dbTimeFormat)
}

func parseTimestamp(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(dbTimeFormat, s); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return time.Time{}
}

// Store is the SQLite-backed audit log.
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

// parallelCompressThreshold is the total captured size at which compressing
// the columns concurrently starts to pay for itself. Below it, eight
// goroutines plus eight gzip writers cost more than the compression itself —
// which is the common case, since most requests carry small header blobs and
// empty upstream/response fields.
const parallelCompressThreshold = 32 << 10

// compressibleColumns are the eight captured columns, in the order the insert
// statement expects them.
func compressibleColumns(log RequestLog) [8]struct {
	name  string
	value []byte
} {
	return [8]struct {
		name  string
		value []byte
	}{
		{"request_headers", []byte(log.RequestHeaders)},
		{"upstream_headers", []byte(log.UpstreamHeaders)},
		{"upstream_response_headers", []byte(log.UpstreamResponseHeaders)},
		{"response_headers", []byte(log.ResponseHeaders)},
		{"request_body", log.RequestBody},
		{"upstream_body", log.UpstreamBody},
		{"upstream_response_body", log.UpstreamResponseBody},
		{"response_body", log.ResponseBody},
	}
}

func (s *Store) Insert(ctx context.Context, log RequestLog) error {
	type gzipResult struct {
		data []byte
		err  error
	}
	columns := compressibleColumns(log)
	results := make([]gzipResult, len(columns))
	compress := func(i int) {
		results[i].data, results[i].err = gzipBytes(columns[i].value)
	}

	var total int
	for i := range columns {
		total += len(columns[i].value)
	}
	if total >= parallelCompressThreshold {
		// Bodies are the dominant cost; compressing them concurrently cuts
		// insert latency roughly 3x once there is real data to work on.
		var wg sync.WaitGroup
		wg.Add(len(columns))
		for i := range columns {
			go func(i int) {
				defer wg.Done()
				compress(i)
			}(i)
		}
		wg.Wait()
	} else {
		for i := range columns {
			compress(i)
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	for i, r := range results {
		if r.err != nil {
			return fmt.Errorf("compress %s: %w", columns[i].name, r.err)
		}
	}
	compressedReqHeaders, compressedUpHeaders, compressedUpRespHeaders, compressedRespHeaders := results[0].data, results[1].data, results[2].data, results[3].data
	compressedReqBody, compressedUpBody, compressedUpRespBody, compressedRespBody := results[4].data, results[5].data, results[6].data, results[7].data
	args := []any{log.ID, formatTimestamp(log.StartedAt), log.GatewayID,
		log.GatewayName, log.Prefix, log.IngressProtocol, log.UpstreamProtocol,
		log.Model, log.RequestPath, log.RequestURL, log.UpstreamURL, log.Method, log.StatusCode,
		log.ClientResponseStatusCode, log.UpstreamResponseStatusCode, boolInt(log.Success),
		boolInt(log.Stream), log.DurationMS, compressedReqHeaders,
		compressedReqBody, compressedUpHeaders,
		compressedUpBody, compressedUpRespHeaders,
		compressedUpRespBody, compressedRespHeaders,
		compressedRespBody, boolInt(log.ResponseTruncated), log.Error, log.Usage.InputTokens, log.Usage.CacheReadTokens,
		log.Usage.CacheWriteTokens, log.Usage.PromptTokens, log.Usage.OutputTokens,
		log.Usage.ReasoningTokens, boolInt(log.Usage.CacheSupported),
		boolInt(log.Usage.UsagePresent), log.Usage.CacheSource}
	if len(args) != len(requestLogInsertColumns) {
		return fmt.Errorf("request log insert has %d values for %d columns", len(args), len(requestLogInsertColumns))
	}
	_, err := s.db.ExecContext(ctx, requestLogInsertSQL, args...)
	return err
}

// List returns summary rows (no bodies or headers) matching filter, newest
// first. Summaries are enough for the log table; Get loads the full row.
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
	// Non-nil so an empty page marshals as [] rather than null: callers
	// iterate the result, and the API hands it straight to JSON. No capacity
	// hint — Limit is caller-supplied and unbounded here.
	result := []RequestLog{}
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

// CheckpointWAL forces a WAL checkpoint and truncates the -wal file to keep
// database files from growing unbounded. Safe to call periodically.
func (s *Store) CheckpointWAL(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
	return err
}

// Vacuum rebuilds the database file, reclaiming space left behind by deleted
// rows. This is the only way to actually shrink the .db file after large
// DELETEs — SQLite's free-list keeps the pages internally but never returns
// them to the filesystem.
//
// SQLite performs the rebuild atomically on its own, but refuses to VACUUM
// from inside a transaction, so this must never be called within one. It also
// needs exclusive use of the database: the pool is capped at a single
// connection, so every other query — including the audit insert on the request
// path — waits for it, and gives up after busy_timeout. Call it off the
// request path and only when there is space worth reclaiming.
func (s *Store) Vacuum(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, "VACUUM")
	return err
}

// ReclaimSpace shrinks the database file after rows have been deleted:
// CheckpointWAL folds the WAL back into the main file, then VACUUM returns the
// freed pages to the filesystem. Both steps are needed — a checkpoint alone
// leaves the pages on SQLite's free-list, and the .db file never shrinks.
//
// It holds the single pooled connection for the whole rebuild, so call it off
// the request path and only when there is space worth reclaiming.
func (s *Store) ReclaimSpace(ctx context.Context) error {
	if err := s.Vacuum(ctx); err != nil {
		return fmt.Errorf("vacuum: %w", err)
	}
	if err := s.CheckpointWAL(ctx); err != nil {
		return fmt.Errorf("checkpoint wal: %w", err)
	}
	return nil
}

// Delete removes logs started before `before`, or every row when it is nil,
// and reports how many rows went.
func (s *Store) Delete(ctx context.Context, before *time.Time) (int64, error) {
	var result sql.Result
	var err error
	if before == nil {
		result, err = s.db.ExecContext(ctx, `DELETE FROM request_logs`)
	} else {
		result, err = s.db.ExecContext(ctx, `DELETE FROM request_logs WHERE started_at < ?`, formatTimestamp(*before))
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
		switch status := strings.ToLower(filter.Status); status {
		case "success":
			conditions = append(conditions, "success = 1")
		case "failure":
			conditions = append(conditions, "success = 0")
		default:
			if strings.HasSuffix(status, "xx") && len(status) == 3 {
				if base, err := strconv.Atoi(strings.TrimSuffix(status, "xx")); err == nil {
					conditions = append(conditions, "status_code >= ? AND status_code < ?")
					args = append(args, base*100, (base+1)*100)
					break
				}
			}
			if code, err := strconv.Atoi(status); err == nil {
				conditions = append(conditions, "status_code = ?")
				args = append(args, code)
				break
			}
			// An unrecognized status is not expressible as a condition. It
			// must narrow the result to nothing rather than fall through
			// unfiltered: the caller believes it is looking at a subset, and
			// silently returning every row corrupts counts and hit rates.
			conditions = append(conditions, "1=0")
		}
	}
	if filter.From != nil {
		conditions = append(conditions, "started_at >= ?")
		args = append(args, formatTimestamp(*filter.From))
	}
	if filter.To != nil {
		conditions = append(conditions, "started_at <= ?")
		args = append(args, formatTimestamp(*filter.To))
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
	log.StartedAt = parseTimestamp(started)
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
	var reqBody, upBody, upRespBody, respBody []byte
	var reqHeaders, upHeaders, upRespHeaders, respHeaders []byte
	if err := row.Scan(&log.ID, &started, &log.GatewayID, &log.GatewayName,
		&log.Prefix, &ingress, &upstream, &log.Model, &log.RequestPath,
		&log.RequestURL, &log.UpstreamURL, &log.Method, &log.StatusCode,
		&log.ClientResponseStatusCode, &log.UpstreamResponseStatusCode, &success, &stream,
		&log.DurationMS, &reqHeaders, &reqBody,
		&upHeaders, &upBody,
		&upRespHeaders,
		&upRespBody, &respHeaders,
		&respBody, &responseTruncated, &log.Error, &log.Usage.InputTokens,
		&log.Usage.CacheReadTokens, &log.Usage.CacheWriteTokens,
		&log.Usage.PromptTokens, &log.Usage.OutputTokens,
		&log.Usage.ReasoningTokens, &cacheSupported, &usagePresent,
		&log.Usage.CacheSource); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return RequestLog{}, fmt.Errorf("log not found: %w", err)
		}
		return RequestLog{}, err
	}
	log.StartedAt = parseTimestamp(started)
	log.IngressProtocol = config.Protocol(ingress)
	log.UpstreamProtocol = config.Protocol(upstream)
	log.Success = success != 0
	log.Stream = stream != 0
	log.Usage.CacheSupported = cacheSupported != 0
	log.Usage.UsagePresent = usagePresent != 0
	log.ResponseTruncated = responseTruncated != 0
	log.RequestHeaders = gunzipString(reqHeaders)
	log.UpstreamHeaders = gunzipString(upHeaders)
	log.UpstreamResponseHeaders = gunzipString(upRespHeaders)
	log.ResponseHeaders = gunzipString(respHeaders)
	log.RequestBody = decompressBody(reqBody)
	log.UpstreamBody = decompressBody(upBody)
	log.UpstreamResponseBody = decompressBody(upRespBody)
	log.ResponseBody = decompressBody(respBody)
	return log, nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
