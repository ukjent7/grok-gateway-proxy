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

// stats reports what this one stream's filter dropped or renamed. The request
// handler reads it after the stream ends, from the goroutine that drove it.
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
		// A drop pending from the previous line only ever swallows the blank
		// line right after it; anything else clears the pending skip so a kept
		// event keeps its own terminator.
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
			// The ping check runs on the raw payload — a ping is a ping before
			// and after a rename, and pings are the frames worth recognising
			// without paying for the line transform first.
			if r.isPingPayload != nil && r.isPingPayload(dataLinePayload(trimmed)) {
				if r.filterStats != nil {
					r.filterStats.droppedPings++
				}
				r.eventLine = nil
				r.skipBlank = true
				continue
			}
			// Everything else is judged on the transformed line, so a frame is
			// renamed once and cannot be kept or dropped for reasons that
			// describe bytes the client never receives.
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

// dataLinePayload returns the payload of a `data:` line with its trailing
// newline already trimmed, and nothing for any other line. The line transform
// rewrites a payload in place and never touches the framing, so the payload of
// the original line and of the transformed one come out of here the same way.
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

// rewriteSSEDataPayload applies fn to the JSON payload of a `data:` line and
// puts the result back between the same framing bytes. A line that is not a
// data line, and a payload that is empty or the [DONE] sentinel, come back
// untouched without fn ever seeing them — so fn only has to handle payloads.
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
	// The prefix has to be nothing but whitespace, or the match is the string
	// "data:" inside some value rather than the field that opens the line.
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
	// Fast path for non-tool payloads: SenseNova only rewrites the tool_calls
	// type, finish_reason and the empty identity fields a continuation echoes,
	// so text deltas bypass JSON parsing entirely. That is the overwhelming
	// majority of chunks on a text-heavy stream.
	if !bytes.Contains(line, []byte(toolCallsKey)) && !bytes.Contains(line, []byte("finish_reason")) {
		return line
	}
	return rewriteSSEDataPayload(line, transformSenseNovaPayload)
}

// legacyReasoningEventRenames maps each legacy reasoning stream event name onto
// the newer `reasoning_text` variant. Only the `type` member is edited, so the
// same string appearing as delta text survives as the content it is.
var legacyReasoningEventRenames = []jsonMemberRewrite{
	newJSONMemberRewrite(typeKey, "response.reasoning.delta", []byte(`"response.reasoning_text.delta"`), false),
	newJSONMemberRewrite(typeKey, "response.reasoning.done", []byte(`"response.reasoning_text.done"`), false),
}

// renameLegacyReasoningPayload rewrites the event `type` of a JSON payload.
func renameLegacyReasoningPayload(payload []byte) []byte {
	renamed, _ := applyJSONMemberRewrites(payload, legacyReasoningEventRenames...)
	return renamed
}

// rewriteLegacyReasoningEventNames renames the legacy reasoning stream event
// names to their newer `reasoning_text` variants. `data:` payloads are rewritten
// through the JSON member-rewrite engine, which touches only the value of the
// `type` member and leaves the rest of the line — key order, whitespace, and any
// other string that happens to contain the old event name — as it arrived.
// `event:` lines are plain text and match the announced name directly.
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
	return rewriteSSEDataPayload(line, renameLegacyReasoningPayload)
}

// isPingPayload reports whether payload is a keepalive ping frame.
// It is pure: counting is done by the caller (sseLineTransformer) via filterStats.
func (s *sseFilterStats) isPingPayload(payload []byte) bool {
	if len(payload) == 0 {
		return false
	}
	if bytes.Equal(payload, []byte("ping")) {
		return true
	}
	// Fast path: non-JSON payloads (e.g. "[DONE]" or plain text) are not pings.
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
