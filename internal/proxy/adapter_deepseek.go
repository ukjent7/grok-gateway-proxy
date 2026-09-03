package proxy

import (
	"bytes"
	"encoding/json"
	"io"
)

type DeepSeekResponsesAdapter struct{ baseResponsesAdapter }

func (DeepSeekResponsesAdapter) ValidateRequest(body []byte) error {
	return validateJSONRequest(body, "DeepSeek Responses")
}

func (DeepSeekResponsesAdapter) TransformRequestBody(body []byte) ([]byte, error) {
	sanitized, err := sanitizeResponsesRequest(body)
	if err != nil {
		return nil, err
	}
	return adaptResponsesRequestForDeepSeek(sanitized)
}

func (DeepSeekResponsesAdapter) TransformResponseBody(body []byte) ([]byte, error) {
	return body, nil
}

func (DeepSeekResponsesAdapter) TransformSSE(reader io.Reader) io.Reader {
	return newResponsesSSEFilter(reader)
}

// adaptResponsesRequestForDeepSeek 做 DeepSeek 方言对齐：移除 include、
// 清理 reasoning 的 summary/encrypted_content、minimal effort 降级为 low。
// 增量字节编辑，保留客户端正文的键序与字面量（同 sanitizeResponsesRequest）。
func adaptResponsesRequestForDeepSeek(body []byte) ([]byte, error) {
	hasInclude := bytes.Contains(body, []byte(`"include"`))
	hasReasoning := bytes.Contains(body, []byte(`"reasoning"`))
	if !hasInclude && !hasReasoning {
		return body, nil
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return body, nil
	}
	var edits []topLevelMemberEdit
	if hasInclude {
		if _, ok := payload["include"]; ok {
			edits = append(edits, topLevelMemberEdit{key: "include", delete: true})
		}
	}
	if hasReasoning {
		if raw, ok := payload["reasoning"]; ok {
			if mapped, effortChanged := mapDeepSeekReasoningEffort(raw); effortChanged {
				edits = append(edits, topLevelMemberEdit{key: "reasoning", value: mapped})
			}
		}
		if raw, ok := payload["input"]; ok {
			if cleaned, inputChanged := stripUnsupportedReasoningFields(raw); inputChanged {
				edits = append(edits, topLevelMemberEdit{key: "input", value: cleaned})
			}
		}
	}
	out, changed := applyTopLevelMemberEdits(body, edits)
	if !changed {
		return body, nil
	}
	return out, nil
}

var deepSeekEffortClamps = map[string]string{
	"minimal": "low",
}

func mapDeepSeekReasoningEffort(raw json.RawMessage) (json.RawMessage, bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return raw, false
	}
	effortRaw, ok := fields["effort"]
	if !ok {
		return raw, false
	}
	var effort string
	if err := json.Unmarshal(effortRaw, &effort); err != nil {
		return raw, false
	}
	clamped, ok := deepSeekEffortClamps[effort]
	if !ok {
		return raw, false
	}
	encoded, err := json.Marshal(clamped)
	if err != nil {
		return raw, false
	}
	fields["effort"] = encoded
	out, err := marshalJSONNoEscape(fields)
	if err != nil {
		return raw, false
	}
	return out, true
}

func stripUnsupportedReasoningFields(raw json.RawMessage) (json.RawMessage, bool) {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return raw, false
	}
	kept := make([]json.RawMessage, 0, len(items))
	changed := false
	for _, item := range items {
		var probe struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(item, &probe); err != nil || probe.Type != "reasoning" {
			kept = append(kept, item)
			continue
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(item, &fields); err != nil {
			kept = append(kept, item)
			continue
		}
		itemChanged := false
		if _, ok := fields["summary"]; ok {
			delete(fields, "summary")
			itemChanged = true
		}
		if _, ok := fields["encrypted_content"]; ok {
			delete(fields, "encrypted_content")
			itemChanged = true
		}
		if itemChanged {
			cleaned, err := marshalJSONNoEscape(fields)
			if err != nil {
				kept = append(kept, item)
				continue
			}
			kept = append(kept, cleaned)
			changed = true
		} else {
			kept = append(kept, item)
		}
	}
	if !changed {
		return raw, false
	}
	out, err := marshalJSONNoEscape(kept)
	if err != nil {
		return raw, false
	}
	return out, true
}
