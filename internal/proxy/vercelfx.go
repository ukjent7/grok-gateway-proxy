package proxy

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"grok-gateway-proxy/internal/store"
)

// vercelfx.go implements the Vercel FX disguise mode for the ve gateway. When
// enabled, upstream requests are rewritten into the official fx client's v3
// language-model protocol so the request hits Vercel AI Gateway's free
// promotional pool:
//
//   - HTTP headers impersonate the fx CLI (fx/ UA, HTTP-Referer, X-Title).
//   - The request body is converted from the Responses protocol to the v3
//     language-model format with a `headers: {user-agent, x-title}` object
//     injected (the promo trigger).
//   - The v3 SSE response (text-delta / reasoning-delta / tool-input-* /
//     finish) is converted back to the Responses protocol SSE the client
//     expects, and non-streaming responses are assembled into a Responses
//     JSON object.
const (
	vercelFXUpstreamPath = "/v3/ai/language-model"
	vercelFXReferer      = "https://github.com/vercel-labs/fx"
	vercelFXTitle        = "fx"
	vercelFXMaxOutput    = 128000
)

// vercelFXUpstreamURL derives the v3 language-model endpoint from the
// gateway's configured base URL (same scheme+host, fx pathway path).
func vercelFXUpstreamURL(baseURL string) string {
	parsed, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "https://ai-gateway.vercel.sh" + vercelFXUpstreamPath
	}
	return parsed.Scheme + "://" + parsed.Host + vercelFXUpstreamPath
}

// fxHex returns a random lowercase hex string of the given byte length.
func fxHex(bytesLen int) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])[:bytesLen*2]
}

// fxDisguiseHeaders returns the HTTP headers that make a request look like it
// came from the official fx CLI.
func fxDisguiseHeaders(userAgent, model, sessionID string) map[string]string {
	return map[string]string{
		"User-Agent":                  userAgent,
		"HTTP-Referer":                vercelFXReferer,
		"X-Title":                     vercelFXTitle,
		"ai-gateway-protocol-version": "0.0.1",
		"ai-language-model-specification-version": "4",
		"ai-language-model-id":                    model,
		"ai-language-model-streaming":             "true",
		"x-session-id":                            sessionID,
		"x-session-affinity":                      sessionID,
	}
}

// --- Request: Responses protocol -> v3 language-model body ---

// convertResponsesToV3 converts a Responses API request body into the v3
// language-model payload, injecting the promo headers object. userAgent is
// the disguised fx UA (must match the HTTP User-Agent header).
func convertResponsesToV3(body []byte, userAgent string) ([]byte, error) {
	if userAgent == "" {
		userAgent = "fx/0.0.3"
	}
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, err
	}
	// model is passed via the ai-language-model-id header in fxDisguiseHeaders,
	// not in the v3 request body.
	_ = asString(req["model"])
	prompt := responsesInputToV3Prompt(req["input"], req["instructions"])

	v3 := map[string]any{
		"prompt": prompt,
		"headers": map[string]any{
			"user-agent": userAgent,
			"x-title":    vercelFXTitle,
		},
	}
	if maxOut := asInt64(req["max_output_tokens"]); maxOut > 0 {
		v3["maxOutputTokens"] = maxOut
	} else {
		v3["maxOutputTokens"] = vercelFXMaxOutput
	}
	if temperature, ok := req["temperature"]; ok {
		if f, ok := asFloat(temperature); ok {
			v3["temperature"] = f
		}
	}
	if topP, ok := req["top_p"]; ok {
		if f, ok := asFloat(topP); ok {
			v3["topP"] = f
		}
	}
	if topK, ok := req["top_k"]; ok {
		if f, ok := asFloat(topK); ok {
			v3["topK"] = f
		}
	}
	if stop := req["stop"]; stop != nil {
		switch s := stop.(type) {
		case string:
			v3["stopSequences"] = []string{s}
		case []any:
			seq := make([]string, 0, len(s))
			for _, item := range s {
				seq = append(seq, asString(item))
			}
			v3["stopSequences"] = seq
		}
	}
	if tools := responsesToolsToV3(req["tools"]); len(tools) > 0 {
		v3["tools"] = tools
		v3["toolChoice"] = responsesToolChoiceToV3(req["tool_choice"])
	}
	if reasoning := responsesReasoningToV3(req["reasoning"]); reasoning != "" {
		v3["reasoning"] = reasoning
	}
	if text, ok := req["text"].(map[string]any); ok {
		if format, ok := text["format"].(map[string]any); ok {
			switch format["type"] {
			case "json_object", "json_schema":
				v3["responseFormat"] = map[string]any{"type": "json"}
			}
		}
	}
	return json.Marshal(v3)
}

