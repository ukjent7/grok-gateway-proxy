package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
)

var standardResponsesToolTypes = map[string]bool{
	"function":                      true,
	"file_search":                   true,
	"computer_use_preview":          true,
	"web_search":                    true,
	"web_search_2025_08_26":         true,
	"web_search_preview":            true,
	"web_search_preview_2025_03_11": true,
	"mcp":                           true,
	"code_interpreter":              true,
	"image_generation":              true,
	"local_shell":                   true,
	"shell":                         true,
	"custom":                        true,
	"computer":                      true,
	"namespace":                     true,
	"tool_search":                   true,
	"apply_patch":                   true,
	"programmatic_tool_calling":     true,
}

var standardResponsesIncludeValues = map[string]bool{
	"file_search_call.results":              true,
	"web_search_call.results":               true,
	"web_search_call.action.sources":        true,
	"message.input_image.image_url":         true,
	"computer_call_output.output.image_url": true,
	"code_interpreter_call.outputs":         true,
	"reasoning.encrypted_content":           true,
	"message.output_text.logprobs":          true,
}

var responsesStreamEventTypes = map[string]bool{
	"response.created":                             true,
	"response.in_progress":                         true,
	"response.completed":                           true,
	"response.failed":                              true,
	"response.incomplete":                          true,
	"response.output_item.added":                   true,
	"response.output_item.done":                    true,
	"response.content_part.added":                  true,
	"response.content_part.done":                   true,
	"response.output_text.delta":                   true,
	"response.output_text.done":                    true,
	"response.refusal.delta":                       true,
	"response.refusal.done":                        true,
	"response.function_call_arguments.delta":       true,
	"response.function_call_arguments.done":        true,
	"response.file_search_call.in_progress":        true,
	"response.file_search_call.searching":          true,
	"response.file_search_call.completed":          true,
	"response.web_search_call.in_progress":         true,
	"response.web_search_call.searching":           true,
	"response.web_search_call.completed":           true,
	"response.reasoning_summary_part.added":        true,
	"response.reasoning_summary_part.done":         true,
	"response.reasoning_summary_text.delta":        true,
	"response.reasoning_summary_text.done":         true,
	"response.reasoning_text.delta":                true,
	"response.reasoning_text.done":                 true,
	"response.image_generation_call.completed":     true,
	"response.image_generation_call.generating":    true,
	"response.image_generation_call.in_progress":   true,
	"response.image_generation_call.partial_image": true,
	"response.mcp_call_arguments.delta":            true,
	"response.mcp_call_arguments.done":             true,
	"response.mcp_call.completed":                  true,
	"response.mcp_call.failed":                     true,
	"response.mcp_call.in_progress":                true,
	"response.mcp_list_tools.completed":            true,
	"response.mcp_list_tools.failed":               true,
	"response.mcp_list_tools.in_progress":          true,
	"response.code_interpreter_call.in_progress":   true,
	"response.code_interpreter_call.interpreting":  true,
	"response.code_interpreter_call.completed":     true,
	"response.code_interpreter_call_code.delta":    true,
	"response.code_interpreter_call_code.done":     true,
	"response.output_text.annotation.added":        true,
	"response.queued":                              true,
	"response.custom_tool_call_input.delta":        true,
	"response.custom_tool_call_input.done":         true,
	"error":                                        true,
}

var responsesClientInterceptedEventTypes = map[string]bool{
	"response.doom_loop_check": true,
}

type sseFilterStats struct {
	droppedUnknown int
	droppedPings   int
	renamedLegacy  int
}

func (s sseFilterStats) isZero() bool {
	return s.droppedUnknown == 0 && s.droppedPings == 0 && s.renamedLegacy == 0
}

func (s sseFilterStats) String() string {
	return fmt.Sprintf("dropped_unknown=%d dropped_pings=%d renamed_legacy=%d",
		s.droppedUnknown, s.droppedPings, s.renamedLegacy)
}

type streamStatsReporter interface {
	stats() sseFilterStats
}

// sanitizeResponsesRequest 清洗 Grok Build 的 Responses 请求：
// 剥离 stream_tool_calls、非标准 tools、非标准 include。
//
// 采用增量字节编辑而非 map 重序列化：客户端正文的键序、空白、数字字面量与
// 字符串转义全部原样保留，只有被修改的成员被触碰。这让上游收到的正文与
// 客户端发出的正文可直接对照（审计、diff、以及不重排任何对键序敏感的上游）。
func sanitizeResponsesRequest(body []byte) ([]byte, error) {
	hasStreamToolCalls := bytes.Contains(body, []byte(`"stream_tool_calls"`))
	hasTools := bytes.Contains(body, []byte(`"tools"`))
	hasInclude := bytes.Contains(body, []byte(`"include"`))
	if !hasStreamToolCalls && !hasTools && !hasInclude {
		return body, nil
	}

	// 探测（只读）：确认是 JSON 对象并取出原值供过滤函数使用。
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return body, nil
	}

	var edits []topLevelMemberEdit
	if _, ok := payload["stream_tool_calls"]; ok {
		edits = append(edits, topLevelMemberEdit{key: "stream_tool_calls", delete: true})
	}
	if raw, ok := payload["tools"]; ok {
		if cleaned, toolsChanged := sanitizeResponsesTools(raw); toolsChanged {
			// cleaned 为 nil 表示全部被过滤：删键而非留 "tools": []，
			// 部分上游会把空数组判为显式请求"不包含任何内容"，语义上等同于不传。
			edits = append(edits, topLevelMemberEdit{key: "tools", delete: cleaned == nil, value: cleaned})
		}
	}
	if raw, ok := payload["include"]; ok {
		if cleaned, includeChanged := sanitizeResponsesInclude(raw); includeChanged {
			edits = append(edits, topLevelMemberEdit{key: "include", delete: cleaned == nil, value: cleaned})
		}
	}

	out, changed := applyTopLevelMemberEdits(body, edits)
	if !changed {
		return body, nil
	}
	return out, nil
}

