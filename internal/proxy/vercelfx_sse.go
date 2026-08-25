package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

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