// responsesInputToV3Prompt converts Responses `input` (string or item list)
// plus `instructions` into the v3 prompt array.
func responsesInputToV3Prompt(input any, instructions any) []map[string]any {
	prompt := []map[string]any{}
	if sys := asString(instructions); sys != "" {
		prompt = append(prompt, map[string]any{
			"role":    "system",
			"content": sys,
		})
	}
	switch in := input.(type) {
	case string:
		return append(prompt, v3UserPart(in))
	case nil:
		return append(prompt, v3UserPart(" "))
	}
	items, ok := input.([]any)
	if !ok {
		return append(prompt, v3UserPart(" "))
	}

	var pendingUser []map[string]any
	toolNames := map[string]string{}
	flushUser := func() {
		if len(pendingUser) > 0 {
			prompt = append(prompt, map[string]any{"role": "user", "content": pendingUser})
			pendingUser = nil
		}
	}
	for _, item := range items {
		obj, ok := item.(map[string]any)
		if !ok {
			pendingUser = append(pendingUser, v3TextPart(" "))
			continue
		}
		switch typ := asString(obj["type"]); typ {
		case "message", "input_message":
			flushUser()
			role := asString(obj["role"])
			if role == "" {
				role = "user"
			}
			content := responsesContentToV3Parts(obj["content"])
			switch role {
			case "assistant":
				prompt = append(prompt, map[string]any{"role": "assistant", "content": content})
			default:
				prompt = append(prompt, map[string]any{"role": "user", "content": content})
			}
		case "function_call":
			flushUser()
			args := obj["arguments"]
			callID := asString(obj["call_id"])
			if callID == "" {
				callID = asString(obj["id"])
			}
			name := asString(obj["name"])
			if name == "" {
				name = "tool"
			}
			if callID != "" {
				toolNames[callID] = name
			}
			if argsStr, ok := args.(string); ok {
				var parsed any
				if json.Unmarshal([]byte(argsStr), &parsed) == nil {
					args = parsed
				}
			}
			if args == nil {
				args = map[string]any{}
			}
			prompt = append(prompt, map[string]any{
				"role": "assistant",
				"content": []map[string]any{
					{
						"type":       "tool-call",
						"toolCallId": callID,
						"toolName":   name,
						"input":      args,
					},
				},
			})
		case "function_call_output":
			flushUser()
			output := obj["output"]
			callID := asString(obj["call_id"])
			if callID == "" {
				callID = asString(obj["id"])
			}
			toolName := asString(obj["name"])
			if toolName == "" {
				toolName = toolNames[callID]
			}
			if toolName == "" {
				toolName = "tool"
			}
			if output == nil {
				output = ""
			}
			if _, isStr := output.(string); !isStr {
				if b, err := json.Marshal(output); err == nil {
					output = string(b)
				}
			}
			prompt = append(prompt, map[string]any{
				"role": "tool",
				"content": []map[string]any{
					{
						"type":       "tool-result",
						"toolCallId": callID,
						"toolName":   toolName,
						"output":     map[string]any{"type": "text", "value": output},
					},
				},
			})
		case "input_text", "text":
			pendingUser = append(pendingUser, v3TextPart(asString(obj["text"])))
		case "input_image", "image_url":
			pendingUser = append(pendingUser, v3ImagePart(obj))
		case "reasoning":
			// Historical reasoning (Responses `reasoning` items) has no V3 prompt slot.
			// Drop it to match official fx (fx-main) and fx-gateway-proxy behavior:
			// both ignore reasoning history and only forward reasoning.effort -> reasoning.
			continue
		default:
			pendingUser = append(pendingUser, v3TextPart(" "))
		}
	}
	flushUser()
	if len(prompt) == 0 {
		prompt = append(prompt, v3UserPart(" "))
	}
	return prompt
}

