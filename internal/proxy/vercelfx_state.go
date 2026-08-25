package proxy

import (
	"strings"
	"time"
)

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

// strict clients, which would incorrectly mark an upstream response as
