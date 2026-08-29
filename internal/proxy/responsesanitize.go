package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"sync/atomic"
)

// responsesanitize.go aligns Grok Build's xAI-flavored Responses traffic with
// the standard OpenAI Responses protocol. Grok Build serializes typed request
// fields from an enum of standard vocabulary, but adds xAI-only extensions on
// top: the backend-only `stream_tool_calls` option, raw-JSON hosted tools
// (`x_search`, and a `web_search` entry whose filters can carry
// `excluded_domains`), and extra `include` values such as
// `no_inline_citations`. None of these exist in the standard protocol, so a
// conformant upstream can reject the request outright.
//
// The request sanitizer strips exactly those extensions and returns the body
// byte-for-byte when nothing offending is present. The SSE filter drops
// events outside Grok Build's typed event vocabulary (keepalive pings and
// newer standard event types its parser cannot deserialize — an unparseable
// frame fails the whole stream client-side).
//
// Whitelists in this file are derived from the canonical specs:
//   - Tool types: openai-4.0.51/src/responses/openai-responses-api.ts#OpenAIResponsesTool
//                 + async-openai/src/types/responses/{api,response}.rs#Tool
//   - Include values: openai-4.0.51/src/responses/openai-responses-api.ts#OpenAIResponsesIncludeValue
//                 = async-openai/src/types/responses/api.rs#IncludeEnum
//   - Stream events: async-openai/src/types/responses/stream.rs#ResponseStreamEvent
//                 (grok-build's openai-responses-api.ts#openaiResponsesChunkSchema lags behind;
//                 unknown events are dropped to avoid client deserialization failure).
// Keep these in sync via `go:generate` from openapi.yaml or the upstream crates.

//go:generate go run ./gen/sanitize -openapi ../../openapi.yaml -out responsesanitize_gen.go

// standardResponsesToolTypes is the tool `type` vocabulary of the standard
// Responses protocol (mirroring the Tool enum Grok Build itself serializes
// from). Entries with any other type are xAI-only and are stripped.
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

// standardResponsesIncludeValues is the standard `include` vocabulary
// (mirroring the Include enum Grok Build serializes from). Configured extras
// outside this set (e.g. the xAI-only `no_inline_citations`) are stripped.
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

// responsesStreamEventTypes is the SSE event vocabulary Grok Build's typed
// parser understands (the ResponseStreamEvent enum of its async-openai
// types, which lags the current standard — e.g. it has no
// response.apply_patch_call_* events). Frames with any other type are
// dropped instead of forwarded, because a single unparseable frame fails
// the entire stream client-side.
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

// Observability counters for SSE filtering - exported for metrics.
var (
	droppedUnknownEventCount atomic.Int64
	droppedPingCount         atomic.Int64
	renamedLegacyEventCount  atomic.Int64
)

// sanitizeMetrics returns snapshot of filtering counters for observability.
func sanitizeMetrics() map[string]int64 {
	return map[string]int64{
		"dropped_unknown_events": droppedUnknownEventCount.Load(),
		"dropped_pings":          droppedPingCount.Load(),
		"renamed_legacy_events":  renamedLegacyEventCount.Load(),
	}
}

// sanitizeResponsesRequest strips xAI-only extensions from a Responses
// request body so it conforms to the standard protocol. Bodies without any
// offending field are returned byte-for-byte.
// Optimization: fast-path byte checks before JSON parsing to avoid allocations.
func sanitizeResponsesRequest(body []byte) ([]byte, error) {
	// Fast path: if none of the xAI-only markers are present, avoid JSON parse entirely.
	// This keeps conformant bodies byte-identical and zero-alloc.
	hasStreamToolCalls := bytes.Contains(body, []byte("stream_tool_calls"))
	hasTools := bytes.Contains(body, []byte(`"tools"`))
	hasInclude := bytes.Contains(body, []byte(`"include"`))
	if !hasStreamToolCalls && !hasTools && !hasInclude {
		return body, nil
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		// Not a JSON object; leave it for the upstream to reject.
		return body, nil
	}
	changed := false
	if _, ok := payload["stream_tool_calls"]; ok {
		delete(payload, "stream_tool_calls")
		changed = true
	}
	if hasTools {
		if raw, ok := payload["tools"]; ok {
			if cleaned, toolsChanged := sanitizeResponsesTools(raw); toolsChanged {
				payload["tools"] = cleaned
				changed = true
			}
		}
	}
	if hasInclude {
		if raw, ok := payload["include"]; ok {
			if cleaned, includeChanged := sanitizeResponsesInclude(raw); includeChanged {
				payload["include"] = cleaned
				changed = true
			}
		}
	}
	if !changed {
		return body, nil
	}
	return json.Marshal(payload)
}

type webSearchFilters struct {
	AllowedDomains  []string `json:"allowed_domains"`
	ExcludedDomains []string `json:"excluded_domains"`
}