// responsesContentToV3Parts converts a message content value (string or block
// list) into v3 content parts.
func responsesContentToV3Parts(content any) []map[string]any {
	if s, ok := content.(string); ok {
		return []map[string]any{v3TextPart(s)}
	}
	blocks, ok := content.([]any)
	if !ok {
		return []map[string]any{v3TextPart(" ")}
	}
	parts := []map[string]any{}
	for _, block := range blocks {
		obj, ok := block.(map[string]any)
		if !ok {
			parts = append(parts, v3TextPart(" "))
			continue
		}
		switch typ := asString(obj["type"]); typ {
		case "input_text", "output_text", "text":
			parts = append(parts, v3TextPart(asString(obj["text"])))
		case "input_image", "image_url":
			parts = append(parts, v3ImagePart(obj))
		default:
			parts = append(parts, v3TextPart(" "))
		}
	}
	if len(parts) == 0 {
		parts = append(parts, v3TextPart(" "))
	}
	return parts
}

func v3UserPart(text string) map[string]any {
	if strings.TrimSpace(text) == "" {
		text = " "
	}
	return map[string]any{"role": "user", "content": []map[string]any{v3TextPart(text)}}
}

func v3TextPart(text string) map[string]any {
	if strings.TrimSpace(text) == "" {
		text = " "
	}
	return map[string]any{"type": "text", "text": text}
}

func v3ImagePart(obj map[string]any) map[string]any {
	imageURL, _ := obj["image_url"].(map[string]any)
	raw := asString(obj["url"])
	if imageURL != nil {
		raw = asString(imageURL["url"])
	}
	if raw == "" {
		return v3TextPart(" ")
	}
	mediaType := "image/png"
	data := raw
	if strings.HasPrefix(raw, "data:") {
		if header, rest, found := strings.Cut(raw[5:], ","); found {
			if mt, _, ok := strings.Cut(header, ";"); ok {
				mediaType = mt
			} else {
				mediaType = header
			}
			data = rest
		}
	}
	return map[string]any{"type": "file", "mediaType": mediaType, "data": data}
}

// responsesToolsToV3 converts Responses function tools into v3 tools
// (inputSchema instead of parameters).
func responsesToolsToV3(tools any) []map[string]any {
	items, ok := tools.([]any)
	if !ok {
		return nil
	}
	v3Tools := []map[string]any{}
	for _, item := range items {
		tool, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if asString(tool["type"]) != "function" {
			continue
		}
		fn := tool
		if f, ok := tool["function"].(map[string]any); ok {
			fn = f
		}
		schema := fn["parameters"]
		if schema == nil {
			schema = fn["input_schema"]
		}
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		v3Tools = append(v3Tools, map[string]any{
			"type":        "function",
			"name":        asString(fn["name"]),
			"description": asString(fn["description"]),
			"inputSchema": schema,
		})
	}
	return v3Tools
}

func responsesToolChoiceToV3(tc any) map[string]any {
	if tc == nil {
		return map[string]any{"type": "auto"}
	}
	switch v := tc.(type) {
	case string:
		switch v {
		case "auto", "none", "required":
			return map[string]any{"type": v}
		default:
			return map[string]any{"type": "tool", "toolName": v}
		}
	case map[string]any:
		if asString(v["type"]) == "function" {
			if fn, ok := v["function"].(map[string]any); ok {
				return map[string]any{"type": "tool", "toolName": asString(fn["name"])}
			}
			return map[string]any{"type": "tool", "toolName": asString(v["name"])}
		}
		return map[string]any{"type": asString(v["type"])}
	}
	return map[string]any{"type": "auto"}
}

// responsesReasoningToV3 maps Responses reasoning.effort to the v3 reasoning
// level. Defaults to xhigh (matching the fx free pool behavior).
func responsesReasoningToV3(reasoning any) string {
	if r, ok := reasoning.(map[string]any); ok {
		switch strings.ToLower(asString(r["effort"])) {
		case "xhigh", "max":
			return "xhigh"
		case "high":
			return "high"
		case "medium", "low", "minimal", "auto", "default":
			return "auto"
		case "off", "none":
			return ""
		}
	}
	return "xhigh"
}

// --- Response: v3 SSE events -> Responses protocol ---

// v3Event is a single parsed data: payload from the v3 SSE stream.
type v3Event map[string]any

func (e v3Event) typeName() string { return asString(e["type"]) }
func (e v3Event) text() string     { return asString(e["delta"]) }

// arguments returns the tool-call arguments payload, tolerating both the
// string `delta` form and the object `input` form.
func (e v3Event) arguments() string {
	if delta := e.text(); delta != "" {
		return delta
	}
	if input, ok := e["input"]; ok && input != nil {
		if s, ok := input.(string); ok {
			return s
		}
		if b, err := json.Marshal(input); err == nil {
			return string(b)
		}
	}
	return ""
}

