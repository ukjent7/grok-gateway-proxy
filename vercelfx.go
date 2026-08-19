package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"
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
	model := asString(req["model"])
	if model == "" {
		model = "zai/glm-5.2"
	}
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
						"toolCallId": asString(obj["call_id"]),
						"toolName":   asString(obj["name"]),
						"input":      args,
					},
				},
			})
		case "function_call_output":
			flushUser()
			output := obj["output"]
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
						"toolCallId": asString(obj["call_id"]),
						"toolName":   asString(obj["name"]),
						"output":     map[string]any{"type": "text", "value": output},
					},
				},
			})
		case "input_text", "text":
			pendingUser = append(pendingUser, v3TextPart(asString(obj["text"])))
		case "input_image", "image_url":
			pendingUser = append(pendingUser, v3ImagePart(obj))
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

// fxResponseState accumulates a v3 stream into a Responses response.
type fxResponseState struct {
	model          string
	responseID     string
	createdAt      int64
	textParts      []string
	reasoningParts []string
	// toolCalls keyed by v3 tool id
	toolCalls map[string]map[string]any
	// toolOrder preserves tool-call arrival order
	toolOrder []string
	usage     map[string]any
	finish    string
	done      bool
}

func newFXResponseState(model string) *fxResponseState {
	return &fxResponseState{
		model:      model,
		responseID: "resp_" + fxHex(8),
		createdAt:  time.Now().Unix(),
		toolCalls:  map[string]map[string]any{},
	}
}

