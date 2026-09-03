package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
)

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
	filterStats   *sseFilterStats
}

func newSSELineTransformer(reader io.Reader, transformLine func([]byte) []byte, stats *sseFilterStats, isPingPayload, dropPayload func(payload []byte) bool) *sseLineTransformer {
	if stats == nil {
		stats = &sseFilterStats{}
	}
	return &sseLineTransformer{
		reader:        bufio.NewReaderSize(reader, 64*1024),
		transformLine: transformLine,
		isPingPayload: isPingPayload,
		dropPayload:   dropPayload,
		filterStats:   stats,
	}
}

func (r *sseLineTransformer) stats() sseFilterStats { return *r.filterStats }

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

		skipBlank := r.skipBlank && len(trimmed) == 0
		r.skipBlank = false
		if skipBlank {
			continue
		}
		switch {
		case bytes.HasPrefix(trimmed, []byte("event:")):
			r.flushEventLine()
			r.eventLine = append(r.eventLine[:0], r.applyLine(line)...)
		case bytes.HasPrefix(trimmed, []byte("data:")):

			if r.isPingPayload != nil && r.isPingPayload(dataLinePayload(trimmed)) {
				if r.filterStats != nil {
					r.filterStats.droppedPings++
				}
				r.eventLine = nil
				r.skipBlank = true
				continue
			}

			out := r.applyLine(line)
			if r.dropPayload != nil && r.dropPayload(dataLinePayload(bytes.TrimRight(out, "\r\n"))) {
				if r.filterStats != nil {
					r.filterStats.droppedUnknown++
				}
				r.eventLine = nil
				r.skipBlank = true
				continue
			}
			r.flushEventLine()
			r.pending.Write(out)
		default:
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

func dataLinePayload(trimmedLine []byte) []byte {
	if !bytes.HasPrefix(trimmedLine, []byte("data:")) {
		return nil
	}
	return bytes.TrimSpace(trimmedLine[len("data:"):])
}

func (r *sseLineTransformer) flushEventLine() {
	if len(r.eventLine) > 0 {
		r.pending.Write(r.eventLine)
		r.eventLine = nil
	}
}

func rewriteSSEDataPayload(line []byte, fn func([]byte) []byte) []byte {
	lineEnd := []byte(nil)
	content := line
	switch {
	case bytes.HasSuffix(content, []byte("\r\n")):
		lineEnd, content = []byte("\r\n"), content[:len(content)-2]
	case bytes.HasSuffix(content, []byte("\n")):
		lineEnd, content = []byte("\n"), content[:len(content)-1]
	case bytes.HasSuffix(content, []byte("\r")):
		lineEnd, content = []byte("\r"), content[:len(content)-1]
	}
	dataIndex := bytes.Index(content, []byte("data:"))

	if dataIndex < 0 || len(bytes.TrimSpace(content[:dataIndex])) != 0 {
		return line
	}
	payloadStart := skipJSONWhitespace(content, dataIndex+len("data:"))
	payloadEnd := len(content)
	for payloadEnd > payloadStart && isJSONWhitespace(content[payloadEnd-1]) {
		payloadEnd--
	}
	payload := content[payloadStart:payloadEnd]
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return line
	}
	rewritten := fn(payload)
	if bytes.Equal(rewritten, payload) {
		return line
	}
	out := make([]byte, 0, len(line)-(payloadEnd-payloadStart)+len(rewritten))
	out = append(out, content[:payloadStart]...)
	out = append(out, rewritten...)
	out = append(out, content[payloadEnd:]...)
	return append(out, lineEnd...)
}

func transformSenseNovaSSELine(line []byte) []byte {

	if !bytes.Contains(line, []byte(toolCallsKey)) && !bytes.Contains(line, []byte("finish_reason")) {
		return line
	}
	return rewriteSSEDataPayload(line, transformSenseNovaPayload)
}

var legacyReasoningEventRenames = []jsonMemberRewrite{
	newJSONMemberRewrite(typeKey, "response.reasoning.delta", []byte(`"response.reasoning_text.delta"`), false),
	newJSONMemberRewrite(typeKey, "response.reasoning.done", []byte(`"response.reasoning_text.done"`), false),
}

func renameLegacyReasoningPayload(payload []byte) []byte {
	renamed, _ := applyJSONMemberRewrites(payload, legacyReasoningEventRenames...)
	return renamed
}

func rewriteLegacyReasoningEventNames(line []byte) []byte {

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
	return rewriteSSEDataPayload(line, renameLegacyReasoningPayload)
}

func (s *sseFilterStats) isPingPayload(payload []byte) bool {
	if len(payload) == 0 {
		return false
	}
	if bytes.Equal(payload, []byte("ping")) {
		return true
	}

	if payload[0] != '{' {
		return false
	}
	var event struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(payload, &event) != nil {
		return false
	}
	return event.Type == "ping"
}