// v3SSEParser reads a v3 SSE stream and yields each data payload.
type v3SSEParser struct {
	reader *bufio.Reader
}

func newV3SSEParser(r io.Reader) *v3SSEParser {
	return &v3SSEParser{reader: bufio.NewReader(r)}
}

func (p *v3SSEParser) next() (v3Event, error) {
	for {
		line, err := p.reader.ReadBytes('\n')
		if len(line) == 0 && err != nil {
			return nil, err
		}
		trimmed := bytes.TrimSpace(line)
		if !bytes.HasPrefix(trimmed, []byte("data:")) {
			if err != nil {
				return nil, err
			}
			continue
		}
		payload := bytes.TrimSpace(trimmed[len("data:"):])
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			if err != nil {
				return nil, err
			}
			continue
		}
		var ev v3Event
		if err := json.Unmarshal(payload, &ev); err != nil {
			return nil, err
		}
		return ev, nil
	}
}

// fxOutputItem is one entry of the Responses output array. fxResponseState
// keeps items in arrival order, so the slice index doubles as both the
// streamed output_index and the item's position in the final output array —
// keeping the two in lockstep for strict clients (async-openai).
type fxOutputItem struct {
	kind      string // "reasoning", "function_call" or "message"
	id        string // rs_ / fc_ / msg_ prefixed item id
	toolID    string // v3 tool id (function_call only)
	name      string
	arguments string
}

// fxResponseState accumulates a v3 stream into a Responses response.
type fxResponseState struct {
	model          string
	responseID     string
	createdAt      int64
	textParts      []string
	reasoningParts []string
	// items holds every output item in arrival order.
	items []fxOutputItem
	// toolIndex maps v3 tool id to the item's position in items.
	toolIndex map[string]int
	// reasoningIdx and messageIdx point into items (-1 until first seen).
	reasoningIdx int
	messageIdx   int
	usage        map[string]any
	finish       string
	done         bool
}

func newFXResponseState(model string) *fxResponseState {
	return &fxResponseState{
		model:        model,
		responseID:   "resp_" + fxHex(8),
		createdAt:    time.Now().Unix(),
		toolIndex:    map[string]int{},
		reasoningIdx: -1,
		messageIdx:   -1,
	}
}

// observe folds one v3 event into the accumulator and reports what changed so
// the SSE reader can emit the matching Responses events. index is the item's
// position in s.items (and therefore its output_index); delta is the text or
// argument delta to forward ("" when the event adds nothing new); created
// reports whether the event opened a new output item.
func (s *fxResponseState) observe(ev v3Event) (kind, itemID string, index int, delta string, created bool) {
	switch ev.typeName() {
	case "text-delta":
		idx, created := s.observeText(ev.text())
		return "message", s.items[idx].id, idx, ev.text(), created
	case "reasoning-delta":
		idx, created := s.observeReasoning(ev.text())
		return "reasoning", s.items[idx].id, idx, ev.text(), created
	case "tool-input-start", "tool-call":
		id := toolEventID(ev)
		if id == "" {
			id = "call_" + fxHex(8)
		}
		name := asString(ev["toolName"])
		idx, created := s.observeToolStart(id, name)
		if ev.typeName() == "tool-call" {
			// A terminal tool-call carries the complete input; forward only the
			// part not already streamed by tool-input-delta (may be empty).
			delta = s.observeToolSnapshot(id, name, ev.arguments())
		}
		return "function_call", s.items[idx].id, idx, delta, created
	case "tool-input-delta":
		id := toolEventID(ev)
		idx, ok := s.toolIndex[id]
		if !ok {
			return "", "", -1, "", false
		}
		s.items[idx].arguments += ev.text()
		return "function_call", s.items[idx].id, idx, ev.text(), false
	case "finish":
		s.observeFinish(ev)
	}
	return "", "", -1, "", false
}

func (s *fxResponseState) observeText(delta string) (int, bool) {
	created := s.messageIdx == -1
	if created {
		s.messageIdx = len(s.items)
		s.items = append(s.items, fxOutputItem{kind: "message", id: "msg_" + fxHex(8)})
	}
	s.textParts = append(s.textParts, delta)
	return s.messageIdx, created
}

func (s *fxResponseState) observeReasoning(delta string) (int, bool) {
	created := s.reasoningIdx == -1
	if created {
		s.reasoningIdx = len(s.items)
		s.items = append(s.items, fxOutputItem{kind: "reasoning", id: "rs_" + fxHex(8)})
	}
	s.reasoningParts = append(s.reasoningParts, delta)
	return s.reasoningIdx, created
}

