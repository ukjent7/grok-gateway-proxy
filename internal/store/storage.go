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

	_ "modernc.org/sqlite" // pure-Go SQLite driver
)

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

type Store struct {
	db      *sql.DB
	changes changeBroker
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

var requestLogColumns = []string{
	"id", "started_at", "gateway_id", "gateway_name", "prefix", "ingress_protocol",
	"upstream_protocol", "model", "request_path", "upstream_url", "method",
	"status_code", "success", "stream", "duration_ms", "upstream_timeout_ms", "error",
	"input_tokens", "cache_read_tokens", "cache_write_tokens", "prompt_tokens",
	"output_tokens", "reasoning_tokens", "cache_supported", "usage_present", "cache_source",
	"request_url", "client_response_status_code", "upstream_response_status_code",
	"response_truncated",
	"request_headers", "request_body", "upstream_headers", "upstream_body",
	"upstream_response_headers", "upstream_response_body",
	"response_headers", "response_body",
}

var detailScalarColumns = []string{
	"request_url", "client_response_status_code", "upstream_response_status_code",
	"response_truncated",
}

var capturedColumns = []string{
	"request_headers", "request_body", "upstream_headers", "upstream_body",
	"upstream_response_headers", "upstream_response_body",
	"response_headers", "response_body",
}

var summaryColumns = len(requestLogColumns) - len(detailScalarColumns) - len(capturedColumns)

var (
	requestLogSummaryColumns = strings.Join(requestLogColumns[:summaryColumns], ", ")
	requestLogAllColumns     = strings.Join(requestLogColumns, ", ")
	requestLogInsertSQL      = buildRequestLogInsertSQL()
)

func buildRequestLogInsertSQL() string {
	placeholders := strings.Repeat("?,", len(requestLogColumns))
	return "INSERT INTO request_logs (" + strings.Join(requestLogColumns, ", ") +
		") VALUES (" + strings.TrimSuffix(placeholders, ",") + ")"
}

func insertValues(log RequestLog) (map[string]any, error) {
	values := map[string]any{
		"id":                            log.ID,
		"started_at":                    formatTimestamp(log.StartedAt),
		"gateway_id":                    log.GatewayID,
		"gateway_name":                  log.GatewayName,
		"prefix":                        log.Prefix,
		"ingress_protocol":              string(log.IngressProtocol),
		"upstream_protocol":             string(log.UpstreamProtocol),
		"model":                         log.Model,
		"request_path":                  log.RequestPath,
		"request_url":                   log.RequestURL,
		"upstream_url":                  log.UpstreamURL,
		"method":                        log.Method,
		"status_code":                   log.StatusCode,
		"client_response_status_code":   log.ClientResponseStatusCode,
		"upstream_response_status_code": log.UpstreamResponseStatusCode,
		"success":                       boolInt(log.Success),
		"stream":                        boolInt(log.Stream),
		"duration_ms":                   log.DurationMS,
		"upstream_timeout_ms":           log.UpstreamTimeoutMS,
		"error":                         log.Error,
		"response_truncated":            boolInt(log.ResponseTruncated),
		"input_tokens":                  log.Usage.InputTokens,
		"cache_read_tokens":             log.Usage.CacheReadTokens,
		"cache_write_tokens":            log.Usage.CacheWriteTokens,
		"prompt_tokens":                 log.Usage.PromptTokens,
		"output_tokens":                 log.Usage.OutputTokens,
		"reasoning_tokens":              log.Usage.ReasoningTokens,
		"cache_supported":               boolInt(log.Usage.CacheSupported),
		"usage_present":                 boolInt(log.Usage.UsagePresent),
		"cache_source":                  log.Usage.CacheSource,
	}
	for column, raw := range capturedValues(log) {

		compressed, err := gzipBytes(raw)
		if err != nil {
			return nil, fmt.Errorf("compress %s: %w", column, err)
		}
		values[column] = compressed
	}
	return values, nil
}

func capturedValues(log RequestLog) map[string][]byte {
	return map[string][]byte{
		"request_headers":           []byte(log.RequestHeaders),
		"upstream_headers":          []byte(log.UpstreamHeaders),
		"upstream_response_headers": []byte(log.UpstreamResponseHeaders),
		"response_headers":          []byte(log.ResponseHeaders),
		"request_body":              log.RequestBody,
		"upstream_body":             log.UpstreamBody,
		"upstream_response_body":    log.UpstreamResponseBody,
		"response_body":             log.ResponseBody,
	}
}

func (s *Store) Insert(ctx context.Context, log RequestLog) error {
	values, err := insertValues(log)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	args := make([]any, len(requestLogColumns))
	for i, column := range requestLogColumns {
		value, ok := values[column]
		if !ok {

			return fmt.Errorf("request log insert has no value for column %q", column)
		}
		args[i] = value
	}
	_, err = s.db.ExecContext(ctx, requestLogInsertSQL, args...)
	if err == nil {

		s.NotifyChange()
	}
	return err
}

func (s *Store) List(ctx context.Context, filter LogFilter) ([]RequestLog, error) {
	where, args := buildLogFilter(filter)
	query := `SELECT ` + requestLogSummaryColumns + `
FROM request_logs` + where + ` ORDER BY started_at DESC LIMIT ? OFFSET ?`
	args = append(args, filter.Limit, filter.Offset)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

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

func (s *Store) RecentByGateway(ctx context.Context, filter LogFilter, perGateway int) (map[string][]RequestLog, error) {
	if perGateway <= 0 {
		return map[string][]RequestLog{}, nil
	}
	where, args := buildLogFilter(filter)
	query := `SELECT ` + requestLogSummaryColumns + ` FROM (
SELECT ` + requestLogSummaryColumns + `,
       ROW_NUMBER() OVER (PARTITION BY gateway_id ORDER BY started_at DESC) AS newest_first
FROM request_logs` + where + `) WHERE newest_first <= ? ORDER BY gateway_id, newest_first`
	args = append(args, perGateway)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string][]RequestLog{}
	for rows.Next() {
		log, err := scanSummary(rows)
		if err != nil {
			return nil, err
		}
		result[log.GatewayID] = append(result[log.GatewayID], log)
	}
	return result, rows.Err()
}

func (s *Store) Get(ctx context.Context, id string) (RequestLog, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+requestLogAllColumns+` FROM request_logs WHERE id = ?`, id)
	log, err := scanFull(row)
	if errors.Is(err, sql.ErrNoRows) {
		return RequestLog{}, fmt.Errorf("%w %q", ErrLogNotFound, id)
	}
	return log, err
}

