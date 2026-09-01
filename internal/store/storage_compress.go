package store

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
)

var compressedTag = []byte("GZ")
var legacyCompressedPrefix = []byte("\x1f\x8bGZ")

const gzipID1, gzipID2 = 0x1f, 0x8b

func gzipBytes(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return []byte{}, nil
	}
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	gw.Extra = compressedTag
	if _, err := gw.Write(data); err != nil {
		return nil, err
	}
	if err := gw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func compressedPayload(data []byte) ([]byte, bool) {
	if len(data) < 2 || data[0] != gzipID1 || data[1] != gzipID2 {
		return nil, false
	}
	if bytes.HasPrefix(data, legacyCompressedPrefix) {
		return data[len(legacyCompressedPrefix):], true
	}
	return data, true
}

func gunzipBytes(data []byte) ([]byte, error) {
	payload, compressed := compressedPayload(data)
	if !compressed {
		return data, nil
	}
	gr, err := gzip.NewReader(bytes.NewReader(payload))
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

func gunzipString(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	decompressed, err := gunzipBytes(data)
	if err != nil {
		return string(data)
	}
	return string(decompressed)
}

func decompressBody(data []byte) []byte {
	if len(data) == 0 {
		return []byte{}
	}
	decompressed, err := gunzipBytes(data)
	if err != nil {
		return data
	}
	return decompressed
}

func (s *Store) compressExistingRows() error {
	for _, column := range capturedColumns {
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
		// Skip already-compressed rows, in either on-disk form.
		if _, compressed := compressedPayload(raw); compressed {
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