// observeToolStart registers a tool call (or refreshes its name) and returns
// the item's position in s.items.
func (s *fxResponseState) observeToolStart(id, name string) (int, bool) {
	if idx, ok := s.toolIndex[id]; ok {
		if name != "" {
			s.items[idx].name = name
		}
		return idx, false
	}
	idx := len(s.items)
	s.toolIndex[id] = idx
	s.items = append(s.items, fxOutputItem{kind: "function_call", id: "fc_" + fxHex(8), toolID: id, name: name})
	return idx, true
}

// observeToolSnapshot applies the complete arguments snapshot from a terminal
// tool-call event and returns only the suffix not yet delivered by
// tool-input-delta events, so streamed deltas and the final item always agree.
func (s *fxResponseState) observeToolSnapshot(id, name, snapshot string) string {
	idx, ok := s.toolIndex[id]
	if !ok {
		return ""
	}
	if name != "" {
		s.items[idx].name = name
	}
	delta := mergeToolArgumentSnapshot(s.items[idx].arguments, snapshot)
	s.items[idx].arguments += delta
	return delta
}

func (s *fxResponseState) observeFinish(ev v3Event) {
	if fr, ok := ev["finishReason"].(map[string]any); ok {
		s.finish = asString(fr["unified"])
		if s.finish == "" {
			s.finish = asString(fr["raw"])
		}
	}
	if u, ok := ev["usage"].(map[string]any); ok {
		s.usage = u
	}
	s.done = true
}

// renderItem renders the output item at position i for output_item.done and
// the final response.
func (s *fxResponseState) renderItem(i int) map[string]any {
	item := s.items[i]
	switch item.kind {
	case "reasoning":
		return map[string]any{
			"id":     item.id,
			"type":   "reasoning",
			"status": "completed",
			"summary": []any{
				map[string]any{"type": "summary_text", "text": strings.Join(s.reasoningParts, "")},
			},
		}
	case "function_call":
		return map[string]any{
			"id":        item.id,
			"type":      "function_call",
			"status":    "completed",
			"call_id":   item.toolID,
			"name":      item.name,
			"arguments": item.arguments,
		}
	default:
		return map[string]any{
			"id":     item.id,
			"type":   "message",
			"status": "completed",
			"role":   "assistant",
			"content": []any{
				map[string]any{"type": "output_text", "text": strings.Join(s.textParts, ""), "annotations": []any{}},
			},
		}
	}
}

// buildOutput assembles the Responses output item list in arrival order.
func (s *fxResponseState) buildOutput() []any {
	output := make([]any, 0, len(s.items))
	for i := range s.items {
		output = append(output, s.renderItem(i))
	}
	if len(output) == 0 {
		output = append(output, map[string]any{
			"id":     "msg_" + fxHex(8),
			"type":   "message",
			"status": "completed",
			"role":   "assistant",
			"content": []any{
				map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
			},
		})
	}
	return output
}

// buildResponse assembles the final Responses JSON object.
func (s *fxResponseState) buildResponse() map[string]any {
	status := "completed"
	incompleteDetails := any(nil)
	if s.finish == "length" {
		status = "incomplete"
		incompleteDetails = map[string]any{"reason": "max_output_tokens"}
	}
	return map[string]any{
		"id":                  s.responseID,
		"object":              "response",
		"created_at":          s.createdAt,
		"status":              status,
		"error":               nil,
		"incomplete_details":  incompleteDetails,
		"model":               s.model,
		"output":              s.buildOutput(),
		"parallel_tool_calls": true,
		"tool_choice":         "auto",
		"tools":               []any{},
		"output_text":         strings.Join(s.textParts, ""),
		"usage":               responsesUsage(s.usage),
		"metadata":            map[string]any{},
		"store":               false,
		"truncation":          "disabled",
	}
}

