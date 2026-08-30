package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"

	"grok-gateway-proxy/internal/redact"
)

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

const currentSchemaVersion = 4

// migrateDataIfNeeded runs one-time data migrations for existing databases:
// backfilling columns added after the initial schema, and scrubbing
// credentials that may have been stored before write-time header
// sanitization existed. The schema_version marker in proxy_meta makes each
// migration run exactly once per database.
//
// The steps below are deliberately NOT wrapped in a single transaction: some
// of them rewrite the whole table in batches, and holding one write
// transaction across every batch would block readers for the entire
// migration. That makes each step's idempotence a load-bearing contract
// rather than a nice-to-have:
//
//   - every step must be safe to re-run from scratch (each is a conditional
//     UPDATE or a value-preserving rewrite, never an unconditional transform);
//   - schema_version is stamped only after all steps have succeeded.
//
// Together these mean an interrupted migration (crash, kill, disk full) simply
// re-runs from the beginning on the next startup and converges. If you add a
// step here, preserve both properties and bump currentSchemaVersion.
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
	// Compress existing rows that were written before transparent gzip
	// compression was introduced (schema_version < 3).
	if version < 3 {
		if err := s.compressExistingRows(); err != nil {
			return err
		}
	}
	// Normalize timestamps to fixed-width 30-char format for lexicographical
	// monotonic comparison and sorting in SQLite (schema_version < 4).
	if version < 4 {
		if err := s.normalizeTimestamps(); err != nil {
			return err
		}
	}
	_, err = s.db.Exec(
		`INSERT INTO proxy_meta (key, value) VALUES ('schema_version', ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		strconv.Itoa(currentSchemaVersion),
	)
	return err
}

// batchRewriteColumn rewrites one column of every matching row, handing each
// row's current value to rewrite, which reports the replacement and whether it
// differs. Rows whose rewrite reports no change are left alone.
//
// Rows are read in bounded batches and each batch is committed as a single
// transaction. Reading the whole table first pins every candidate row (and its
// rewritten value) in memory at once, while issuing one UPDATE per row costs a
// transaction — and under WAL an fsync — per row. Paging on the primary key
// keeps the scan indexed, so the total cost stays linear in the row count.
//
// column and where are interpolated into SQL and must be compile-time
// constants from this file; neither ever carries caller-supplied text.
// rewrite reports the replacement value, whether it differs from the original,
// or an error, which aborts the whole migration.
func (s *Store) batchRewriteColumn(column, where string, rewrite func(value []byte) ([]byte, bool, error)) error {
	const batchSize = 256
	lastID := ""
	for {
		nextID, exhausted, err := s.rewriteColumnBatch(column, where, lastID, batchSize, rewrite)
		if err != nil {
			return err
		}
		if exhausted {
			return nil
		}
		lastID = nextID
	}
}

// rewriteColumnBatch applies one batch and reports the last id it saw and
// whether that batch was the final one.
func (s *Store) rewriteColumnBatch(column, where, lastID string, batchSize int, rewrite func([]byte) ([]byte, bool, error)) (string, bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return "", true, err
	}
	// Rollback after a successful commit is a no-op returning ErrTxDone; the
	// only reason to call it is the early returns above.
	defer func() { _ = tx.Rollback() }()

	// The cursor cannot advance by OFFSET: an open Rows holds the single
	// pooled connection, so the UPDATEs have to wait for it to be closed.
	rows, err := tx.Query(
		fmt.Sprintf(`SELECT id, %s FROM request_logs WHERE id > ? %s ORDER BY id LIMIT ?`, column, where),
		lastID, batchSize,
	)
	if err != nil {
		return "", true, err
	}
	type pendingUpdate struct {
		id    string
		value []byte
	}
	var updates []pendingUpdate
	var scanned int
	var last string
	for rows.Next() {
		var id string
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			rows.Close()
			return "", true, err
		}
		rewritten, changed, err := rewrite(raw)
		if err != nil {
			rows.Close()
			return "", true, fmt.Errorf("rewrite %s of %s: %w", column, id, err)
		}
		if changed {
			updates = append(updates, pendingUpdate{id: id, value: rewritten})
		}
		last = id
		scanned++
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return "", true, err
	}
	if err := rows.Close(); err != nil {
		return "", true, err
	}
	if len(updates) > 0 {
		statement, err := tx.Prepare(fmt.Sprintf(`UPDATE request_logs SET %s = ? WHERE id = ?`, column))
		if err != nil {
			return "", true, err
		}
		defer statement.Close()
		for _, update := range updates {
			if _, err := statement.Exec(update.value, update.id); err != nil {
				return "", true, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return "", true, err
	}
	return last, scanned < batchSize, nil
}

func (s *Store) normalizeTimestamps() error {
	return s.batchRewriteColumn("started_at", `AND length(started_at) < 30`, func(raw []byte) ([]byte, bool, error) {
		t := parseTimestamp(string(raw))
		if t.IsZero() {
			return nil, false, nil
		}
		normalized := formatTimestamp(t)
		if normalized == string(raw) {
			return nil, false, nil
		}
		return []byte(normalized), true, nil
	})
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
		err = s.batchRewriteColumn(column, `AND `+column+` != ''`, func(raw []byte) ([]byte, bool, error) {
			// Decompress in case a partial compression migration left rows
			// compressed while version was not yet stamped.
			text := gunzipString(raw)
			redacted := redact.RedactStoredHeaders(text)
			if redacted == text {
				return nil, false, nil
			}
			// Re-compress the redacted value before storing.
			compressed, err := gzipString(redacted)
			if err != nil {
				return nil, false, err
			}
			return compressed, true, nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}
