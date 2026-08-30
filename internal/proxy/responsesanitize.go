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
// (`x_search`), and extra `include` values such as `no_inline_citations`. None
// of these exist in the standard protocol, so a conformant upstream can reject
// the request outright.
//
// `web_search.filters.excluded_domains` is deliberately not in that list: the
// standard protocol spells the same capability `blocked_domains`
// (openai-4.0.51/package/src/responses/openai-responses-api.ts#OpenAIResponsesTool),
// so the key is renamed instead of dropped, which keeps the caller's intended
// search scope intact for an upstream that implements it.
//
// The request sanitizer strips exactly those extensions and returns the body
// byte-for-byte when nothing offending is present. The SSE filter drops
// events outside Grok Build's typed event vocabulary (keepalive pings and
// newer standard event types its parser cannot deserialize — an unparseable
// frame fails the whole stream client-side).
//
// Whitelists in this file are derived from the canonical specs:
//   - Tool types: openai-4.0.51/package/src/responses/openai-responses-api.ts#OpenAIResponsesTool
//                 + async-openai/src/types/responses/response.rs#Tool
//   - Include values: openai-4.0.51/package/src/responses/openai-responses-api.ts#OpenAIResponsesIncludeValue
//                 = async-openai/src/types/responses/response.rs#IncludeEnum
//   - Stream events: async-openai/src/types/responses/stream.rs#ResponseStreamEvent
//   - Client-intercepted events: crates/codegen/xai-grok-sampler/src/client.rs
//     (raw-JSON hooks that run before typed deserialization)
//
// These lists are hand-maintained against the sources above; there is no
// generator. Two things to know when bumping dependencies:
//
//   - grok-build resolves async-openai from a fork (our-forks/async-openai.git
//     pinned by Cargo.lock), not from the upstream checkout these paths refer
//     to. If the fork adds event variants, they belong in
//     responsesStreamEventTypes.
//   - The two specs drift apart over time. The tool list is the union of both;
//     the include and stream-event lists have matched exactly so far. Re-diff
//     rather than assuming.

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

// responsesClientInterceptedEventTypes are events Grok Build consumes through
// raw-JSON hooks that run *before* typed deserialization, so they never reach
// the ResponseStreamEvent enum. Unlike the set above, these must be forwarded
// even though the typed parser cannot deserialize them: the hook swallows them
// before parsing, so forwarding is safe, whereas dropping them silently
// disables the feature behind the hook without any error.
//
//   - response.doom_loop_check: consumed by is_check_event /
//     DoomLoopCollector::absorb (crates/codegen/xai-grok-sampler/src/client.rs),
//     which matches either the SSE `event:` name or the payload `type` and
//     runs whether or not doom-loop recovery is enabled. Dropping it disabled
//     doom-loop recovery silently.
var responsesClientInterceptedEventTypes = map[string]bool{
	"response.doom_loop_check": true,
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

// sanitizeMetricsDelta reports what accumulated since `before`. The counters
// are process-wide, so per-stream observability has to diff them: logging the
// raw snapshots would print a forever-growing total on every stream once
// anything had ever been filtered. Counters that did not move are omitted.
func sanitizeMetricsDelta(before map[string]int64) map[string]int64 {
	after := sanitizeMetrics()
	delta := make(map[string]int64, len(after))
	for key, value := range after {
		if moved := value - before[key]; moved > 0 {
			delta[key] = moved
		}
	}
	return delta
}

// sanitizeResponsesRequest strips xAI-only extensions from a Responses
// request body so it conforms to the standard protocol. Bodies without any
// offending field are returned byte-for-byte.
// Optimization: fast-path byte checks before JSON parsing to avoid allocations.
func sanitizeResponsesRequest(body []byte) ([]byte, error) {
	// Fast path: if none of the xAI-only markers are present, avoid JSON parse
	// entirely. This keeps conformant bodies byte-identical and zero-alloc.
	//
	// Each marker is matched with its surrounding quotes so only a real member
	// name counts. An unquoted search also matches the same text inside a
	// value — a user message talking about `stream_tool_calls` — and would
	// then parse and reserialize a body that needs no changes.
	hasStreamToolCalls := bytes.Contains(body, []byte(`"stream_tool_calls"`))
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
	return marshalJSONNoEscape(payload)
}

// webSearchFilters reads only the keys the sanitizer acts on. The rest of a
// `filters` object (e.g. `allowed_domains`) is never touched: rewriting
// happens on the raw JSON, so unknown keys survive untouched.
type webSearchFilters struct {
	ExcludedDomains []string `json:"excluded_domains"`
}

type toolTypeProbe struct {
	Type    string            `json:"type"`
	Filters *webSearchFilters `json:"filters"`
}

// sanitizeResponsesTools removes tool entries outside the standard vocabulary
// and reports whether anything changed. A `web_search` tool carrying the xAI
// `filters.excluded_domains` key is not removed: the standard protocol spells
// the same capability `blocked_domains`, so the key is renamed and the tool
// kept, preserving the caller's intended search scope. Dropping the tool
// instead would silently widen the search to unbounded.
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
	out, err := marshalJSONNoEscape(kept)
	if err != nil {
		return raw, false
	}
	return out, true
}

// renameExcludedDomains renames the xAI `filters.excluded_domains` key of a
// web_search tool to the standard `filters.blocked_domains`, preserving every
// other field. It reports false only when the entry does not carry the key,
// in which case it must be forwarded untouched.
//
// A tool that spells the capability both ways is resolved in favour of
// `excluded_domains`, the only spelling the caller (Grok Build) emits; the
// standard spelling is then no longer reachable from its request, so the
// merged value is the one the caller actually asked for.
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
	out, err := marshalJSONNoEscape(kept)
	if err != nil {
		return raw, false
	}
	return out, true
}

// isUnknownResponsesEventPayload reports whether a `data:` payload is a frame
// the client cannot deserialize, i.e. one that would fail the whole stream:
// a JSON object whose `type` is absent or outside the known vocabularies, a
// valid JSON value that is not an object (async-openai's ResponseStreamEvent
// is a `#[serde(tag = "type")]` enum with no untagged or catch-all variant, so
// `null`, arrays, and scalars all fail it), or a malformed object.
//
// Non-JSON payloads pass through: `ping` and `[DONE]` are legitimate SSE
// protocol elements handled by the client's SSE decoder before any typed
// parsing, and the filter is only ever attached to Responses streams (the
// chat-completions gateway uses its own transform with no drop predicate).
func isUnknownResponsesEventPayload(payload []byte) bool {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 {
		return false
	}
	if trimmed[0] == '{' {
		var probe struct {
			Type string `json:"type"`
		}
		// The Unmarshal happens anyway for classification, so rejecting
		// malformed objects here costs nothing extra: the client cannot parse
		// them either, and forwarding one would fail the entire stream.
		if err := json.Unmarshal(trimmed, &probe); err != nil {
			return true
		}
		// probe.Type is "" when the object carries no `type`, which matches
		// neither vocabulary and is therefore dropped.
		return !responsesStreamEventTypes[probe.Type] &&
			!responsesClientInterceptedEventTypes[probe.Type]
	}
	// Everything else that IS valid JSON but not an object is equally
	// undeserializable. The scan is cheap in practice: pings are classified by
	// isPingPayload before this runs, so per stream only the single [DONE]
	// sentinel reaches here — and it fails the Unmarshal, passing through.
	var value any
	if err := json.Unmarshal(trimmed, &value); err != nil {
		return false
	}
	_, isObject := value.(map[string]any)
	return !isObject
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