// responsesUsage converts the v3 usage object into Responses usage shape.
func responsesUsage(v3Usage map[string]any) map[string]any {
	pt := int64(0)
	ct := int64(0)
	cached := int64(0)
	reasoning := int64(0)
	if in, ok := v3Usage["inputTokens"].(map[string]any); ok {
		pt = asInt64(in["total"])
		cached = asInt64(in["cacheRead"])
	}
	if out, ok := v3Usage["outputTokens"].(map[string]any); ok {
		ct = asInt64(out["total"])
		reasoning = asInt64(out["reasoning"])
	}
	if raw, ok := v3Usage["raw"].(map[string]any); ok {
		if v := asInt64(raw["prompt_tokens"]); v > 0 {
			pt = v
		}
		if v := asInt64(raw["completion_tokens"]); v > 0 {
			ct = v
		}
		if details, ok := raw["prompt_tokens_details"].(map[string]any); ok {
			if v := asInt64(details["cached_tokens"]); v > 0 {
				cached = v
			}
		}
		if v := asInt64(raw["reasoning_tokens"]); v > 0 {
			reasoning = v
		}
	}
	return map[string]any{
		"input_tokens":  pt,
		"output_tokens": ct,
		"total_tokens":  pt + ct,
		"input_tokens_details": map[string]any{
			"cached_tokens": cached,
		},
		"output_tokens_details": map[string]any{
			"reasoning_tokens": reasoning,
		},
	}
}

// extractFXUsage reads the original v3 usage object rather than the normalized
// Responses usage object. The latter must always contain cached_tokens: 0 for
// strict clients, which would incorrectly mark an upstream response as
// cache-supported when the gateway did not report cache information.
func extractFXUsage(body []byte) store.UsageMetrics {
	var root map[string]any
	if json.Unmarshal(body, &root) == nil {
		if usage, ok := root["usage"].(map[string]any); ok {
			return extractFXUsageMap(usage)
		}
		return store.UsageMetrics{}
	}

	var last store.UsageMetrics
	scanner := bufio.NewScanner(bytes.NewReader(body))
	const maxLine = 1024 * 1024
	buf := make([]byte, 64*1024)
	scanner.Buffer(buf, maxLine)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
			continue
		}
		if json.Unmarshal(data, &root) != nil {
			continue
		}
		if usage, ok := root["usage"].(map[string]any); ok {
			last = extractFXUsageMap(usage)
		}
	}
	return last
}

func extractFXUsageMap(usage map[string]any) store.UsageMetrics {
	result := store.UsageMetrics{UsagePresent: true}
	var total, cached, cacheWrite int64
	var totalOK, cachedOK, cacheWriteOK, noCacheOK bool

	if input, ok := usage["inputTokens"].(map[string]any); ok {
		total, totalOK = firstNumberOK(input, "total", "totalTokens", "inputTokens")
		cached, cachedOK = firstNumberOK(input, "cacheRead", "cache_read", "cacheReadTokens", "cachedTokens")
		cacheWrite, cacheWriteOK = firstNumberOK(input, "cacheWrite", "cache_write", "cacheWriteTokens")
		_, noCacheOK = firstNumberOK(input, "noCache", "no_cache", "uncached")
	} else if value, ok := firstNumberOK(usage, "inputTokens", "input_tokens"); ok {
		total, totalOK = value, true
	}

	if raw, ok := usage["raw"].(map[string]any); ok {
		if value, ok := firstNumberOK(raw, "prompt_tokens", "input_tokens"); ok && !totalOK {
			total, totalOK = value, true
		}
		if value, ok := firstNumberOK(raw, "cache_read_input_tokens", "cache_read_tokens"); ok && !cachedOK {
			cached, cachedOK = value, true
		}
		if value, ok := firstNumberOK(raw, "cache_write_input_tokens", "cache_write_tokens"); ok && !cacheWriteOK {
			cacheWrite, cacheWriteOK = value, true
		}
		if details, ok := raw["prompt_tokens_details"].(map[string]any); ok {
			if value, ok := firstNumberOK(details, "cached_tokens", "cache_read_tokens"); ok && !cachedOK {
				cached, cachedOK = value, true
			}
			if value, ok := firstNumberOK(details, "cache_write_tokens"); ok && !cacheWriteOK {
				cacheWrite, cacheWriteOK = value, true
			}
		}
	}

	if details, ok := usage["input_tokens_details"].(map[string]any); ok {
		if value, ok := firstNumberOK(details, "cached_tokens", "cache_read_tokens"); ok && !cachedOK {
			cached, cachedOK = value, true
		}
		if value, ok := firstNumberOK(details, "cache_write_tokens"); ok && !cacheWriteOK {
			cacheWrite = value
		}
	}

	if output, ok := usage["outputTokens"].(map[string]any); ok {
		result.OutputTokens, _ = firstNumberOK(output, "total", "totalTokens", "outputTokens")
		result.ReasoningTokens, _ = firstNumberOK(output, "reasoning", "reasoningTokens")
	} else {
		result.OutputTokens = firstNumber(usage, "output_tokens", "completion_tokens")
		result.ReasoningTokens = firstNumber(usage, "reasoning_tokens")
	}
	if raw, ok := usage["raw"].(map[string]any); ok {
		if value := firstNumber(raw, "completion_tokens", "output_tokens"); value > 0 {
			result.OutputTokens = value
		}
		if value := firstNumber(raw, "reasoning_tokens"); value > 0 {
			result.ReasoningTokens = value
		}
	}

	if totalOK {
		result.PromptTokens = total
		// When the v3 gateway reports both cacheRead and noCache, cache
		// accounting is supported — cacheRead: 0 means a genuine cache miss
		// (0% hit rate), not that caching is unsupported. Only treat caching
		// as unsupported when neither field is present.
		if cachedOK || noCacheOK {
			result.CacheReadTokens = cached
			result.CacheWriteTokens = cacheWrite
			result.InputTokens = maxInt64(0, total-cached-cacheWrite)
			result.CacheSupported = true
			result.CacheSource = "v3.inputTokens"
		} else {
			result.InputTokens = total
		}
	}
	return result
}