var ErrLogNotFound = errors.New("log not found")

var metricsAggregates = []string{
	"COUNT(*)",
	"COALESCE(SUM(CASE WHEN success = 1 THEN 1 ELSE 0 END), 0)",
	"COALESCE(SUM(input_tokens), 0)",
	"COALESCE(SUM(prompt_tokens), 0)",
	"COALESCE(SUM(CASE WHEN cache_supported = 1 THEN prompt_tokens ELSE 0 END), 0)",
	"COALESCE(SUM(CASE WHEN cache_supported = 1 THEN cache_read_tokens ELSE 0 END), 0)",
	"COALESCE(SUM(CASE WHEN cache_supported = 1 THEN cache_write_tokens ELSE 0 END), 0)",
	"COALESCE(SUM(output_tokens), 0)",
	"COALESCE(SUM(reasoning_tokens), 0)",
	"COALESCE(SUM(CASE WHEN cache_supported = 1 THEN 1 ELSE 0 END), 0)",
	"COALESCE(SUM(CASE WHEN usage_present = 1 THEN 1 ELSE 0 END), 0)",
}

var metricsAggregateColumns = strings.Join(metricsAggregates, ", ")

func (s *Store) Metrics(ctx context.Context, filter LogFilter) (Metrics, error) {
	where, args := buildLogFilter(filter)
	row := s.db.QueryRowContext(ctx,
		`SELECT `+metricsAggregateColumns+` FROM request_logs`+where, args...)
	var aggregate metricsAggregate
	if err := scanMetricsAggregate(row, &aggregate); err != nil {
		return Metrics{}, err
	}
	result := aggregate.metrics()
	result.From = filter.From
	result.To = filter.To
	result.GatewayID = filter.GatewayID
	result.Model = filter.Model

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

	rows, err := s.db.QueryContext(ctx,
		`SELECT gateway_id, `+metricsAggregateColumns+` FROM request_logs`+where+
			` GROUP BY gateway_id ORDER BY gateway_id`, args...)
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

func (s *Store) PruneOlderThan(ctx context.Context, retention time.Duration) (int64, error) {
	if retention <= 0 {
		return 0, nil
	}
	cutoff := time.Now().Add(-retention)
	return s.Delete(ctx, &cutoff)
}

func (s *Store) CheckpointWAL(ctx context.Context) error {
	var busy, log, checkpointed int
	err := s.db.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &log, &checkpointed)
	if err != nil {
		return err
	}
	if busy != 0 {
		return fmt.Errorf("wal checkpoint busy: %d pages still in the wal, %d not folded back", log, checkpointed)
	}
	return nil
}

