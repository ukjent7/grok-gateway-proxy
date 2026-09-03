package proxy

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

		return doc, false
	}
	if rewriter.failed || len(rewriter.edits) == 0 {
		return doc, false
	}
	edits := rewriter.edits

	out := make([]byte, 0, len(doc))
	var copied int
	for _, edit := range edits {
		out = append(out, doc[copied:edit.start]...)
		out = append(out, edit.to...)
		copied = edit.end
	}
	return append(out, doc[copied:]...), true
}

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
	if _, err := r.decoder.Token(); err != nil {
		r.failed = true
	}
}

func (r *jsonRewriter) array(inToolCalls bool) {
	cursor := int(r.decoder.InputOffset())
	for !r.failed && r.decoder.More() {
		cursor = r.value("", cursor, inToolCalls)
	}
	if _, err := r.decoder.Token(); err != nil {
		r.failed = true
	}
}

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

func nextJSONToken(src []byte, from int) int {
	at := skipJSONWhitespace(src, from)
	if at < len(src) && (src[at] == ':' || src[at] == ',') {
		at = skipJSONWhitespace(src, at+1)
	}
	return at
}

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

func marshalJSONNoEscape(value any) ([]byte, error) {
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}

	return bytes.TrimRight(out.Bytes(), "\n"), nil
}

func decodeJSONDocument(body []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return value, nil
}

func transformToolCallType(body []byte, from, to string) []byte {
	rewritten, _ := applyJSONMemberRewrites(body, newJSONMemberRewrite(typeKey, from, []byte(strconv.Quote(to)), true))
	return rewritten
}

