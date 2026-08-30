package store

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
)

// compressedColumns are the text/blob columns that benefit from gzip
// compression. Bodies are the dominant space consumers (JSON/SSE text
// compresses 5-10x); headers are smaller but also highly compressible.
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

// gzipString compresses a header JSON string. Returns empty bytes for empty
// input so no compression overhead is added for missing fields.
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

// compressExistingRows compresses all body and header columns for rows that
// were written before transparent compression (schema_version < 3). A row is
// only updated where compression actually saved space, so re-running this is a
// no-op on already-compressed data.
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
	return s.batchRewriteColumn(column, fmt.Sprintf(`AND length(%s) > 0`, column), func(raw []byte) ([]byte, bool, error) {
		// Skip already-compressed rows (the magic prefix).
		if bytes.HasPrefix(raw, []byte(compressedMagic)) {
			return nil, false, nil
		}
		compressed, err := gzipBytes(raw)
		if err != nil {
			return nil, false, err
		}
		// Only update if compression actually saved space.
		if len(compressed) >= len(raw) {
			return nil, false, nil
		}
		return compressed, true, nil
	})
}
