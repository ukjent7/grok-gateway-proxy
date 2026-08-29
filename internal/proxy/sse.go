package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
)

// sse.go contains the line-level SSE pass-through reader used by the gateway
// adapters. The proxy forwards the upstream stream byte-for-byte except for
// minimal, targeted rewrites: at most one injected per-line transform plus an
// optional ping-keepalive filter.

// sseLineTransformer buffers the upstream stream line by line, applies an
// optional per-line transform, and drops events by their `data:` payload:
// keepalive pings (isPingPayload) and anything matching dropPayload (e.g.
// event types outside the client's vocabulary). When an event is dropped,
// the blank line terminating it is consumed as well so the stream stays
// byte-identical apart from the removed event.
type sseLineTransformer struct {
	reader        *bufio.Reader
	pending       bytes.Buffer
	eventLine     []byte
	skipBlank     bool
	done          bool
	err           error
	transformLine func([]byte) []byte
	isPingPayload func(payload []byte) bool
	dropPayload   func(payload []byte) bool
}

func newSSELineTransformer(reader io.Reader, transformLine func([]byte) []byte, isPingPayload, dropPayload func([]byte) bool) *sseLineTransformer {
	return &sseLineTransformer{
		reader:        bufio.NewReaderSize(reader, 64*1024),
		transformLine: transformLine,
		isPingPayload: isPingPayload,
		dropPayload:   dropPayload,
	}
}

func (r *sseLineTransformer) Read(p []byte) (int, error) {
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
			r.eventLine = append(r.eventLine[:0], r.applyLine(line)...)
		case bytes.HasPrefix(trimmed, []byte("data:")):
			payload := bytes.TrimSpace(trimmed[len("data:"):])
			if (r.isPingPayload != nil && r.isPingPayload(payload)) || (r.dropPayload != nil && r.dropPayload(payload)) {
				r.eventLine = nil
				r.skipBlank = true
				continue
			}
			r.flushEventLine()
			r.pending.Write(r.applyLine(line))
		default:
			// Drop the blank line that terminates a dropped ping event so the
			// stream stays byte-identical apart from the removed event.
			if r.skipBlank && len(trimmed) == 0 {
				r.skipBlank = false
				continue
			}
			r.skipBlank = false
			r.flushEventLine()
			r.pending.Write(r.applyLine(line))
		}
	}
}

func (r *sseLineTransformer) applyLine(line []byte) []byte {
	if r.transformLine == nil {
		return line
	}
	return r.transformLine(line)
}

func (r *sseLineTransformer) flushEventLine() {
	if len(r.eventLine) > 0 {
		r.pending.Write(r.eventLine)
		r.eventLine = nil
	}
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

// rewriteLegacyReasoningEventNames renames the legacy reasoning stream event
// names to the newer `reasoning_text` variants. `data:` lines are rewritten
// via structured JSON property replacement so only the "type" field value is
// touched — event names embedded in other string values (e.g. delta text)
// survive intact, and the rename is immune to key-order/whitespace variations
// in the upstream serialization. `event:` lines are plain text and use a
// targeted match on the event name.
func rewriteLegacyReasoningEventNames(line []byte) []byte {
	// `event:` lines are not JSON — match the announced event name directly.
	if trimmed := bytes.TrimLeft(line, " \t"); bytes.HasPrefix(trimmed, []byte("event:")) {
		name := bytes.TrimSpace(trimmed[len("event:"):])
		switch string(name) {
		case "response.reasoning.delta":
			return bytes.Replace(line, name, []byte("response.reasoning_text.delta"), 1)
		case "response.reasoning.done":
			return bytes.Replace(line, name, []byte("response.reasoning_text.done"), 1)
		}
		return line
	}
	// `data:` and other lines: parse JSON string boundaries and rewrite only
	// the value of the "type" property.
	line, _ = replaceJSONPropertyStringValue(line, "type", "response.reasoning.delta", `"response.reasoning_text.delta"`)
	line, _ = replaceJSONPropertyStringValue(line, "type", "response.reasoning.done", `"response.reasoning_text.done"`)
	return line
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
