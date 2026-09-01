package proxy

// jsonrewrite holds the span-preserving JSON member rewriter used for SSE
// payloads. It rewrites only the value of a specific member (e.g. "type")
// in place via byte splices, preserving key order, whitespace, and every
// other member's bytes. A typed map[string]any decode+marshal would lose that
// (see TestStandardRenameHandlesSpacedAndNestedSerialization) and would also
// pay a decode for every streaming chunk that the byte prefilter currently
// skips (BenchmarkSSESenseNovaTransform: 151 ns / 0 alloc vs ~20µs/149 allocs
// for a real edit). The generic machinery (docMayMatch prefilter, decoder
// offset framing) is the cost of that span preservation — keep it limited to
// the two SSE rewrite tables (legacyReasoningEventRenames,
// senseNovaResponseRewrites) and the SenseNova delta-field stripper.

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
)

const (
	typeKey      = "type"
	toolCallsKey = "tool_calls"
)

var (
	toolCallsKeyToken = []byte(`"` + toolCallsKey + `"`)
	emptyStringToken  = []byte(`""`)
)

type jsonMemberRewrite struct {
	key, from           string
	to                  []byte
	inToolCallsOnly     bool
	keyToken, fromToken []byte
}

func newJSONMemberRewrite(key, from string, to []byte, inToolCallsOnly bool) jsonMemberRewrite {
	return jsonMemberRewrite{
		key:             key,
		from:            from,
		to:              to,
		inToolCallsOnly: inToolCallsOnly,
		keyToken:        []byte(`"` + key + `"`),
		fromToken:       []byte(`"` + from + `"`),
	}
}

type jsonSpanEdit struct {
	start, end int
	to         []byte
}

type jsonRewriter struct {
	src      []byte
	decoder  *json.Decoder
	rewrites []jsonMemberRewrite
	edits    []jsonSpanEdit
	failed   bool
}

func applyJSONMemberRewrites(doc []byte, rewrites ...jsonMemberRewrite) ([]byte, bool) {
	if !docMayMatch(doc, rewrites) {
		return doc, false
	}
	rewriter := &jsonRewriter{src: doc, decoder: json.NewDecoder(bytes.NewReader(doc)), rewrites: rewrites}
	switch token, err := rewriter.decoder.Token(); {
	case err != nil:
		return doc, false
	case token == json.Delim('{'):
		rewriter.object(false)
	case token == json.Delim('['):
		rewriter.array(false)
	default:
		// A scalar root has no members to edit.
		return doc, false
	}
	if rewriter.failed || len(rewriter.edits) == 0 {
		return doc, false
	}
	edits := rewriter.edits

	// The walk follows the source, so the edits are already ascending and
	// disjoint: a matched value is always a string, and a string has no
	// members for a second edit to live in.
	out := make([]byte, 0, len(doc))
	var copied int
	for _, edit := range edits {
		out = append(out, doc[copied:edit.start]...)
		out = append(out, edit.to...)
		copied = edit.end
	}
	return append(out, doc[copied:]...), true
}

// object walks the members of an object whose '{' has been consumed. inToolCalls
// says whether this object sits below a "tool_calls" member.
func (r *jsonRewriter) object(inToolCalls bool) {
	for !r.failed && r.decoder.More() {
		name, err := r.decoder.Token()
		if err != nil {
			r.failed = true
			return
		}
		key, ok := name.(string)
		if !ok {
			r.failed = true
			return
		}
		r.value(key, int(r.decoder.InputOffset()), inToolCalls || key == toolCallsKey)
	}
	if _, err := r.decoder.Token(); err != nil { // the closing '}'
		r.failed = true
	}
}

// array walks the elements of an array whose '[' has been consumed. Being
// below a tool_calls member is what the scope means; the array itself is the
// member, so its elements inherit it.
func (r *jsonRewriter) array(inToolCalls bool) {
	cursor := int(r.decoder.InputOffset())
	for !r.failed && r.decoder.More() {
		cursor = r.value("", cursor, inToolCalls)
	}
	if _, err := r.decoder.Token(); err != nil { // the closing ']'
		r.failed = true
	}
}