func sanitizeSenseNovaToolCallHistory(body []byte) ([]byte, error) {

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

var senseNovaResponseRewrites = []jsonMemberRewrite{
	newJSONMemberRewrite(typeKey, "function_call", []byte(`"function"`), true),
	newJSONMemberRewrite("finish_reason", "", []byte("null"), false),
}

func transformSenseNovaResponseBody(body []byte) []byte {

	if !bytes.Contains(body, []byte("function_call")) && !bytes.Contains(body, []byte("finish_reason")) {
		return body
	}
	rewritten, _ := applyJSONMemberRewrites(body, senseNovaResponseRewrites...)
	return rewritten
}

func transformSenseNovaPayload(payload []byte) []byte {
	return stripEmptySenseNovaToolCallDeltaFields(transformSenseNovaResponseBody(payload))
}

func stripEmptySenseNovaToolCallDeltaFields(body []byte) []byte {

	if !bytes.Contains(body, toolCallsKeyToken) || !bytes.Contains(body, emptyStringToken) {
		return body
	}
	stripped, changed := stripEmptyToolCallFields(json.RawMessage(body))
	if !changed {
		return body
	}
	return stripped
}

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

func deleteEmptyStringMember(members map[string]json.RawMessage, key string) bool {
	var text string
	raw, ok := members[key]
	if !ok || json.Unmarshal(raw, &text) != nil || text != "" {
		return false
	}
	delete(members, key)
	return true
}

func rebuild(value any, fallback json.RawMessage) json.RawMessage {
	out, err := marshalJSONNoEscape(value)
	if err != nil {
		return fallback
	}
	return out
}

// insertTopLevelJSONMember 在 JSON 顶层对象的末尾追加一个成员，**不改动其余任何字节**。
//
// 与"反序列化进 map 再序列化"的区别至关重要：Go 序列化 map 时会按字母序重排所有键，
// 导致上游收到的正文字节序与客户端发出的完全不同。本函数只在 '}' 之前插入，
// 因此键序、空白、数字字面量、字符串转义全部原样保留。
//
// key 已存在时不做任何改动（返回 changed=false），保证绝不重写现有值。
// 输入不是合法的 JSON 对象时同样原样返回，交给调用方决定如何处理。
func insertTopLevelJSONMember(doc []byte, key string, value json.RawMessage) ([]byte, bool) {
	trimmed := bytes.TrimSpace(doc)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return doc, false
	}
	end, ok, hasMembers := topLevelObjectEnd(doc)
	if !ok {
		return doc, false
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(doc, &probe); err != nil {
		return doc, false
	}
	if _, exists := probe[key]; exists {
		return doc, false
	}
	insertAt := end
	for insertAt > 0 && isJSONWhitespace(doc[insertAt-1]) {
		insertAt--
	}
	out := make([]byte, 0, len(doc)+len(key)+len(value)+4)
	out = append(out, doc[:insertAt]...)
	if hasMembers {
		out = append(out, ',')
	}
	out = append(out, '"')
	out = append(out, key...)
	out = append(out, '"', ':')
	out = append(out, value...)
	out = append(out, doc[insertAt:]...)
	return out, true
}

// topLevelObjectEnd 返回顶层对象右花括号的下标、是否找到、以及对象体内是否已有成员。
// 扫描时跟踪字符串与转义，因此字符串内部的 '{' / '}' 不会误判。
func topLevelObjectEnd(doc []byte) (end int, ok bool, hasMembers bool) {
	start := -1
	for i, b := range doc {
		if isJSONWhitespace(b) {
			continue
		}
		if b == '{' {
			start = i
		}
		break
	}
	if start < 0 {
		return 0, false, false
	}
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(doc); i++ {
		c := doc[i]
		if inString {
			if escaped {
				escaped = false
				continue
			}
			switch c {
			case '\\':
				escaped = true
			case '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i, true, len(bytes.TrimSpace(doc[start+1:i])) > 0
			}
		}
	}
	return 0, false, false
}

// topLevelMemberSpan 描述顶层对象中一个成员的字节位置。
type topLevelMemberSpan struct {
	key        string
	keyStart   int // 键的起始 '"' 位置
	valueStart int // 值的首个非空白字节
	valueEnd   int // 值的最后一个字节之后
}

// topLevelMemberSpans 枚举顶层对象的成员及字节位置。
// 只扫描第一层；嵌套结构由 scanJSONValue 整体跳过（含字符串与转义）。
func topLevelMemberSpans(doc []byte) ([]topLevelMemberSpan, bool) {
	start := -1
	for i, b := range doc {
		if isJSONWhitespace(b) {
			continue
		}
		if b == '{' {
			start = i
		}
		break
	}
	if start < 0 {
		return nil, false
	}
	i := start + 1
	var members []topLevelMemberSpan
	for {
		for i < len(doc) && isJSONWhitespace(doc[i]) {
			i++
		}
		if i >= len(doc) {
			return nil, false
		}
		if doc[i] == '}' {
			return members, true
		}
		if doc[i] != '"' {
			return nil, false
		}
		keyStart := i
		end, ok := scanJSONString(doc, i)
		if !ok {
			return nil, false
		}
		var key string
		if err := json.Unmarshal(doc[keyStart:end], &key); err != nil {
			return nil, false
		}
		i = end
		for i < len(doc) && isJSONWhitespace(doc[i]) {
			i++
		}
		if i >= len(doc) || doc[i] != ':' {
			return nil, false
		}
		i++
		for i < len(doc) && isJSONWhitespace(doc[i]) {
			i++
		}
		valueStart := i
		valueEnd, ok := scanJSONValue(doc, i)
		if !ok {
			return nil, false
		}
		members = append(members, topLevelMemberSpan{key: key, keyStart: keyStart, valueStart: valueStart, valueEnd: valueEnd})
		i = valueEnd
		for i < len(doc) && isJSONWhitespace(doc[i]) {
			i++
		}
		if i < len(doc) && doc[i] == ',' {
			i++
			continue
		}
		// '}' 或非法输入交给下一轮循环判定
	}
}

func scanJSONString(doc []byte, i int) (int, bool) {
	if i >= len(doc) || doc[i] != '"' {
		return 0, false
	}
	i++
	for i < len(doc) {
		switch doc[i] {
		case '\\':
			i += 2
		case '"':
			return i + 1, true
		default:
			i++
		}
	}
	return 0, false
}

// scanJSONValue 返回从 i 开始的任意 JSON 值的结束位置（不含）。
func scanJSONValue(doc []byte, i int) (int, bool) {
	for i < len(doc) && isJSONWhitespace(doc[i]) {
		i++
	}
	if i >= len(doc) {
		return 0, false
	}
	switch doc[i] {
	case '"':
		return scanJSONString(doc, i)
	case '{', '[':
		open, closeCh := doc[i], byte('}')
		if doc[i] == '[' {
			closeCh = ']'
		}
		depth := 0
		inString, escaped := false, false
		for ; i < len(doc); i++ {
			c := doc[i]
			if inString {
				if escaped {
					escaped = false
				} else if c == '\\' {
					escaped = true
				} else if c == '"' {
					inString = false
				}
				continue
			}
			switch c {
			case '"':
				inString = true
			case open:
				depth++
			case closeCh:
				depth--
				if depth == 0 {
					return i + 1, true
				}
			}
		}
		return 0, false
	default:
		// 标量：数字 / true / false / null
		for i < len(doc) {
			c := doc[i]
			if c == ',' || c == '}' || c == ']' || isJSONWhitespace(c) {
				return i, true
			}
			i++
		}
		return i, true
	}
}

// topLevelMemberEdit 描述对顶层成员的一次修改。
type topLevelMemberEdit struct {
	key    string
	delete bool
	value  json.RawMessage // delete=false 时生效
}

// applyTopLevelMemberEdits 以成员级重拼的方式修改顶层对象：
// 每个成员的原始字节（键、冒号、空白）原样保留，被删除的成员整段跳过，
// 成员之间的原始分隔符（",\n  " 这类）也原样保留。因此未被修改的成员
// 在输出中与输入逐字节相同，且不会产生悬空逗号。
// 成员不存在时跳过（本函数只修改已存在的成员，不新增）。
func applyTopLevelMemberEdits(doc []byte, edits []topLevelMemberEdit) ([]byte, bool) {
	if len(edits) == 0 {
		return doc, false
	}
	members, ok := topLevelMemberSpans(doc)
	if !ok || len(members) == 0 {
		return doc, false
	}
	end, okEnd, _ := topLevelObjectEnd(doc)
	if !okEnd {
		return doc, false
	}
	editsByKey := make(map[string]topLevelMemberEdit, len(edits))
	for _, e := range edits {
		editsByKey[e.key] = e
	}
	changed := false
	for _, m := range members {
		if _, hit := editsByKey[m.key]; hit {
			changed = true
			break
		}
	}
	if !changed {
		return doc, false
	}

	out := make([]byte, 0, len(doc))
	// 对象开头（"{" 及其后的空白）保留到第一个成员之前。
	out = append(out, doc[:members[0].keyStart]...)
	prevKept := -1
	for i, m := range members {
		e, hasEdit := editsByKey[m.key]
		if hasEdit && e.delete {
			continue
		}
		if prevKept >= 0 {
			// 前一个保留成员的原始分隔符（",\n  " 等）。注意不能直接取
			// doc[prev.valueEnd : m.keyStart]——中间若有被删成员，其字节会混进分隔符；
			// 因此只取到原始逗号及其后的空白为止。
			sepStart := members[prevKept].valueEnd
			sepEnd := sepStart
			for sepEnd < len(doc) && isJSONWhitespace(doc[sepEnd]) {
				sepEnd++
			}
			if sepEnd < len(doc) && doc[sepEnd] == ',' {
				sepEnd++
				for sepEnd < len(doc) && isJSONWhitespace(doc[sepEnd]) {
					sepEnd++
				}
				out = append(out, doc[sepStart:sepEnd]...)
			} else {
				out = append(out, ',')
			}
		}
		if hasEdit {
			// 键与冒号原样，仅替换值。
			out = append(out, doc[m.keyStart:m.valueStart]...)
			out = append(out, e.value...)
		} else {
			out = append(out, doc[m.keyStart:m.valueEnd]...)
		}
		prevKept = i
	}
	// 最后一个成员之后到 '}' 之间的原始字节（通常为空或换行缩进），再加 '}' 及其后缀。
	out = append(out, doc[members[len(members)-1].valueEnd:end]...)
	out = append(out, doc[end:]...)
	return out, true
}

func skipJSONWhitespace(body []byte, start int) int {
	for start < len(body) {
		if !isJSONWhitespace(body[start]) {
			return start
		}
		start++
	}
	return start
}

func isJSONWhitespace(value byte) bool {
	return value == ' ' || value == '\t' || value == '\r' || value == '\n'
}

func firstNonSpace(raw json.RawMessage) byte {
	for _, b := range raw {
		if !isJSONWhitespace(b) {
			return b
		}
	}
	return 0
}