type webSearchFilters struct {
	ExcludedDomains []string `json:"excluded_domains"`
}

type toolTypeProbe struct {
	Type    string            `json:"type"`
	Filters *webSearchFilters `json:"filters"`
}

// sanitizeResponsesTools 过滤非标准工具类型并翻译 web_search 的域名过滤字段。
// 返回 (nil, true) 表示所有条目都被过滤，调用方应直接删掉 tools 键。
func sanitizeResponsesTools(raw json.RawMessage) (json.RawMessage, bool) {
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return raw, false
	}
	kept := make([]json.RawMessage, 0, len(entries))
	changed := false
	for _, entry := range entries {
		var probe toolTypeProbe
		if err := json.Unmarshal(entry, &probe); err != nil {

			kept = append(kept, entry)
			continue
		}
		if !standardResponsesToolTypes[probe.Type] {
			changed = true
			continue
		}
		if probe.Type == "web_search" && probe.Filters != nil && len(probe.Filters.ExcludedDomains) > 0 {
			if renamed, ok := renameExcludedDomains(entry); ok {
				kept = append(kept, renamed)
				changed = true
				continue
			}
		}
		kept = append(kept, entry)
	}
	if !changed {
		return raw, false
	}
	if len(kept) == 0 {
		return nil, true
	}
	out, err := marshalJSONNoEscape(kept)
	if err != nil {
		return raw, false
	}
	return out, true
}

func renameExcludedDomains(entry json.RawMessage) (json.RawMessage, bool) {
	var tool map[string]json.RawMessage
	if err := json.Unmarshal(entry, &tool); err != nil {
		return nil, false
	}
	filtersRaw, ok := tool["filters"]
	if !ok {
		return nil, false
	}
	var filters map[string]json.RawMessage
	if err := json.Unmarshal(filtersRaw, &filters); err != nil {
		return nil, false
	}
	excluded, hasExcluded := filters["excluded_domains"]
	if !hasExcluded {
		return nil, false
	}
	delete(filters, "excluded_domains")
	filters["blocked_domains"] = excluded
	renamedFilters, err := marshalJSONNoEscape(filters)
	if err != nil {
		return nil, false
	}
	tool["filters"] = renamedFilters
	out, err := marshalJSONNoEscape(tool)
	if err != nil {
		return nil, false
	}
	return out, true
}

// sanitizeResponsesInclude 过滤非标准的 include 值。
// 返回 (nil, true) 表示所有值都被过滤，调用方应直接删掉 include 键。
func sanitizeResponsesInclude(raw json.RawMessage) (json.RawMessage, bool) {
	var values []string
	if err := json.Unmarshal(raw, &values); err != nil {
		return raw, false
	}
	kept := make([]string, 0, len(values))
	changed := false
	for _, value := range values {
		if standardResponsesIncludeValues[value] {
			kept = append(kept, value)
		} else {
			changed = true
		}
	}
	if !changed {
		return raw, false
	}
	if len(kept) == 0 {
		return nil, true
	}
	out, err := marshalJSONNoEscape(kept)
	if err != nil {
		return raw, false
	}
	return out, true
}

func isUnknownResponsesEventPayload(payload []byte) bool {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 {
		return false
	}
	if trimmed[0] == '{' {
		var probe struct {
			Type string `json:"type"`
		}

		if err := json.Unmarshal(trimmed, &probe); err != nil {
			return true
		}

		return !responsesStreamEventTypes[probe.Type] &&
			!responsesClientInterceptedEventTypes[probe.Type]
	}

	var value any
	if err := json.Unmarshal(trimmed, &value); err != nil {
		return false
	}
	_, isObject := value.(map[string]any)
	return !isObject
}

func newResponsesSSEFilter(reader io.Reader) io.Reader {
	stats := &sseFilterStats{}
	return newSSELineTransformer(reader, func(line []byte) []byte {
		renamed := rewriteLegacyReasoningEventNames(line)

		if !bytes.Equal(renamed, line) && dataLinePayload(bytes.TrimRight(line, "\r\n")) != nil {
			stats.renamedLegacy++
		}
		return renamed
	}, stats, stats.isPingPayload, stats.dropUnknownResponsesEvent)
}

func (s *sseFilterStats) dropUnknownResponsesEvent(payload []byte) bool {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 {
		slog.Debug("dropping empty SSE payload")
		return true
	}
	if isUnknownResponsesEventPayload(trimmed) {
		preview := trimmed
		if len(preview) > 200 {
			preview = preview[:200]
		}
		slog.Debug("dropping unknown SSE event", "payload", string(preview))
		return true
	}
	return false
}
