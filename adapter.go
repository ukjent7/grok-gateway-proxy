package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// GatewayAdapter describes one upstream's native protocol. Adapters deliberately
// do not translate Responses and Chat Completions; they validate the fixed
// endpoint and preserve the provider payload, including reasoning and tool data.
type GatewayAdapter interface {
	ID() string
	Protocol() Protocol
	EndpointPath() string
	AcceptsPath(path string) bool
	RejectMessage(path string) string
	ValidateRequest(body []byte) error
	NormalizeError(status int, body []byte) []byte
}

type OpenCodeResponsesAdapter struct{}

func (OpenCodeResponsesAdapter) ID() string           { return "OpenCodeResponsesAdapter" }
func (OpenCodeResponsesAdapter) Protocol() Protocol   { return ProtocolResponses }
func (OpenCodeResponsesAdapter) EndpointPath() string { return "/responses" }
func (a OpenCodeResponsesAdapter) AcceptsPath(path string) bool {
	return path == a.EndpointPath()
}
func (a OpenCodeResponsesAdapter) RejectMessage(path string) string {
	return fmt.Sprintf("%s accepts only %s, got %s", a.ID(), a.EndpointPath(), path)
}
func (OpenCodeResponsesAdapter) ValidateRequest(body []byte) error {
	return validateJSONRequest(body, "OpenCode Responses")
}
func (OpenCodeResponsesAdapter) NormalizeError(status int, body []byte) []byte {
	return normalizeUpstreamError(status, body)
}

type SenseNovaChatAdapter struct{}

func (SenseNovaChatAdapter) ID() string           { return "SenseNovaChatAdapter" }
func (SenseNovaChatAdapter) Protocol() Protocol   { return ProtocolChat }
func (SenseNovaChatAdapter) EndpointPath() string { return "/chat/completions" }
func (a SenseNovaChatAdapter) AcceptsPath(path string) bool {
	return path == a.EndpointPath()
}
func (a SenseNovaChatAdapter) RejectMessage(path string) string {
	return fmt.Sprintf("%s accepts only %s, got %s", a.ID(), a.EndpointPath(), path)
}
func (SenseNovaChatAdapter) ValidateRequest(body []byte) error {
	return validateJSONRequest(body, "SenseNova Chat Completions")
}
func (SenseNovaChatAdapter) NormalizeError(status int, body []byte) []byte {
	return normalizeUpstreamError(status, body)
}

// SenseNova's tool-call history decoder uses function_call for the tool call
// variant, while the client-facing Chat Completions format uses function.
// Keep the conversion scoped to tool_calls so tools[].type remains function.
func (SenseNovaChatAdapter) TransformRequestBody(body []byte) ([]byte, error) {
	transformed, err := transformToolCallType(body, "function", "function_call")
	if err != nil {
		return nil, err
	}
	return sanitizeSenseNovaToolCallHistory(transformed)
}

func (SenseNovaChatAdapter) TransformResponseBody(body []byte) ([]byte, error) {
	return transformSenseNovaResponseBody(body)
}

func (SenseNovaChatAdapter) TransformSSE(reader io.Reader) io.Reader {
	return &senseNovaSSEReader{reader: bufio.NewReaderSize(reader, 64*1024)}
}

type payloadTransformer interface {
	TransformRequestBody([]byte) ([]byte, error)
	TransformResponseBody([]byte) ([]byte, error)
}

type streamTransformer interface {
	TransformSSE(io.Reader) io.Reader
}

func transformRequestBody(adapter GatewayAdapter, body []byte) ([]byte, error) {
	if transformer, ok := adapter.(payloadTransformer); ok {
		return transformer.TransformRequestBody(body)
	}
	return body, nil
}

func transformResponseBody(adapter GatewayAdapter, body []byte) ([]byte, error) {
	if transformer, ok := adapter.(payloadTransformer); ok {
		return transformer.TransformResponseBody(body)
	}
	return body, nil
}

func transformToolCallType(body []byte, from, to string) ([]byte, error) {
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
	// Only rewrite the tool-call type value inside tool_calls arrays, matching
	// the request-side conversion; tools[].type and any other "type" property
	// stay untouched. finish_reason "" is normalized to null because strict
	// clients reject the empty string on the terminal choice.
	result, changed := replaceJSONPropertyStringValueInToolCalls(body, "function_call", `"function"`)
	var finishChanged bool
	result, finishChanged = replaceJSONPropertyStringValue(result, "finish_reason", "", "null")
	changed = changed || finishChanged
	if !changed {
		return body, nil
	}
	return result, nil
}

