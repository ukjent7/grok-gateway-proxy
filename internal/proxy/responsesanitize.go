package proxy

import (
	"bytes"
	"encoding/json"
	"io"
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

// sanitizeResponsesRequest strips xAI-only extensions from a Responses
// request body so it conforms to the standard protocol. Bodies without any
// offending field are returned byte-for-byte.
func sanitizeResponsesRequest(body []byte) ([]byte, error) {
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
	if raw, ok := payload["tools"]; ok {
		if cleaned, toolsChanged := sanitizeResponsesTools(raw); toolsChanged {
			payload["tools"] = cleaned
			changed = true
		}
	}
	if raw, ok := payload["include"]; ok {
		if cleaned, includeChanged := sanitizeResponsesInclude(raw); includeChanged {
			payload["include"] = cleaned
			changed = true
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
// and reports whether anything changed. A `web_search` entry configured with
// `excluded_domains` is dropped entirely: the standard tool has no exclusion
// filter, and silently widening the search would defeat the user's configured
// blocklist.
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
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(payload), &probe); err != nil {
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
	if len(bytes.TrimSpace(payload)) == 0 {
		return true
	}
	return isUnknownResponsesEventPayload(rewriteLegacyReasoningEventNames(payload))
}
