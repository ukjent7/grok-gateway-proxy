package main

import (
	"bufio"
	"bytes"
	"encoding/json"
)

// sse.go contains the line-level SSE readers used by the gateway adapters.

type senseNovaSSEReader struct {
	reader  *bufio.Reader
	pending bytes.Buffer
	done    bool
	err     error
}

func (r *senseNovaSSEReader) Read(p []byte) (int, error) {
	if r.pending.Len() > 0 {
		return r.pending.Read(p)
	}
	if r.done {
		return 0, r.err
	}
	line, err := r.reader.ReadBytes('\n')
	if len(line) > 0 {
		r.pending.Write(transformSenseNovaSSELine(line))
	}
	if err != nil {
		r.done = true
		r.err = err
	}
	if r.pending.Len() > 0 {
		return r.pending.Read(p)
	}
	return 0, err
}

func transformSenseNovaSSELine(line []byte) []byte {
	lineEnd := []byte(nil)
	content := line
	if bytes.HasSuffix(content, []byte("\r\n")) {
		lineEnd = []byte("\r\n")
		content = content[:len(content)-2]
	} else if bytes.HasSuffix(content, []byte("\n")) {
		lineEnd = []byte("\n")
		content = content[:len(content)-1]
	} else if bytes.HasSuffix(content, []byte("\r")) {
		lineEnd = []byte("\r")
		content = content[:len(content)-1]
	}
	dataIndex := bytes.Index(content, []byte("data:"))
	if dataIndex < 0 || len(bytes.TrimSpace(content[:dataIndex])) != 0 {
		return line
	}
	payloadStart := dataIndex + len("data:")
	for payloadStart < len(content) && isJSONWhitespace(content[payloadStart]) {
		payloadStart++
	}
	payloadEnd := len(content)
	for payloadEnd > payloadStart && isJSONWhitespace(content[payloadEnd-1]) {
		payloadEnd--
	}
	payload := content[payloadStart:payloadEnd]
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return line
	}
	converted, err := transformSenseNovaResponseBody(payload)
	if err != nil {
		return line
	}
	converted, err = stripEmptySenseNovaToolCallDeltaFields(converted)
	if err != nil || bytes.Equal(converted, payload) {
		return line
	}
	result := make([]byte, 0, len(line))
	result = append(result, content[:payloadStart]...)
	result = append(result, converted...)
	result = append(result, content[payloadEnd:]...)
	result = append(result, lineEnd...)
	return result
}

// rewriteVercelReasoningEvent renames the legacy reasoning stream event names
// to the newer `reasoning_text` variants. Only the exact type tag / event name
// is rewritten, so delta or text content that merely quotes the old name is
// never corrupted.
func rewriteVercelReasoningEvent(line []byte) []byte {
	line = bytes.ReplaceAll(line, []byte(`"type":"response.reasoning.delta"`), []byte(`"type":"response.reasoning_text.delta"`))
	line = bytes.ReplaceAll(line, []byte(`"type":"response.reasoning.done"`), []byte(`"type":"response.reasoning_text.done"`))
	line = bytes.ReplaceAll(line, []byte("event: response.reasoning.delta"), []byte("event: response.reasoning_text.delta"))
	line = bytes.ReplaceAll(line, []byte("event: response.reasoning.done"), []byte("event: response.reasoning_text.done"))
	return line
}

type vercelSSEReader struct {
	reader    *bufio.Reader
	pending   bytes.Buffer
	eventLine []byte
	skipBlank bool
	done      bool
	err       error
}

func (r *vercelSSEReader) Read(p []byte) (int, error) {
	for {
		if r.pending.Len() > 0 {
			return r.pending.Read(p)
		}
		if r.done {
			return 0, r.err
		}
		line, err := r.reader.ReadBytes('\n')
		if len(line) == 0 && err != nil {
			r.done = true
			r.err = err
			r.flushEventLine()
			if r.pending.Len() > 0 {
				return r.pending.Read(p)
			}
			return 0, err
		}
		if len(line) == 0 {
			continue
		}
		trimmed := bytes.TrimRight(line, "\r\n")
		switch {
		case bytes.HasPrefix(trimmed, []byte("event:")):
			r.flushEventLine()
			r.eventLine = append(r.eventLine[:0], rewriteVercelReasoningEvent(line)...)
		case bytes.HasPrefix(trimmed, []byte("data:")):
			payload := bytes.TrimSpace(trimmed[len("data:"):])
			if isVercelPing(payload) {
				r.eventLine = nil
				r.skipBlank = true
				continue
			}
			r.flushEventLine()
			r.pending.Write(rewriteVercelReasoningEvent(line))
		default:
			// Drop the blank line that terminates a dropped ping event so the
			// stream stays byte-identical apart from the removed event.
			if r.skipBlank && len(trimmed) == 0 {
				r.skipBlank = false
				continue
			}
			r.skipBlank = false
			r.flushEventLine()
			r.pending.Write(line)
		}
	}
}

func (r *vercelSSEReader) flushEventLine() {
	if len(r.eventLine) > 0 {
		r.pending.Write(r.eventLine)
		r.eventLine = nil
	}
}

func isVercelPing(payload []byte) bool {
	if len(payload) == 0 {
		return false
	}
	if bytes.Equal(payload, []byte("ping")) {
		return true
	}
	var event struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(payload, &event) != nil {
		return false
	}
	return event.Type == "ping"
}