// replaceJSONPropertyStringValueInToolCalls changes a "type" property string
// value only when the property belongs to an entry of a "tool_calls" array.
// Everything else — key order, whitespace, and any other "type" field such as
// tools[].type — is left byte-for-byte intact.
func replaceJSONPropertyStringValueInToolCalls(body []byte, from, to string) ([]byte, bool) {
	typeToken := []byte(`"type"`)
	toolCallsKeyToken := []byte(`"tool_calls"`)
	fromToken := []byte(`"` + from + `"`)
	toToken := []byte(to)
	changed := false
	// bracketDepth tracks the nesting of {} and [] containers.
	bracketDepth := 0
	// toolCallDepths holds the bracketDepth at which each currently open
	// tool_calls array started; empty while not inside any.
	var toolCallDepths []int
	// lastKey is the most recently seen object key at the current depth.
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
				index = index + len(toToken)
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

// SenseNova repeats empty id/name fields on tool-call continuation chunks.
// Some OpenAI-compatible clients merge those chunks by assignment, so the
// repeated empty fields overwrite the identity from the first chunk and the
// next request contains an invalid empty tool call. Omit only empty identity
// fields inside streamed tool_calls; arguments remain incremental fragments.
func stripEmptySenseNovaToolCallDeltaFields(body []byte) ([]byte, error) {
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

type VercelResponsesAdapter struct{}

func (VercelResponsesAdapter) ID() string           { return "VercelResponsesAdapter" }
func (VercelResponsesAdapter) Protocol() Protocol   { return ProtocolResponses }
func (VercelResponsesAdapter) EndpointPath() string { return "/responses" }
func (a VercelResponsesAdapter) AcceptsPath(path string) bool {
	return path == a.EndpointPath()
}
func (a VercelResponsesAdapter) RejectMessage(path string) string {
	return fmt.Sprintf("%s accepts only %s, got %s", a.ID(), a.EndpointPath(), path)
}
func (VercelResponsesAdapter) ValidateRequest(body []byte) error {
	return validateJSONRequest(body, "Vercel Responses")
}
func (VercelResponsesAdapter) NormalizeError(status int, body []byte) []byte {
	return normalizeUpstreamError(status, body)
}

// Vercel's AI Gateway adapts third-party providers to the Responses API, and
// two of its behaviors break strict clients (e.g. Grok Build's async-openai
// based parser): it injects keepalive `ping` events (data: {"type":"ping"})
// and, for reasoning models, emits the legacy event names
// `response.reasoning.delta` / `response.reasoning.done` instead of the
// `response.reasoning_text.delta` / `response.reasoning_text.done` variants
// the client's enum knows. Drop the pings and rename the legacy reasoning
// events (their payloads are field-for-field identical); everything else is
// passed through untouched.
func (VercelResponsesAdapter) TransformSSE(reader io.Reader) io.Reader {
	return &vercelSSEReader{reader: bufio.NewReaderSize(reader, 64*1024)}
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

func validateJSONRequest(body []byte, protocolName string) error {
	if len(strings.TrimSpace(string(body))) == 0 {
		return fmt.Errorf("%s request body cannot be empty", protocolName)
	}
	if !json.Valid(body) {
		return fmt.Errorf("%s request body must be valid JSON", protocolName)
	}
	return nil
}

func normalizeUpstreamError(status int, body []byte) []byte {
	message := fmt.Sprintf("upstream returned HTTP %d", status)
	var upstream struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &upstream) == nil && strings.TrimSpace(upstream.Error.Message) != "" {
		message = upstream.Error.Message
	}
	errorBody := map[string]any{
		"type":    "upstream_error",
		"message": message,
		"code":    strconv.Itoa(status),
	}
	if len(body) > 0 {
		if json.Valid(body) {
			errorBody["details"] = json.RawMessage(body)
		} else {
			errorBody["details"] = string(body)
		}
	}
	result, err := json.Marshal(map[string]any{"error": errorBody})
	if err != nil {
		return []byte(`{"error":{"type":"upstream_error","message":"upstream request failed"}}`)
	}
	return result
}

var gatewayAdapters = map[string]GatewayAdapter{
	"oc": OpenCodeResponsesAdapter{},
	"st": SenseNovaChatAdapter{},
	"ve": VercelResponsesAdapter{},
}

func adapterFor(id string) (GatewayAdapter, bool) {
	adapter, ok := gatewayAdapters[id]
	return adapter, ok
}