// apply processes one v3 event into the shared accumulator.
func (s *fxResponseState) apply(ev v3Event) {
	switch ev.typeName() {
	case "text-delta":
		s.textParts = append(s.textParts, ev.text())
	case "reasoning-delta":
		s.reasoningParts = append(s.reasoningParts, ev.text())
	case "tool-input-start":
		id := asString(ev["id"])
		if id == "" {
			id = "call_" + fxHex(8)
		}
		s.toolCalls[id] = map[string]any{
			"id":        id,
			"name":      asString(ev["toolName"]),
			"arguments": "",
		}
		s.toolOrder = append(s.toolOrder, id)
	case "tool-input-delta":
		id := asString(ev["id"])
		if call, ok := s.toolCalls[id]; ok {
			call["arguments"] = asString(call["arguments"]) + ev.text()
		}
	case "tool-call":
		id := asString(ev["toolCallId"])
		if id == "" {
			id = "call_" + fxHex(8)
		}
		s.toolCalls[id] = map[string]any{
			"id":        id,
			"name":      asString(ev["toolName"]),
			"arguments": ev.arguments(),
		}
		s.toolOrder = append(s.toolOrder, id)
	case "finish":
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
}

// buildOutput assembles the Responses output item list from accumulated parts.
func (s *fxResponseState) buildOutput() []any {
	output := []any{}
	if len(s.reasoningParts) > 0 {
		output = append(output, map[string]any{
			"id":     "rs_" + fxHex(8),
			"type":   "reasoning",
			"status": "completed",
			"summary": []any{
				map[string]any{"type": "summary_text", "text": strings.Join(s.reasoningParts, "")},
			},
		})
	}
	for _, id := range s.toolOrder {
		call := s.toolCalls[id]
		output = append(output, map[string]any{
			"id":        "fc_" + fxHex(8),
			"type":      "function_call",
			"status":    "completed",
			"call_id":   id,
			"name":      asString(call["name"]),
			"arguments": asString(call["arguments"]),
		})
	}
	text := strings.Join(s.textParts, "")
	if text != "" || len(output) == 0 {
		output = append(output, map[string]any{
			"id":     "msg_" + fxHex(8),
			"type":   "message",
			"status": "completed",
			"role":   "assistant",
			"content": []any{
				map[string]any{"type": "output_text", "text": text, "annotations": []any{}},
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

// vercelFXSSEToResponses converts a full v3 SSE stream (non-streaming client
// request) into a Responses JSON object.
func vercelFXSSEToResponses(model string, reader io.Reader) ([]byte, error) {
	state := newFXResponseState(model)
	parser := newV3SSEParser(reader)
	for {
		ev, err := parser.next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		state.apply(ev)
	}
	return json.Marshal(state.buildResponse())
}

// vercelFXSSEReader converts a v3 SSE stream into Responses protocol SSE on
// the fly (streaming client request).
type vercelFXSSEReader struct {
	parser  *v3SSEParser
	state   *fxResponseState
	pending *bytes.Buffer
	// message/item stream state
	started        bool
	messageID      string
	messageStarted bool
	// output_index for the message item
	messageOutputIndex int
	// emitted tool-call ids (for done events)
	emittedTools []string
	done         bool
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
		if err == io.EOF {
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

// emit writes the Responses SSE events for one v3 event.
func (r *vercelFXSSEReader) emit(ev v3Event) {
	if !r.started {
		r.started = true
		r.writeEvent(map[string]any{
			"type": "response.created",
			"response": map[string]any{
				"id":         r.state.responseID,
				"object":     "response",
				"created_at": r.state.createdAt,
				"status":     "in_progress",
				"model":      r.state.model,
				"output":     []any{},
				"usage": map[string]any{
					"input_tokens":  0,
					"output_tokens": 0,
					"total_tokens":  0,
				},
			},
		})
	}
	switch ev.typeName() {
	case "text-delta":
		if !r.messageStarted {
			r.messageStarted = true
			r.messageID = "msg_" + fxHex(8)
			r.messageOutputIndex = len(r.state.toolOrder)
			r.writeEvent(map[string]any{
				"type":         "response.output_item.added",
				"output_index": r.messageOutputIndex,
				"item": map[string]any{
					"id":      r.messageID,
					"type":    "message",
					"status":  "in_progress",
					"role":    "assistant",
					"content": []any{},
				},
			})
			r.writeEvent(map[string]any{
				"type":          "response.content_part.added",
				"item_id":       r.messageID,
				"output_index":  r.messageOutputIndex,
				"content_index": 0,
				"part":          map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
			})
		}
		delta := ev.text()
		r.state.textParts = append(r.state.textParts, delta)
		r.writeEvent(map[string]any{
			"type":          "response.output_text.delta",
			"item_id":       r.messageID,
			"output_index":  r.messageOutputIndex,
			"content_index": 0,
			"delta":         delta,
		})
	case "reasoning-delta":
		r.state.reasoningParts = append(r.state.reasoningParts, ev.text())
	case "tool-input-start", "tool-call":
		r.emitToolStart(ev)
		if args := ev.arguments(); args != "" {
			r.emitToolDelta(ev)
		}
	case "tool-input-delta":
		r.emitToolDelta(ev)
	case "finish":
		r.state.apply(ev)
	}
}

func (r *vercelFXSSEReader) emitToolStart(ev v3Event) {
	id := asString(ev["id"])
	if id == "" {
		id = asString(ev["toolCallId"])
	}
	if id == "" {
		id = "call_" + fxHex(8)
	}
	name := asString(ev["toolName"])
	call, exists := r.state.toolCalls[id]
	if !exists {
		call = map[string]any{
			"id":        id,
			"name":      name,
			"arguments": "",
			"fc_id":     "fc_" + fxHex(8),
		}
		r.state.toolCalls[id] = call
		r.state.toolOrder = append(r.state.toolOrder, id)
	}
	outputIndex := indexOf(r.state.toolOrder, id)
	r.writeEvent(map[string]any{
		"type":         "response.output_item.added",
		"output_index": outputIndex,
		"item": map[string]any{
			"id":        asString(call["fc_id"]),
			"type":      "function_call",
			"status":    "in_progress",
			"call_id":   id,
			"name":      asString(call["name"]),
			"arguments": "",
		},
	})
}

func (r *vercelFXSSEReader) emitToolDelta(ev v3Event) {
	id := asString(ev["id"])
	if id == "" {
		id = asString(ev["toolCallId"])
	}
	call, ok := r.state.toolCalls[id]
	if !ok {
		return
	}
	delta := ev.arguments()
	if delta == "" {
		return
	}
	call["arguments"] = asString(call["arguments"]) + delta
	outputIndex := indexOf(r.state.toolOrder, id)
	r.writeEvent(map[string]any{
		"type":         "response.function_call_arguments.delta",
		"item_id":      asString(call["fc_id"]),
		"output_index": outputIndex,
		"delta":        delta,
	})
}

// finalize emits the terminal Responses events (done + completed) after the
// v3 stream ends.
func (r *vercelFXSSEReader) finalize() {
	if r.done {
		return
	}
	r.done = true
	for _, id := range r.state.toolOrder {
		call := r.state.toolCalls[id]
		outputIndex := indexOf(r.state.toolOrder, id)
		fcID := asString(call["fc_id"])
		r.writeEvent(map[string]any{
			"type":         "response.function_call_arguments.done",
			"item_id":      fcID,
			"output_index": outputIndex,
			"arguments":    asString(call["arguments"]),
		})
		r.writeEvent(map[string]any{
			"type":         "response.output_item.done",
			"output_index": outputIndex,
			"item": map[string]any{
				"id":        fcID,
				"type":      "function_call",
				"status":    "completed",
				"call_id":   id,
				"name":      asString(call["name"]),
				"arguments": asString(call["arguments"]),
			},
		})
	}
	if r.messageStarted {
		text := strings.Join(r.state.textParts, "")
		r.writeEvent(map[string]any{
			"type":          "response.output_text.done",
			"item_id":       r.messageID,
			"output_index":  r.messageOutputIndex,
			"content_index": 0,
			"text":          text,
		})
		r.writeEvent(map[string]any{
			"type":          "response.content_part.done",
			"item_id":       r.messageID,
			"output_index":  r.messageOutputIndex,
			"content_index": 0,
			"part":          map[string]any{"type": "output_text", "text": text, "annotations": []any{}},
		})
		r.writeEvent(map[string]any{
			"type":         "response.output_item.done",
			"output_index": r.messageOutputIndex,
			"item": map[string]any{
				"id":      r.messageID,
				"type":    "message",
				"status":  "completed",
				"role":    "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}},
			},
		})
	}
	r.writeEvent(map[string]any{
		"type":     "response.completed",
		"response": r.state.buildResponse(),
	})
}

func (r *vercelFXSSEReader) writeEvent(event map[string]any) {
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

func indexOf(items []string, target string) int {
	for i, item := range items {
		if item == target {
			return i
		}
	}
	return -1
}
