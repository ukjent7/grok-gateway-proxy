package store

import (
	"database/sql"
	"errors"
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

func (s *Store) normalizeTimestamps() error {
	rows, err := s.db.Query(`SELECT id, started_at FROM request_logs WHERE length(started_at) < 30`)
	if err != nil {
		return err
	}
	defer rows.Close()
	type pendingTimeUpdate struct {
		id        string
		startedAt string
	}
	var updates []pendingTimeUpdate
	for rows.Next() {
		var id, raw string
		if err := rows.Scan(&id, &raw); err != nil {
			return err
		}
		t := parseTimestamp(raw)
		if !t.IsZero() {
			updates = append(updates, pendingTimeUpdate{id: id, startedAt: formatTimestamp(t)})
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, update := range updates {
		if _, err := s.db.Exec(`UPDATE request_logs SET started_at = ? WHERE id = ?`, update.startedAt, update.id); err != nil {
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
			value []byte
		}
		var updates []pendingUpdate
		for rows.Next() {
			var id string
			var raw []byte
			if err := rows.Scan(&id, &raw); err != nil {
				rows.Close()
				return err
			}
			// Decompress in case a partial compression migration left rows
			// compressed while version was not yet stamped.
			text := gunzipString(raw)
			if redacted := redact.RedactStoredHeaders(text); redacted != text {
				// Re-compress the redacted value before storing.
				compressed, err := gzipString(redacted)
				if err != nil {
					rows.Close()
					return err
				}
				updates = append(updates, pendingUpdate{id: id, value: compressed})
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
