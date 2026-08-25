package proxy

import (
	"encoding/json"
	"strings"
)

// convertResponsesToV3 converts a Responses API request body into the v3
// language-model payload, injecting the promo headers object. userAgent is
// the disguised fx UA (must match the HTTP User-Agent header).
func convertResponsesToV3(body []byte, userAgent string) ([]byte, error) {
	if userAgent == "" {
		userAgent = "fx/0.0.3"
	}
	var req ResponsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, err
	}

	var input any
	if len(req.Input) > 0 {
		_ = json.Unmarshal(req.Input, &input)
	}
	var instructions any
	if len(req.Instructions) > 0 {
		_ = json.Unmarshal(req.Instructions, &instructions)
		if s, ok := instructions.(string); ok {
			instructions = s
		} else {
			// Fallback: the instructions field may be non-string; preserve the
			// original string extraction semantics.
			instructions = asString(instructions)
		}
	}

	prompt := responsesInputToV3Prompt(input, instructions)

	v3 := V3Payload{
		Prompt: prompt,
		Headers: V3Headers{
			UserAgent: userAgent,
			XTitle:    vercelFXTitle,
		},
		MaxOutputTokens: decodeMaxOutputTokens(req.MaxOutputTokens),
	}

	if f, ok := decodeFloat(req.Temperature); ok {
		v3.Temperature = &f
	}
	if f, ok := decodeFloat(req.TopP); ok {
		v3.TopP = &f
	}
	if f, ok := decodeFloat(req.TopK); ok {
		v3.TopK = &f
	}
	if seq := decodeStopSequences(req.Stop); len(seq) > 0 {
		v3.StopSequences = seq
	}
	if len(req.Tools) > 0 {
		var rawTools any
		if json.Unmarshal(req.Tools, &rawTools) == nil {
			if tools := responsesToolsToV3(rawTools); len(tools) > 0 {
				v3.Tools = tools
				var rawChoice any
				if len(req.ToolChoice) > 0 {
					_ = json.Unmarshal(req.ToolChoice, &rawChoice)
				}
				v3.ToolChoice = responsesToolChoiceToV3(rawChoice)
			}
		}
	}
	var rawReasoning any
	if len(req.Reasoning) > 0 {
		_ = json.Unmarshal(req.Reasoning, &rawReasoning)
	}
	v3.Reasoning = responsesReasoningToV3(rawReasoning)
	if len(req.Text) > 0 {
		var rawText any
		if json.Unmarshal(req.Text, &rawText) == nil {
			if m, ok := rawText.(map[string]any); ok {
				if format, ok := m["format"].(map[string]any); ok {
					switch format["type"] {
					case "json_object", "json_schema":
						v3.ResponseFormat = map[string]any{"type": "json"}
					}
				}
			}
		}
	}
	return json.Marshal(v3)
}

// responsesInputToV3Prompt converts Responses `input` (string or item list)
// plus `instructions` into the v3 prompt array.
func responsesInputToV3Prompt(input any, instructions any) []V3Message {
	prompt := []V3Message{}
	if sys := asString(instructions); sys != "" {
		prompt = append(prompt, V3Message{
			Role:    "system",
			Content: sys,
		})
	}
	switch in := input.(type) {
	case string:
		return append(prompt, v3UserMessage(in))
	case nil:
		return append(prompt, v3UserMessage(" "))
	}
	items, ok := input.([]any)
	if !ok {
		return append(prompt, v3UserMessage(" "))
	}

	var pendingUser []V3ContentPart
	toolNames := map[string]string{}
	flushUser := func() {
		if len(pendingUser) > 0 {
			prompt = append(prompt, V3Message{Role: "user", Content: pendingUser})
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
				prompt = append(prompt, V3Message{Role: "assistant", Content: content})
			default:
				prompt = append(prompt, V3Message{Role: "user", Content: content})
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
			prompt = append(prompt, V3Message{
				Role: "assistant",
				Content: []V3ContentPart{
					{
						Type:       "tool-call",
						ToolCallID: callID,
						ToolName:   name,
						Input:      args,
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
			prompt = append(prompt, V3Message{
				Role: "tool",
				Content: []V3ContentPart{
					{
						Type:       "tool-result",
						ToolCallID: callID,
						ToolName:   toolName,
						Output:     map[string]any{"type": "text", "value": output},
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
		prompt = append(prompt, v3UserMessage(" "))
	}
	return prompt
}

// responsesContentToV3Parts converts a message content value (string or block
// list) into v3 content parts.
func responsesContentToV3Parts(content any) []V3ContentPart {
	if s, ok := content.(string); ok {
		return []V3ContentPart{v3TextPart(s)}
	}
	blocks, ok := content.([]any)
	if !ok {
		return []V3ContentPart{v3TextPart(" ")}
	}
	parts := []V3ContentPart{}
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

func v3UserMessage(text string) V3Message {
	if strings.TrimSpace(text) == "" {
		text = " "
	}
	return V3Message{Role: "user", Content: []V3ContentPart{v3TextPart(text)}}
}

func v3TextPart(text string) V3ContentPart {
	if strings.TrimSpace(text) == "" {
		text = " "
	}
	return V3ContentPart{Type: "text", Text: text}
}

func v3ImagePart(obj map[string]any) V3ContentPart {
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
	return V3ContentPart{Type: "file", MediaType: mediaType, Data: data}
}

// responsesToolsToV3 converts Responses function tools into v3 tools
// (inputSchema instead of parameters).
func responsesToolsToV3(tools any) []V3Tool {
	items, ok := tools.([]any)
	if !ok {
		return nil
	}
	v3Tools := []V3Tool{}
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
		v3Tools = append(v3Tools, V3Tool{
			Type:        "function",
			Name:        asString(fn["name"]),
			Description: asString(fn["description"]),
			InputSchema: schema,
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
