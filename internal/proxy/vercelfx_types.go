package proxy

import "encoding/json"

func decodeMaxOutputTokens(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return vercelFXMaxOutput
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err == nil {
		if v, err := n.Int64(); err == nil && v > 0 {
			return v
		}
		if f, err := n.Float64(); err == nil && int64(f) > 0 {
			return int64(f)
		}
	}
	var anyVal any
	if json.Unmarshal(raw, &anyVal) == nil {
		if v := asInt64(anyVal); v > 0 {
			return v
		}
	}
	return vercelFXMaxOutput
}

func decodeFloat(raw json.RawMessage) (float64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		return f, true
	}
	var anyVal any
	if json.Unmarshal(raw, &anyVal) == nil {
		return asFloat(anyVal)
	}
	return 0, false
}

func decodeStopSequences(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []string{s}
	}
	var arr []any
	if json.Unmarshal(raw, &arr) == nil {
		seq := make([]string, 0, len(arr))
		for _, item := range arr {
			seq = append(seq, asString(item))
		}
		return seq
	}
	return nil
}

// ResponsesRequest is the inbound Responses API shape relevant to FX disguise.
// Promiscuous fields use json.RawMessage so the conversion tolerates both
// string and structured variants without losing the original bytes.
type ResponsesRequest struct {
	Model           string          `json:"model"`
	Input           json.RawMessage `json:"input"`
	Instructions    json.RawMessage `json:"instructions"`
	MaxOutputTokens json.RawMessage `json:"max_output_tokens"`
	Temperature     json.RawMessage `json:"temperature"`
	TopP            json.RawMessage `json:"top_p"`
	TopK            json.RawMessage `json:"top_k"`
	Stop            json.RawMessage `json:"stop"`
	Tools           json.RawMessage `json:"tools"`
	ToolChoice      json.RawMessage `json:"tool_choice"`
	Reasoning       json.RawMessage `json:"reasoning"`
	Text            json.RawMessage `json:"text"`
}

// V3Payload is the v3 language-model request produced by convertResponsesToV3.
type V3Payload struct {
	Prompt          []V3Message `json:"prompt"`
	Headers         V3Headers   `json:"headers"`
	MaxOutputTokens int64       `json:"maxOutputTokens"`
	Temperature     *float64    `json:"temperature,omitempty"`
	TopP            *float64    `json:"topP,omitempty"`
	TopK            *float64    `json:"topK,omitempty"`
	StopSequences   []string    `json:"stopSequences,omitempty"`
	Tools           []V3Tool    `json:"tools,omitempty"`
	ToolChoice      any         `json:"toolChoice,omitempty"`
	Reasoning       string      `json:"reasoning,omitempty"`
	ResponseFormat  any         `json:"responseFormat,omitempty"`
}

// V3Headers carries the promo trigger that selects the free pool.
type V3Headers struct {
	UserAgent string `json:"user-agent"`
	XTitle    string `json:"x-title"`
}

// V3Message is one entry of the v3 prompt array. Content is string for the
// system message and []V3ContentPart for all other roles, matching the wire
// shape of the official fx client.
type V3Message struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

// V3ContentPart is a single content part inside a V3Message.
type V3ContentPart struct {
	Type       string `json:"type"`
	Text       string `json:"text,omitempty"`
	MediaType  string `json:"mediaType,omitempty"`
	Data       string `json:"data,omitempty"`
	ToolCallID string `json:"toolCallId,omitempty"`
	ToolName   string `json:"toolName,omitempty"`
	Input      any    `json:"input,omitempty"`
	Output     any    `json:"output,omitempty"`
}

// V3Tool mirrors the v3 tool description (function tools only).
type V3Tool struct {
	Type        string `json:"type"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	InputSchema any    `json:"inputSchema"`
}