// value consumes the value that begins at the current decoder position,
// recording an edit if one of the rewrites matches it. framingEnd is where the
// bytes before the value started to be read: the value's own offset is the
// first byte after the whitespace, colon or comma that separates it, which is
// what lets the edit splice into the original document.
//
// It returns the end of the value it read, which is where the next element of
// an array begins.
func (r *jsonRewriter) value(key string, framingEnd int, inToolCalls bool) int {
	start := nextJSONToken(r.src, framingEnd)
	token, err := r.decoder.Token()
	if err != nil {
		r.failed = true
		return framingEnd
	}
	end := int(r.decoder.InputOffset())
	if delimiter, isContainer := token.(json.Delim); isContainer {
		switch delimiter {
		case '{':
			r.object(inToolCalls)
		case '[':
			r.array(inToolCalls)
		}
		return int(r.decoder.InputOffset())
	}
	text, isString := token.(string)
	if !isString {
		return end
	}
	for _, rewrite := range r.rewrites {
		if rewrite.key == key && rewrite.from == text && (!rewrite.inToolCallsOnly || inToolCalls) {
			r.edits = append(r.edits, jsonSpanEdit{start: start, end: end, to: rewrite.to})
		}
	}
	return end
}

// nextJSONToken returns the offset of the first byte of the next value at or
// after from, skipping the whitespace and the ':' or ',' that frame it.
func nextJSONToken(src []byte, from int) int {
	at := skipJSONWhitespace(src, from)
	if at < len(src) && (src[at] == ':' || src[at] == ',') {
		at = skipJSONWhitespace(src, at+1)
	}
	return at
}

// docMayMatch is the whole-document prefilter: no rewrite can fire unless some
// member name and some value it rewrites from appear in the bytes at all. It
// over-approximates — a document that carries both tokens in unrelated places
// still gets walked — because missing a match would forward the payload this
// rewrite exists to fix.
func docMayMatch(doc []byte, rewrites []jsonMemberRewrite) bool {
	for _, rewrite := range rewrites {
		if rewrite.inToolCallsOnly && !bytes.Contains(doc, toolCallsKeyToken) {
			continue
		}
		if bytes.Contains(doc, rewrite.keyToken) && bytes.Contains(doc, rewrite.fromToken) {
			return true
		}
	}
	return false
}

// marshalJSONNoEscape marshals v compactly, leaving <, > and & unescaped.
// encoding/json rewrites them to \u003c / \u003e / \u0026 by default, which
// inflates every rewritten request body (a bare "<" triples in size) and makes
// the logged upstream body diverge from the bytes the client actually sent.
// Both matter for this proxy: prompts are mostly source code, and comparing the
// two bodies is what the audit log is for. The result goes to a JSON API over
// the wire and is never embedded in HTML, so dropping the escaping is safe.
func marshalJSONNoEscape(value any) ([]byte, error) {
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	// Encoder.Encode appends a trailing newline; Marshal would not.
	return bytes.TrimRight(out.Bytes(), "\n"), nil
}

// decodeJSONDocument unmarshals a JSON document into a generic value while
// keeping number literals as text. encoding/json decodes every number into
// float64 by default, so a document that only needs some unrelated field
// renamed comes back with its numbers rewritten: 9007199254740992 for
// 9007199254740993, `1` for `1.0`, `1000` for `1e3`. UseNumber leaves the
// original spelling of every number we do not touch alone.
func decodeJSONDocument(body []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return value, nil
}

// transformToolCallType rewrites the "type" of every entry in a tool_calls
// array from one variant to another. Everything else is untouched, including
// the `type` of a tool definition, which is the same word meaning something
// else.
func transformToolCallType(body []byte, from, to string) []byte {
	rewritten, _ := applyJSONMemberRewrites(body, newJSONMemberRewrite(typeKey, from, []byte(strconv.Quote(to)), true))
	return rewritten
}