// vercelFXSSEToResponses converts a full v3 SSE stream (non-streaming client
// request) into a Responses JSON object.
func vercelFXSSEToResponses(model string, reader io.Reader) ([]byte, error) {
	state := newFXResponseState(model)
	parser := newV3SSEParser(reader)
	for {
		ev, err := parser.next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		state.observe(ev)
	}
	return json.Marshal(state.buildResponse())
}

// vercelFXSSEReader converts a v3 SSE stream into Responses protocol SSE on
// the fly (streaming client request). The streamed output_index values come
// from the shared fxResponseState, so they always match the positions of the
// same items in the final response.output array.
type vercelFXSSEReader struct {
	parser  *v3SSEParser
	state   *fxResponseState
	pending *bytes.Buffer
	started bool
	done    bool
	// sequence is the strictly increasing sequence_number each SSE event
	// carries, as required by strict Responses clients (async-openai).
	sequence int64
}

func newVercelFXSSEReader(reader io.Reader, model string) *vercelFXSSEReader {
	return &vercelFXSSEReader{
		parser:  newV3SSEParser(reader),
		state:   newFXResponseState(model),
		pending: &bytes.Buffer{},
	}
}

func (r *vercelFXSSEReader) Read(p []byte) (int, error) {
	for {
		if r.pending.Len() > 0 {
			return r.pending.Read(p)
		}
		if r.done {
			return 0, io.EOF
		}
		ev, err := r.parser.next()
		if errors.Is(err, io.EOF) {
			r.finalize()
			if r.pending.Len() > 0 {
				return r.pending.Read(p)
			}
			r.done = true
			return 0, io.EOF
		}
		if err != nil {
			r.done = true
			return 0, err
		}
		r.emit(ev)
	}
}

