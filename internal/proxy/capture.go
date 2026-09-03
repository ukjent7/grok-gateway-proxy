package proxy

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strconv"
)

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

func newCappedBufferWithHint(limit int64, contentLength string) *cappedBuffer {
	if contentLength != "" {
		if n, err := strconv.ParseInt(contentLength, 10, 64); err == nil && n > 0 {
			hint := n
			if limit > 0 && hint > limit {
				hint = limit
			}
			if hint < 64*1024 {
				c := &cappedBuffer{limit: limit}
				c.buf.Grow(int(hint))
				return c
			}
		}
	}
	return newCappedBuffer(limit)
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if c.truncated {
		return len(p), nil
	}
	if c.limit > 0 {
		remaining := c.limit - int64(c.buf.Len())
		if remaining <= 0 {
			c.truncated = true
			return len(p), nil
		}
		if int64(len(p)) > remaining {
			_, _ = c.buf.Write(p[:remaining])
			c.truncated = true
			return len(p), nil
		}
	}
	_, _ = c.buf.Write(p)
	return len(p), nil
}

func (c *cappedBuffer) Bytes() []byte { return c.buf.Bytes() }

func cloneHeaders(src http.Header) http.Header {
	dst := make(http.Header, len(src))
	for name, values := range src {
		dst[name] = append([]string(nil), values...)
	}
	return dst
}

func copyStream(w http.ResponseWriter, reader io.Reader, usage *usageTracker) error {
	buf := make([]byte, 32*1024)
	flusher, canFlush := w.(http.Flusher)
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if usage != nil {
				usage.observe(chunk)
			}
			if _, writeErr := w.Write(chunk); writeErr != nil {
				return writeErr
			}
			if canFlush {
				flusher.Flush()
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}