func (s *Store) Vacuum(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, "VACUUM")
	return err
}

func (s *Store) ReclaimSpace(ctx context.Context) error {
	if err := s.Vacuum(ctx); err != nil {
		return fmt.Errorf("vacuum: %w", err)
	}
	if err := s.CheckpointWAL(ctx); err != nil {
		return fmt.Errorf("checkpoint wal: %w", err)
	}
	return nil
}

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
	if rows, _ := result.RowsAffected(); rows > 0 {
		s.NotifyChange()
	}
	return result.RowsAffected()
}

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

type requestLogScan struct {
	log               RequestLog
	started           string
	ingress, upstream string
	success           int
	stream            int
	cacheSupported    int
	usagePresent      int
	responseTruncated int
	rawCaptured       [][]byte
}

func (s *requestLogScan) targets() []any {
	s.rawCaptured = make([][]byte, len(capturedColumns))
	targets := []any{
		&s.log.ID, &s.started, &s.log.GatewayID, &s.log.GatewayName, &s.log.Prefix,
		&s.ingress, &s.upstream, &s.log.Model, &s.log.RequestPath, &s.log.UpstreamURL,
		&s.log.Method, &s.log.StatusCode, &s.success, &s.stream, &s.log.DurationMS,
		&s.log.UpstreamTimeoutMS,
		&s.log.Error, &s.log.Usage.InputTokens, &s.log.Usage.CacheReadTokens,
		&s.log.Usage.CacheWriteTokens, &s.log.Usage.PromptTokens,
		&s.log.Usage.OutputTokens, &s.log.Usage.ReasoningTokens,
		&s.cacheSupported, &s.usagePresent, &s.log.Usage.CacheSource,
		&s.log.RequestURL, &s.log.ClientResponseStatusCode,
		&s.log.UpstreamResponseStatusCode, &s.responseTruncated,
	}
	for i := range s.rawCaptured {
		targets = append(targets, &s.rawCaptured[i])
	}
	return targets
}

var capturedIndex = func() map[string]int {
	m := make(map[string]int, len(capturedColumns))
	for i, c := range capturedColumns {
		m[c] = i
	}
	return m
}()

func (s *requestLogScan) captured(column string) []byte {
	idx, ok := capturedIndex[column]
	if !ok {
		panic(fmt.Sprintf("unknown captured column %q", column))
	}
	return s.rawCaptured[idx]
}

func (s *requestLogScan) resolve() RequestLog {
	log := s.log
	log.StartedAt = parseTimestamp(s.started)
	log.IngressProtocol = config.Protocol(s.ingress)
	log.UpstreamProtocol = config.Protocol(s.upstream)
	log.Success = s.success != 0
	log.Stream = s.stream != 0
	log.Usage.CacheSupported = s.cacheSupported != 0
	log.Usage.UsagePresent = s.usagePresent != 0
	log.ResponseTruncated = s.responseTruncated != 0
	return log
}

func (s *requestLogScan) applyPayload(log *RequestLog) {
	log.RequestHeaders = gunzipString(s.captured("request_headers"))
	log.UpstreamHeaders = gunzipString(s.captured("upstream_headers"))
	log.UpstreamResponseHeaders = gunzipString(s.captured("upstream_response_headers"))
	log.ResponseHeaders = gunzipString(s.captured("response_headers"))
	log.RequestBody = decompressBody(s.captured("request_body"))
	log.UpstreamBody = decompressBody(s.captured("upstream_body"))
	log.UpstreamResponseBody = decompressBody(s.captured("upstream_response_body"))
	log.ResponseBody = decompressBody(s.captured("response_body"))
}

func scanSummary(row scanner) (RequestLog, error) {
	var s requestLogScan
	if err := row.Scan(s.targets()[:summaryColumns]...); err != nil {
		return RequestLog{}, err
	}
	return s.resolve(), nil
}

func scanFull(row scanner) (RequestLog, error) {
	var s requestLogScan
	if err := row.Scan(s.targets()...); err != nil {
		return RequestLog{}, err
	}
	log := s.resolve()
	s.applyPayload(&log)
	return log, nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
