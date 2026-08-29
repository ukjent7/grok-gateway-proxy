package proxy

import (
	"bytes"
	"errors"
	"io"
	"net/http"
)

// responseCapture wraps the client-facing ResponseWriter to snapshot the
// status, headers, and (capped) body the client actually receives.
type responseCapture struct {
	http.ResponseWriter
	statusCode int
	headers    http.Header
	body       *cappedBuffer
}

func newResponseCapture(w http.ResponseWriter, limit int64) *responseCapture {
	return &responseCapture{ResponseWriter: w, body: newCappedBuffer(limit)}
}

func (w *responseCapture) WriteHeader(statusCode int) {
	if w.statusCode != 0 {
		return
	}
	w.statusCode = statusCode
	w.headers = cloneHeaders(w.Header())
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *responseCapture) Write(data []byte) (int, error) {
	if w.statusCode == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(data)
	if n > 0 {
		_, _ = w.body.Write(data[:n])
	}
	return n, err
}

func (w *responseCapture) Flush() {
	if w.statusCode == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// cappedBuffer is an io.Writer that stops accepting bytes once limit is
// reached, so a TeeReader can snapshot the upstream stream without unbounded
// buffering. Writes beyond the limit are reported as written so the
// TeeReader keeps passing the full stream through.
type cappedBuffer struct {
	buf       bytes.Buffer
	limit     int64
	truncated bool
}

func newCappedBuffer(limit int64) *cappedBuffer {
	c := &cappedBuffer{limit: limit}
	const preAlloc = 64 * 1024
	if limit > 0 && limit < preAlloc {
		c.buf.Grow(int(limit))
	} else if limit >= preAlloc {
		c.buf.Grow(preAlloc)
	}
	return c
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if c.truncated {
		return len(p), nil
	}
	remaining := c.limit - int64(c.buf.Len())
	if remaining <= 0 {
		c.truncated = true
		return len(p), nil
	}
	if int64(len(p)) > remaining {
		_, _ = c.buf.Write(p[:remaining])
		c.truncated = true
	} else {
		_, _ = c.buf.Write(p)
	}
	return len(p), nil
}

func (c *cappedBuffer) Bytes() []byte { return c.buf.Bytes() }

// cloneHeaders creates a deep copy of headers for audit capture.
func cloneHeaders(src http.Header) http.Header {
	dst := make(http.Header, len(src))
	for name, values := range src {
		dst[name] = append([]string(nil), values...)
	}
	return dst
}

func copyAndCapture(w http.ResponseWriter, reader io.Reader, streaming bool, limit int64) ([]byte, error) {
	capture := newCappedBuffer(limit)
	buf := make([]byte, 32*1024)
	flusher, canFlush := w.(http.Flusher)
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if _, writeErr := w.Write(chunk); writeErr != nil {
				return capture.Bytes(), writeErr
			}
			_, _ = capture.Write(chunk)
			if streaming && canFlush {
				flusher.Flush()
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return capture.Bytes(), nil
			}
			return capture.Bytes(), err
		}
	}
}