// sanitizeSenseNovaToolCallHistory removes incomplete tool-call records from
// the request history. A partially emitted client-side tool call has no
// function name to execute, so forwarding it makes SenseNova reject the
// entire request with "function/name/arguments cannot be empty". Its matching
// tool result is removed as well because it would otherwise be an orphan.
func sanitizeSenseNovaToolCallHistory(body []byte) ([]byte, error) {
	// Fast path: body without tool_calls or messages cannot need cleaning.
	if !bytes.Contains(body, toolCallsKeyToken) && !bytes.Contains(body, []byte(`"messages"`)) {
		return body, nil
	}
	payload, err := decodeJSONDocument(body)
	if err != nil {
		return body, nil
	}
	root, ok := payload.(map[string]any)
	if !ok {
		return body, nil
	}
	messages, ok := root["messages"].([]any)
	if !ok {
		return body, nil
	}

	validCallIDs := make(map[string]struct{})
	changed := false
	for _, item := range messages {
		message, ok := item.(map[string]any)
		if !ok {
			continue
		}
		calls, ok := message[toolCallsKey].([]any)
		if !ok {
			continue
		}

		filtered := make([]any, 0, len(calls))
		for _, item := range calls {
			call, ok := item.(map[string]any)
			if !ok || !validSenseNovaToolCall(call) {
				changed = true
				continue
			}
			filtered = append(filtered, item)
			if id, ok := nonEmptyString(call["id"]); ok {
				validCallIDs[id] = struct{}{}
			}
		}
		if len(filtered) == 0 {
			delete(message, toolCallsKey)
			if len(calls) > 0 {
				changed = true
			}
		} else if len(filtered) != len(calls) {
			message[toolCallsKey] = filtered
			changed = true
		}
	}

	filteredMessages := make([]any, 0, len(messages))
	for _, item := range messages {
		message, ok := item.(map[string]any)
		if ok && message["role"] == "tool" {
			id, hasID := nonEmptyString(message["tool_call_id"])
			if !hasID {
				changed = true
				continue
			}
			if _, exists := validCallIDs[id]; !exists {
				changed = true
				continue
			}
		}
		filteredMessages = append(filteredMessages, item)
	}
	if len(filteredMessages) != len(messages) {
		root["messages"] = filteredMessages
	}
	if !changed {
		return body, nil
	}
	return marshalJSONNoEscape(payload)
}

func validSenseNovaToolCall(call map[string]any) bool {
	if _, ok := nonEmptyString(call["id"]); !ok {
		return false
	}
	function, ok := call["function"].(map[string]any)
	if !ok {
		return false
	}
	if _, ok := nonEmptyString(function["name"]); !ok {
		return false
	}
	if _, ok := nonEmptyString(function["arguments"]); !ok {
		return false
	}
	return true
}

func nonEmptyString(value any) (string, bool) {
	text, ok := value.(string)
	if !ok {
		return "", false
	}
	text = strings.TrimSpace(text)
	return text, text != ""
}

// senseNovaResponseRewrites are the two ways a SenseNova answer departs from
// the Chat Completions the client speaks. Its history decoder names the
// tool-call variant `function_call`, and it serialises an unset finish_reason
// as `""` where the protocol says `null`.
var senseNovaResponseRewrites = []jsonMemberRewrite{
	newJSONMemberRewrite(typeKey, "function_call", []byte(`"function"`), true),
	newJSONMemberRewrite("finish_reason", "", []byte("null"), false),
}

// transformSenseNovaResponseBody normalises a complete SenseNova answer.
func transformSenseNovaResponseBody(body []byte) []byte {
	// Both rewrites need the member they edit to be in the body at all, and
	// the audit-captured body can be large; the scan is what keeps a reply
	// with nothing to normalise from being walked container by container.
	if !bytes.Contains(body, []byte("function_call")) && !bytes.Contains(body, []byte("finish_reason")) {
		return body
	}
	rewritten, _ := applyJSONMemberRewrites(body, senseNovaResponseRewrites...)
	return rewritten
}

// transformSenseNovaPayload normalises one streamed chunk. Both passes edit the
// same payload, so they run in sequence here and the line framing happens
// once, in the caller.
func transformSenseNovaPayload(payload []byte) []byte {
	return stripEmptySenseNovaToolCallDeltaFields(transformSenseNovaResponseBody(payload))
}

// stripEmptySenseNovaToolCallDeltaFields removes the empty id and function.name
// fields SenseNova repeats on tool-call continuation chunks. A continuation
// carries only an argument fragment, so the identity it echoes as "" is not
// merely redundant: SenseNova rejects a tool call whose function has an empty
// name, which is a working stream turning into an error at the second chunk.
func stripEmptySenseNovaToolCallDeltaFields(body []byte) []byte {
	// A payload can only hold one of the fields this deletes if it carries a
	// tool_calls member and an empty string, so two scans are the entire
	// pre-filter — and the parse below is worth skipping, because a stream
	// calls this on every tool-call chunk. The scans over-approximate: an
	// empty `arguments` or a `""` in some other field costs one walk that
	// changes nothing, while missing a real one would forward the payload the
	// client is about to reject.
	if !bytes.Contains(body, toolCallsKeyToken) || !bytes.Contains(body, emptyStringToken) {
		return body
	}
	stripped, changed := stripEmptyToolCallFields(json.RawMessage(body))
	if !changed {
		return body
	}
	return stripped
}