type toolTypeProbe struct {
	Type    string            `json:"type"`
	Filters *webSearchFilters `json:"filters"`
}

// sanitizeResponsesTools removes tool entries outside the standard vocabulary
// and reports whether anything changed. For `web_search` the standard tool
// has no `excluded_domains` filter: instead of dropping the entire tool and
// silently widening the search, we now strip only the excluded_domains key
// and keep the allowed_domains part. This preserves the user's intended
// search scope while staying conformant.
func sanitizeResponsesTools(raw json.RawMessage) (json.RawMessage, bool) {
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return raw, false
	}
	kept := make([]json.RawMessage, 0, len(entries))
	changed := false
	for _, entry := range entries {
		var probe toolTypeProbe
		if err := json.Unmarshal(entry, &probe); err != nil || !standardResponsesToolTypes[probe.Type] {
			changed = true
			continue
		}
		if probe.Type == "web_search" && probe.Filters != nil && len(probe.Filters.ExcludedDomains) > 0 {
			// Optimization 2: if the tool has allowed_domains, strip only excluded_domains and keep;
			// if it only has excluded_domains, drop the whole tool to avoid silently widening to unbounded search.
			if len(probe.Filters.AllowedDomains) > 0 {
				if cleaned, ok := stripExcludedDomains(entry); ok {
					kept = append(kept, cleaned)
					changed = true
					continue
				}
			}
			// Pure excluded_domains -> drop (original safe behavior)
			changed = true
			continue
		}
		kept = append(kept, entry)
	}
	if !changed {
		return raw, false
	}
	out, err := json.Marshal(kept)
	if err != nil {
		return raw, false
	}
	return out, true
}

// stripExcludedDomains removes the excluded_domains field from a web_search tool entry
// while preserving allowed_domains and other fields. Returns the cleaned JSON and true on success.
func stripExcludedDomains(entry json.RawMessage) (json.RawMessage, bool) {
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
	if _, hasExcluded := filters["excluded_domains"]; !hasExcluded {
		return nil, false
	}
	delete(filters, "excluded_domains")
	// If filters becomes empty, remove it entirely to stay conformant (unbounded search).
	if len(filters) == 0 {
		delete(tool, "filters")
	} else {
		cleanedFilters, err := json.Marshal(filters)
		if err != nil {
			return nil, false
		}
		tool["filters"] = cleanedFilters
	}
	out, err := json.Marshal(tool)
	if err != nil {
		return nil, false
	}
	return out, true
}

// sanitizeResponsesInclude removes `include` values outside the standard
// vocabulary and reports whether anything changed.
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
	out, err := json.Marshal(kept)
	if err != nil {
		return raw, false
	}
	return out, true
}

// isUnknownResponsesEventPayload reports whether a `data:` payload carries a
// JSON `type` outside the known event vocabulary. Non-JSON and typeless
// payloads pass through: the proxy does not second-guess frames it cannot
// classify, and the client fails on them no differently than it would have
// without the proxy.
func isUnknownResponsesEventPayload(payload []byte) bool {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 {
		return false
	}
	// Fast path: short-circuit non-JSON payloads (e.g. "ping", "[DONE]")
	if trimmed[0] != '{' {
		return false
	}
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(trimmed, &probe); err != nil {
		return false
	}
	return probe.Type != "" && !responsesStreamEventTypes[probe.Type]
}

// newResponsesSSEFilter translates the upstream stream into the event
// vocabulary Grok Build's parser understands: legacy reasoning event names
// are renamed to their standard reasoning_text equivalents, keepalive pings
// are dropped, and events outside the known vocabulary are dropped.
func newResponsesSSEFilter(reader io.Reader) io.Reader {
	return newSSELineTransformer(reader, rewriteLegacyReasoningEventNames, isVercelPing, dropUnknownResponsesEvent)
}

// dropUnknownResponsesEvent decides on the renamed payload so the legacy
// reasoning event names pass the vocabulary check that follows the rename.
// The rename is idempotent, so applying it here and again at write time is
// safe. An empty `data:` frame is dropped as well: it cannot be parsed by
// the client and would fail the whole stream.
func dropUnknownResponsesEvent(payload []byte) bool {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 {
		droppedUnknownEventCount.Add(1)
		slog.Debug("dropping empty SSE payload")
		return true
	}
	// Single Unmarshal path: rename first so legacy events pass.
	renamed := rewriteLegacyReasoningEventNames(payload)
	if !bytes.Equal(renamed, payload) {
		renamedLegacyEventCount.Add(1)
	}
	if isUnknownResponsesEventPayload(renamed) {
		droppedUnknownEventCount.Add(1)
		preview := trimmed
		if len(preview) > 200 {
			preview = preview[:200]
		}
		slog.Debug("dropping unknown SSE event", "payload", string(preview))
		return true
	}
	return false
}
