package proxy

import (
	"bytes"
	"encoding/json"
	"strings"
)

// jsonrewrite.go holds the JSON rewrite helpers used by the SenseNova adapter
// to normalize tool-call payloads. The response-side helpers
// (replaceJSONPropertyStringValueInToolCalls, replaceJSONPropertyStringValue)
// operate at the byte level to preserve key order, whitespace, and unrelated
// bytes; the request-side helpers (transformToolCallType,
// sanitizeSenseNovaToolCallHistory) unmarshal and re-marshal the document
// because they need structural edits that byte-level rewriting cannot express.

// transformToolCallType rewrites every "type":"from" inside tool_calls arrays
// to "to", using a JSON-aware walk so unrelated "type" fields are untouched.
func transformToolCallType(body []byte, from, to string) ([]byte, error) {
	// Optimization 6: fast-path check before JSON parse.
	if !bytes.Contains(body, []byte(`"tool_calls"`)) || !bytes.Contains(body, []byte(from)) {
		return body, nil
	}
	var payload any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body, nil
	}
	if !rewriteToolCallTypes(payload, from, to) {
		return body, nil
	}
	result, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return result, nil
}

// sanitizeSenseNovaToolCallHistory removes incomplete tool-call records from
// the request history. A partially emitted client-side tool call has no
// function name to execute, so forwarding it makes SenseNova reject the
// entire request with "function/name/arguments cannot be empty". Its matching
// tool result is removed as well because it would otherwise be an orphan.
func sanitizeSenseNovaToolCallHistory(body []byte) ([]byte, error) {
	// Fast path: body without tool_calls or messages cannot need cleaning.
	if !bytes.Contains(body, []byte(`"tool_calls"`)) && !bytes.Contains(body, []byte(`"messages"`)) {
		return body, nil
	}
	var payload any
	if err := json.Unmarshal(body, &payload); err != nil {
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
		calls, ok := message["tool_calls"].([]any)
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
			delete(message, "tool_calls")
			if len(calls) > 0 {
				changed = true
			}
		} else if len(filtered) != len(calls) {
			message["tool_calls"] = filtered
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

	result, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return result, nil
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

func transformSenseNovaResponseBody(body []byte) ([]byte, error) {
	if !json.Valid(body) {
		return body, nil
	}
	// Fast path: only scan if relevant markers present.
	hasToolCall := bytes.Contains(body, []byte("function_call"))
	hasFinish := bytes.Contains(body, []byte("finish_reason"))
	if !hasToolCall && !hasFinish {
		return body, nil
	}
	// Preserve key order/whitespace for streaming tests: use byte-level
	// rewrites. The structured path (rewriteToolCallTypes) would randomize
	// map iteration order and break byte-identical expectations.
	result := body
	changed := false
	if hasToolCall {
		var c bool
		result, c = replaceJSONPropertyStringValueInToolCalls(result, "function_call", `"function"`)
		changed = changed || c
	}
	if hasFinish {
		var finishChanged bool
		result, finishChanged = replaceJSONPropertyStringValue(result, "finish_reason", "", "null")
		changed = changed || finishChanged
	}
	if !changed {
		return body, nil
	}
	return result, nil
}

// replaceJSONPropertyStringValueInToolCalls changes a "type" property string
// value only when the property belongs to an entry of a "tool_calls" array.
// Everything else — key order, whitespace, and any other "type" field such as
// tools[].type — is left byte-for-byte intact. Kept for SenseNova streaming
// where byte-identical passthrough matters for the proxy's log comparison.
func replaceJSONPropertyStringValueInToolCalls(body []byte, from, to string) ([]byte, bool) {
	typeToken := []byte(`"type"`)
	toolCallsKeyToken := []byte(`"tool_calls"`)
	fromToken := []byte(`"` + from + `"`)
	toToken := []byte(to)
	changed := false
	bracketDepth := 0
	var toolCallDepths []int
	lastKeyDepth := -1
	lastKey := []byte(nil)
	inToolCalls := func() bool {
		return len(toolCallDepths) > 0 && bracketDepth > toolCallDepths[len(toolCallDepths)-1]
	}
	for index := 0; index < len(body); {
		switch body[index] {
		case '{', '[':
			lastKeyDepth = -1
			lastKey = nil
			bracketDepth++
			index++
		case '}', ']':
			bracketDepth--
			for len(toolCallDepths) > 0 && bracketDepth <= toolCallDepths[len(toolCallDepths)-1] {
				toolCallDepths = toolCallDepths[:len(toolCallDepths)-1]
			}
			index++
		case '"':
			end, ok := scanJSONString(body, index)
			if !ok {
				return body, changed
			}
			valueStart := skipJSONWhitespace(body, end)
			isKey := valueStart < len(body) && body[valueStart] == ':'
			if isKey {
				lastKeyDepth = bracketDepth
				lastKey = body[index:end]
				if bytes.Equal(body[index:end], toolCallsKeyToken) {
					afterColon := skipJSONWhitespace(body, valueStart+1)
					if afterColon < len(body) && body[afterColon] == '[' {
						toolCallDepths = append(toolCallDepths, bracketDepth)
					}
				}
				index = end
				continue
			}
			if inToolCalls() && lastKeyDepth == bracketDepth && bytes.Equal(lastKey, typeToken) &&
				bytes.Equal(body[index:end], fromToken) {
				result := make([]byte, 0, len(body)-len(fromToken)+len(toToken))
				result = append(result, body[:index]...)
				result = append(result, toToken...)
				result = append(result, body[end:]...)
				body = result
				changed = true
				index += len(toToken)
				continue
			}
			index = end
		default:
			index++
		}
	}
	return body, changed
}

// replaceJSONPropertyStringValue changes only a JSON property value. It keeps
// the original key order, whitespace, and all unrelated bytes intact.
// Kept for SSE legacy event renaming where preserving original serialization
// matters; all other rewrites use structured JSON.
func replaceJSONPropertyStringValue(body []byte, key, from, to string) ([]byte, bool) {
	keyToken := []byte(`"` + key + `"`)
	fromToken := []byte(`"` + from + `"`)
	toToken := []byte(to)
	changed := false
	for index := 0; index < len(body); {
		if body[index] != '"' {
			index++
			continue
		}
		end, ok := scanJSONString(body, index)
		if !ok {
			return body, changed
		}
		if bytes.Equal(body[index:end], keyToken) {
			valueStart := skipJSONWhitespace(body, end)
			if valueStart < len(body) && body[valueStart] == ':' {
				valueStart = skipJSONWhitespace(body, valueStart+1)
				if valueEnd, valueOK := scanJSONString(body, valueStart); valueOK && bytes.Equal(body[valueStart:valueEnd], fromToken) {
					result := make([]byte, 0, len(body)-len(fromToken)+len(toToken))
					result = append(result, body[:valueStart]...)
					result = append(result, toToken...)
					result = append(result, body[valueEnd:]...)
					body = result
					index = valueStart + len(toToken)
					changed = true
					continue
				}
			}
		}
		index = end
	}
	return body, changed
}

func scanJSONString(body []byte, start int) (int, bool) {
	if start >= len(body) || body[start] != '"' {
		return 0, false
	}
	for index := start + 1; index < len(body); index++ {
		switch body[index] {
		case '\\':
			index++
		case '"':
			return index + 1, true
		}
	}
	return 0, false
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

func rewriteToolCallTypes(value any, from, to string) bool {
	changed := false
	switch current := value.(type) {
	case []any:
		for _, item := range current {
			changed = rewriteToolCallTypes(item, from, to) || changed
		}
	case map[string]any:
		for key, child := range current {
			if key == "tool_calls" {
				if calls, ok := child.([]any); ok {
					for _, call := range calls {
						if callObject, ok := call.(map[string]any); ok {
							if callType, ok := callObject["type"].(string); ok && callType == from {
								callObject["type"] = to
								changed = true
							}
						}
						changed = rewriteToolCallTypes(call, from, to) || changed
					}
				}
				continue
			}
			changed = rewriteToolCallTypes(child, from, to) || changed
		}
	}
	return changed
}

// stripEmptySenseNovaToolCallDeltaFields removes empty id/name fields that
// SenseNova repeats on tool-call continuation chunks.
func stripEmptySenseNovaToolCallDeltaFields(body []byte) ([]byte, error) {
	// Fast path: only parse if empty fields likely present.
	if !bytes.Contains(body, []byte(`"id":""`)) && !bytes.Contains(body, []byte(`"name":""`)) {
		return body, nil
	}
	var payload any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body, nil
	}
	if !stripEmptySenseNovaToolCallDeltaFieldsInValue(payload) {
		return body, nil
	}
	result, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return result, nil
}

func stripEmptySenseNovaToolCallDeltaFieldsInValue(value any) bool {
	changed := false
	switch current := value.(type) {
	case []any:
		for _, item := range current {
			changed = stripEmptySenseNovaToolCallDeltaFieldsInValue(item) || changed
		}
	case map[string]any:
		if calls, ok := current["tool_calls"].([]any); ok {
			for _, item := range calls {
				call, ok := item.(map[string]any)
				if !ok {
					continue
				}
				if id, ok := call["id"].(string); ok && id == "" {
					delete(call, "id")
					changed = true
				}
				if function, ok := call["function"].(map[string]any); ok {
					if name, ok := function["name"].(string); ok && name == "" {
						delete(function, "name")
						changed = true
					}
				}
			}
		}
		for _, child := range current {
			changed = stripEmptySenseNovaToolCallDeltaFieldsInValue(child) || changed
		}
	}
	return changed
}
