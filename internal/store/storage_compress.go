package store

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
)

var compressedColumns = []string{
	"request_body", "upstream_body", "upstream_response_body", "response_body",
	"request_headers", "upstream_headers", "upstream_response_headers", "response_headers",
}

const compressedMagic = "\x1f\x8bGZ" // gzip header (0x1f 0x8b) + "GZ" tag

func gzipBytes(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return []byte{}, nil
	}
	var buf bytes.Buffer
	buf.WriteString(compressedMagic)
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(data); err != nil {
		return nil, err
	}
	if err := gw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func gunzipBytes(data []byte) ([]byte, error) {
	if len(data) == 0 || !bytes.HasPrefix(data, []byte(compressedMagic)) {
		return data, nil
	}
	gr, err := gzip.NewReader(bytes.NewReader(data[len(compressedMagic):]))
	if err != nil {
		return nil, fmt.Errorf("decompress: %w", err)
	}
	defer gr.Close()
	return io.ReadAll(gr)
}
func gzipString(s string) ([]byte, error) {
	if s == "" {
		return []byte{}, nil
	}
	return gzipBytes([]byte(s))
}

// gunzipString decompresses a header column. Returns "" for empty input;
// passes through raw data that doesn't have the compression magic.
func gunzipString(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	decompressed, err := gunzipBytes(data)
	if err != nil {
		return string(data) // fallback to raw
	}
	return string(decompressed)
}

// decompressBody decompresses a body column, returning the original bytes.
// Empty and non-compressed data pass through unchanged.
func decompressBody(data []byte) []byte {
	if len(data) == 0 {
		return []byte{}
	}
	decompressed, err := gunzipBytes(data)
	if err != nil {
		return data // fallback to raw
	}
	return decompressed
}

func (s *Store) compressExistingRows() error {
	for _, column := range compressedColumns {
		if err := s.compressColumn(column); err != nil {
			return fmt.Errorf("compress column %s: %w", column, err)
		}
	}
	return nil
}

func (s *Store) compressColumn(column string) error {
	// Check whether this column exists (legacy *_actual columns may not).
	exists, err := s.columnExists(column)
	if err != nil || !exists {
		return err
	}
	rows, err := s.db.Query(fmt.Sprintf(`SELECT id, %s FROM request_logs WHERE length(%s) > 0`, column, column))
	if err != nil {
		return err
	}
	type pendingUpdate struct {
		id  string
		val []byte
	}
	var updates []pendingUpdate
	for rows.Next() {
		var id string
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			rows.Close()
			return err
		}
		// Skip already-compressed rows (the magic prefix).
		if bytes.HasPrefix(raw, []byte(compressedMagic)) {
			continue
		}
		compressed, err := gzipBytes(raw)
		if err != nil {
			rows.Close()
			return err
		}
		// Only update if compression actually saved space.
		if len(compressed) < len(raw) {
			updates = append(updates, pendingUpdate{id: id, val: compressed})
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
		if _, err := s.db.Exec(
			fmt.Sprintf(`UPDATE request_logs SET %s = ? WHERE id = ?`, column),
			update.val, update.id,
		); err != nil {
			return err
		}
	}
	return nil
}