// emit writes the Responses SSE events for one v3 event. It delegates all
// state mutation to fxResponseState.observe so that the streamed output_index
// values come from the same arrival-ordered item list that buildOutput uses for
// the final response — guaranteeing the two stay in lockstep.
func (r *vercelFXSSEReader) emit(ev v3Event) {
	if !r.started {
		r.started = true
		r.writeEvent(map[string]any{
			"type": "response.created",
			"response": map[string]any{
				"id":                  r.state.responseID,
				"object":              "response",
				"created_at":          r.state.createdAt,
				"status":              "in_progress",
				"error":               nil,
				"incomplete_details":  nil,
				"model":               r.state.model,
				"output":              []any{},
				"parallel_tool_calls": true,
				"tool_choice":         "auto",
				"tools":               []any{},
				"output_text":         "",
				"usage": map[string]any{
					"input_tokens":          0,
					"output_tokens":         0,
					"total_tokens":          0,
					"input_tokens_details":  map[string]any{"cached_tokens": 0},
					"output_tokens_details": map[string]any{"reasoning_tokens": 0},
				},
				"metadata":   map[string]any{},
				"store":      false,
				"truncation": "disabled",
			},
		})
	}
	kind, itemID, index, delta, created := r.state.observe(ev)
	switch kind {
	case "message":
		if created {
			r.writeEvent(map[string]any{
				"type":         "response.output_item.added",
				"output_index": index,
				"item": map[string]any{
					"id":      itemID,
					"type":    "message",
					"status":  "in_progress",
					"role":    "assistant",
					"content": []any{},
				},
			})
			r.writeEvent(map[string]any{
				"type":          "response.content_part.added",
				"item_id":       itemID,
				"output_index":  index,
				"content_index": 0,
				"part":          map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
			})
		}
		r.writeEvent(map[string]any{
			"type":          "response.output_text.delta",
			"item_id":       itemID,
			"output_index":  index,
			"content_index": 0,
			"delta":         delta,
		})
	case "reasoning":
		if created {
			r.writeEvent(map[string]any{
				"type":         "response.output_item.added",
				"output_index": index,
				"item": map[string]any{
					"id":      itemID,
					"type":    "reasoning",
					"status":  "in_progress",
					"summary": []any{},
				},
			})
		}
		r.writeEvent(map[string]any{
			"type":          "response.reasoning_text.delta",
			"item_id":       itemID,
			"output_index":  index,
			"content_index": 0,
			"delta":         delta,
		})
	case "function_call":
		if created {
			item := r.state.items[index]
			r.writeEvent(map[string]any{
				"type":         "response.output_item.added",
				"output_index": index,
				"item": map[string]any{
					"id":        itemID,
					"type":      "function_call",
					"status":    "in_progress",
					"call_id":   item.toolID,
					"name":      item.name,
					"arguments": "",
				},
			})
		}
		if delta != "" {
			r.writeEvent(map[string]any{
				"type":         "response.function_call_arguments.delta",
				"item_id":      itemID,
				"output_index": index,
				"delta":        delta,
			})
		}
	}
}

// finalize emits the terminal Responses events (done + completed) after the
// v3 stream ends.
func (r *vercelFXSSEReader) finalize() {
	if r.done {
		return
	}
	r.done = true
	for i := range r.state.items {
		item := r.state.items[i]
		switch item.kind {
		case "function_call":
			r.writeEvent(map[string]any{
				"type":         "response.function_call_arguments.done",
				"item_id":      item.id,
				"output_index": i,
				"arguments":    item.arguments,
			})
		case "message":
			text := strings.Join(r.state.textParts, "")
			r.writeEvent(map[string]any{
				"type":          "response.output_text.done",
				"item_id":       item.id,
				"output_index":  i,
				"content_index": 0,
				"text":          text,
			})
			r.writeEvent(map[string]any{
				"type":          "response.content_part.done",
				"item_id":       item.id,
				"output_index":  i,
				"content_index": 0,
				"part":          map[string]any{"type": "output_text", "text": text, "annotations": []any{}},
			})
		case "reasoning":
			r.writeEvent(map[string]any{
				"type":          "response.reasoning_text.done",
				"item_id":       item.id,
				"output_index":  i,
				"content_index": 0,
				"text":          strings.Join(r.state.reasoningParts, ""),
			})
		}
		r.writeEvent(map[string]any{
			"type":         "response.output_item.done",
			"output_index": i,
			"item":         r.state.renderItem(i),
		})
	}
	r.writeEvent(map[string]any{
		"type":     "response.completed",
		"response": r.state.buildResponse(),
	})
}

func (r *vercelFXSSEReader) writeEvent(event map[string]any) {
	event["sequence_number"] = r.sequence
	r.sequence++
	data, _ := json.Marshal(event)
	r.pending.WriteString("event: " + asString(event["type"]) + "\n")
	r.pending.WriteString("data: ")
	r.pending.Write(data)
	r.pending.WriteString("\n\n")
}

// --- shared helpers ---

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func asInt64(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int:
		return int64(n)
	case int64:
		return n
	case json.Number:
		i, _ := n.Int64()
		return i
	}
	return 0
}

func asFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

func toolEventID(ev v3Event) string {
	for _, key := range []string{"id", "toolCallId", "call_id"} {
		if id := asString(ev[key]); id != "" {
			return id
		}
	}
	return ""
}

// mergeToolArgumentSnapshot returns only the suffix not already emitted by
// tool-input-delta events. A terminal tool-call event carries the complete
// input in current versions of the v3 protocol.
func mergeToolArgumentSnapshot(current, snapshot string) string {
	if snapshot == "" || snapshot == current {
		return ""
	}
	if current == "" {
		return snapshot
	}
	if strings.HasPrefix(snapshot, current) {
		return snapshot[len(current):]
	}
	// Do not append an unrelated complete snapshot: that would make the
	// Responses function arguments invalid JSON. The deltas remain authoritative.
	return ""
}