// stripEmptyToolCallFields walks one JSON value, rebuilding only the containers
// where a field actually went away. Decoding into map[string]json.RawMessage
// rather than any is the point: an untouched member keeps the bytes it arrived
// in, so editing a leaf re-serialises the objects above it and nothing beside
// it.
func stripEmptyToolCallFields(raw json.RawMessage) (json.RawMessage, bool) {
	switch firstNonSpace(raw) {
	case '[':
		var items []json.RawMessage
		if json.Unmarshal(raw, &items) != nil {
			return raw, false
		}
		changed := false
		for i, item := range items {
			if stripped, itemChanged := stripEmptyToolCallFields(item); itemChanged {
				items[i] = stripped
				changed = true
			}
		}
		if !changed {
			return raw, false
		}
		return rebuild(items, raw), true
	case '{':
		var members map[string]json.RawMessage
		if json.Unmarshal(raw, &members) != nil {
			return raw, false
		}
		changed := false
		if calls, ok := members[toolCallsKey]; ok {
			if cleaned, callsChanged := cleanToolCallEntries(calls); callsChanged {
				members[toolCallsKey] = cleaned
				changed = true
			}
		}
		for key, member := range members {
			if stripped, memberChanged := stripEmptyToolCallFields(member); memberChanged {
				members[key] = stripped
				changed = true
			}
		}
		if !changed {
			return raw, false
		}
		return rebuild(members, raw), true
	}
	return raw, false
}

// cleanToolCallEntries drops the echoed identity fields from each entry of a
// tool_calls array.
func cleanToolCallEntries(raw json.RawMessage) (json.RawMessage, bool) {
	var entries []json.RawMessage
	if json.Unmarshal(raw, &entries) != nil {
		return raw, false
	}
	changed := false
	for i, entry := range entries {
		var call map[string]json.RawMessage
		if json.Unmarshal(entry, &call) != nil {
			continue
		}
		entryChanged := deleteEmptyStringMember(call, "id")
		if function, ok := call["function"]; ok {
			var fields map[string]json.RawMessage
			if json.Unmarshal(function, &fields) == nil && deleteEmptyStringMember(fields, "name") {
				call["function"] = rebuild(fields, function)
				entryChanged = true
			}
		}
		if entryChanged {
			entries[i] = rebuild(call, entry)
			changed = true
		}
	}
	if !changed {
		return raw, false
	}
	return rebuild(entries, raw), true
}

// deleteEmptyStringMember removes key from members when its value is the JSON
// string "". The value has already been parsed by the time this runs, so a
// serialization that puts a space after the colon is handled for free.
func deleteEmptyStringMember(members map[string]json.RawMessage, key string) bool {
	var text string
	raw, ok := members[key]
	if !ok || json.Unmarshal(raw, &text) != nil || text != "" {
		return false
	}
	delete(members, key)
	return true
}

// rebuild re-serialises a value the walk changed, falling back to the original
// bytes if that fails so a normalisation can never answer with nothing.
func rebuild(value any, fallback json.RawMessage) json.RawMessage {
	out, err := marshalJSONNoEscape(value)
	if err != nil {
		return fallback
	}
	return out
}

// skipJSONWhitespace returns the index of the first byte at or after start
// that is not JSON whitespace.
func skipJSONWhitespace(body []byte, start int) int {
	for start < len(body) {
		if !isJSONWhitespace(body[start]) {
			return start
		}
		start++
	}
	return start
}

// isJSONWhitespace reports whether b is one of the four bytes JSON allows
// between tokens.
func isJSONWhitespace(value byte) bool {
	return value == ' ' || value == '\t' || value == '\r' || value == '\n'
}

// firstNonSpace reports the first byte of a JSON value that is not whitespace,
// or 0 when it has none.
func firstNonSpace(raw json.RawMessage) byte {
	for _, b := range raw {
		if !isJSONWhitespace(b) {
			return b
		}
	}
	return 0
}
