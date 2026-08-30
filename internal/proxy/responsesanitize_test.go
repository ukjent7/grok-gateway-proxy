package proxy

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// Dropping an event must not swallow the blank line that terminates a *kept*
// event. Without that terminator the client never dispatches the kept event,
// so a dropped ping silently takes the next real event down with it.
func TestResponsesFilterKeepsTerminatorAfterDroppedEvent(t *testing.T) {
	for _, input := range []string{
		// Dropped data line followed by a kept one in the same event block.
		"data: ping\ndata: {\"type\":\"response.completed\"}\n\n",
		// Dropped event, then a kept event introduced by its own event: line.
		"data: ping\nevent: response.completed\ndata: {\"type\":\"response.completed\"}\n\n",
	} {
		out, err := io.ReadAll(newResponsesSSEFilter(strings.NewReader(input)))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(string(out), "\n\n") {
			t.Fatalf("kept event lost its terminating blank line:\nwant suffix: %q\ngot:         %q", "\\n\\n", out)
		}
		if !strings.Contains(string(out), "response.completed") {
			t.Fatalf("kept event was dropped too: %q", out)
		}
	}
}

// The sanitizer must return conformant bodies byte-for-byte so standard
// clients pay no rewrite cost and logged request bytes stay comparable.
func TestSanitizeResponsesRequestNoopIsByteIdentical(t *testing.T) {
	bodies := []string{
		`{"model":"m","input":"hi"}`,
		`{"model":"m","tools":[{"type":"function","name":"lookup","parameters":{}}],"include":["reasoning.encrypted_content"]}`,
		`{"model":"m","tools":[{"type":"web_search","filters":{"allowed_domains":["docs.example"]}}]}`,
		`{"model":"m","tools":[]}`,
	}
	for _, body := range bodies {
		out, err := sanitizeResponsesRequest([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		if string(out) != body {
			t.Fatalf("conformant body was rewritten:\nwant: %s\ngot:  %s", body, out)
		}
	}
}

func TestSanitizeResponsesRequestStripsStreamToolCalls(t *testing.T) {
	out, err := sanitizeResponsesRequest([]byte(`{"model":"m","stream_tool_calls":true,"input":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "stream_tool_calls") {
		t.Fatalf("stream_tool_calls survived: %s", out)
	}
	if !strings.Contains(string(out), `"model":"m"`) {
		t.Fatalf("unrelated fields were lost: %s", out)
	}
}

func TestSanitizeResponsesRequestDropsXSearchTool(t *testing.T) {
	out, err := sanitizeResponsesRequest([]byte(`{"tools":[{"type":"x_search","from_date":"2026-01-01"},{"type":"function","name":"lookup","parameters":{}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "x_search") {
		t.Fatalf("x_search tool survived: %s", out)
	}
	if !strings.Contains(string(out), `"name":"lookup"`) {
		t.Fatalf("function tool was lost: %s", out)
	}
}

// The xAI `excluded_domains` filter is the standard `blocked_domains` under a
// different name, so it is renamed rather than dropped: dropping the tool
// would silently widen the caller's blocked-scope search to unbounded.
func TestSanitizeResponsesRequestRenamesExcludedDomainsToBlocked(t *testing.T) {
	out, err := sanitizeResponsesRequest([]byte(`{"tools":[{"type":"web_search","filters":{"excluded_domains":["evil.example"]}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "excluded_domains") {
		t.Fatalf("excluded_domains survived: %s", out)
	}
	if !strings.Contains(string(out), `"type":"web_search"`) {
		t.Fatalf("web_search tool was dropped: %s", out)
	}
	if !strings.Contains(string(out), `"blocked_domains":["evil.example"]`) {
		t.Fatalf("blocklist was not preserved as blocked_domains: %s", out)
	}
}

// Renaming must not disturb the sibling `allowed_domains` filter or any other
// field of the tool: the rewrite runs on raw JSON for exactly this reason.
func TestSanitizeResponsesRequestRenamesExcludedDomainsKeepsAllowed(t *testing.T) {
	out, err := sanitizeResponsesRequest([]byte(`{"tools":[{"type":"web_search","filters":{"allowed_domains":["docs.example"],"excluded_domains":["evil.example"]},"search_context_size":"low"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "excluded_domains") {
		t.Fatalf("excluded_domains survived: %s", out)
	}
	if !strings.Contains(string(out), `"allowed_domains":["docs.example"]`) {
		t.Fatalf("allowed_domains was lost: %s", out)
	}
	if !strings.Contains(string(out), `"blocked_domains":["evil.example"]`) {
		t.Fatalf("blocklist was not preserved as blocked_domains: %s", out)
	}
	if !strings.Contains(string(out), `"search_context_size":"low"`) {
		t.Fatalf("sibling tool field was lost: %s", out)
	}
	if !strings.Contains(string(out), `"type":"web_search"`) {
		t.Fatalf("web_search tool was lost: %s", out)
	}
}

// A body already using the standard spelling must pass through untouched.
func TestSanitizeResponsesRequestKeepsBlockedDomainWebSearch(t *testing.T) {
	body := `{"tools":[{"type":"web_search","filters":{"blocked_domains":["evil.example"]}}]}`
	out, err := sanitizeResponsesRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != body {
		t.Fatalf("blocked-domains web_search was changed:\nwant: %s\ngot:  %s", body, out)
	}
}

func TestSanitizeResponsesRequestKeepsAllowedDomainWebSearch(t *testing.T) {
	body := `{"tools":[{"type":"web_search","filters":{"allowed_domains":["docs.example"]}}]}`
	out, err := sanitizeResponsesRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != body {
		t.Fatalf("allowed-domains web_search was changed:\nwant: %s\ngot:  %s", body, out)
	}
}

// A bare web_search entry (no filters) is the common unbounded-search form
// and must pass through untouched — and not panic on the absent filters.
func TestSanitizeResponsesRequestKeepsBareWebSearch(t *testing.T) {
	body := `{"tools":[{"type":"web_search"},{"type":"x_search"}]}`
	out, err := sanitizeResponsesRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"type":"web_search"`) {
		t.Fatalf("bare web_search was lost: %s", out)
	}
	if strings.Contains(string(out), "x_search") {
		t.Fatalf("x_search survived: %s", out)
	}
}

func TestSanitizeResponsesRequestStripsNonStandardInclude(t *testing.T) {
	out, err := sanitizeResponsesRequest([]byte(`{"include":["reasoning.encrypted_content","no_inline_citations"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "no_inline_citations") {
		t.Fatalf("non-standard include value survived: %s", out)
	}
	if !strings.Contains(string(out), "reasoning.encrypted_content") {
		t.Fatalf("standard include value was lost: %s", out)
	}
}

// Unparseable non-JSON payloads pass through, because `ping` and `[DONE]` are
// SSE protocol elements the client's decoder handles before any typed parsing.
//
// Everything the client cannot deserialize is dropped: a JSON object with no
// or unknown `type`, a malformed object, and valid JSON that is not an object.
// async-openai's ResponseStreamEvent is a `#[serde(tag = "type")]` enum with
// no untagged or catch-all variant, so all of those fail the stream exactly
// like an unknown type does.
//
// Events in responsesClientInterceptedEventTypes are not unknown: grok-build
// consumes them through raw-JSON hooks ahead of typed deserialization.
func TestUnknownResponsesEventPayloadClassification(t *testing.T) {
	cases := []struct {
		payload string
		unknown bool
	}{
		{`{"type":"response.output_text.delta","delta":"hi"}`, false},
		{`{"type":"response.apply_patch_call_operation_diff.delta","delta":"x"}`, true},
		{`{"type":"response.doom_loop_check","doom_loop_check":{}}`, false},
		{`{"delta":"no type field"}`, true},
		{`{}`, true},
		// A malformed object cannot be deserialized by the client either, so
		// forwarding it would fail the entire stream where dropping keeps it
		// alive.
		{`{"type":"response.created`, true},
		{`not json`, false},
		// Valid JSON that is not an object fails the tagged-enum
		// deserialization just like an unknown type does.
		{`[1,2]`, true},
		{`null`, true},
		// SSE protocol elements must keep passing through.
		{`ping`, false},
		{`[DONE]`, false},
	}
	for _, c := range cases {
		if got := isUnknownResponsesEventPayload([]byte(c.payload)); got != c.unknown {
			t.Fatalf("isUnknownResponsesEventPayload(%s) = %v, want %v", c.payload, got, c.unknown)
		}
	}
}

// The doom-loop check frame must survive the whole filter, `event:` line
// included: grok-build's is_check_event matches either the `event:` name or
// the payload `type`, and the filter buffers the `event:` line separately
// from the `data:` line that decides whether the frame is dropped.
func TestResponsesSSEFilterForwardsDoomLoopCheckEvent(t *testing.T) {
	stream := "event: response.doom_loop_check\n" +
		`data: {"type":"response.doom_loop_check","doom_loop_check":{"triggers":["tail_repetition:4@response"]}}` + "\n\n" +
		"event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","delta":"hi"}` + "\n\n"

	out, err := io.ReadAll(newResponsesSSEFilter(bytes.NewReader([]byte(stream))))
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if !strings.Contains(got, "response.doom_loop_check") {
		t.Fatalf("doom-loop check event was dropped: %q", got)
	}
	if !strings.Contains(got, "tail_repetition") {
		t.Fatalf("doom-loop triggers were lost: %q", got)
	}
	if !strings.Contains(got, "response.output_text.delta") {
		t.Fatalf("unrelated event was dropped: %q", got)
	}
}

// Rewriting a body must not HTML-escape it: `<`, `>` and `&` are everywhere in
// source code, and escaping them triples those bytes while making the logged
// upstream body diverge from what the client sent.
func TestSanitizeResponsesRequestDoesNotHTMLEscape(t *testing.T) {
	body := []byte(`{"model":"m","input":"if (a < b && c > d) { f(&x); }","include":["no_inline_citations"]}`)
	out, err := sanitizeResponsesRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	for _, escaped := range []string{`\u003c`, `\u003e`, `\u0026`} {
		if strings.Contains(string(out), escaped) {
			t.Fatalf("body was HTML-escaped (%s present): %s", escaped, out)
		}
	}
	if !strings.Contains(string(out), "a < b && c > d") {
		t.Fatalf("source-code characters were mangled: %s", out)
	}
	// The rewrite still has to happen.
	if strings.Contains(string(out), "no_inline_citations") {
		t.Fatalf("non-standard include survived: %s", out)
	}
}

// Same guarantee for the DeepSeek cleanups, which rewrite `input` items.
func TestDeepSeekAdaptationDoesNotHTMLEscape(t *testing.T) {
	body := []byte(`{"model":"m","input":[{"type":"reasoning","content":"a<b && c>d","summary":[{"type":"summary_text","text":"s"}]}],"include":["no_inline_citations"]}`)
	out, err := adaptResponsesRequestForDeepSeek([]byte(sanitizeIgnoreError(t, body)))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), `\u003c`) || strings.Contains(string(out), `\u0026`) {
		t.Fatalf("body was HTML-escaped: %s", out)
	}
	if !strings.Contains(string(out), "a<b && c>d") {
		t.Fatalf("reasoning content was mangled: %s", out)
	}
	if strings.Contains(string(out), "summary") {
		t.Fatalf("unsupported summary field survived: %s", out)
	}
}

func sanitizeIgnoreError(t *testing.T, body []byte) string {
	t.Helper()
	out, err := sanitizeResponsesRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// The fast-path markers must match member names only. An unquoted
// "stream_tool_calls" search also matches the same text inside a value — a
// user message asking about the option — which would parse and reserialize a
// body that needs no changes at all.
func TestSanitizeResponsesRequestFastPathIgnoresMarkerInsideValue(t *testing.T) {
	body := []byte(`{"model":"deepseek-chat","input":"what does stream_tool_calls do?"}`)
	got, err := sanitizeResponsesRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("body was rewritten: got %s", got)
	}
	allocs := testing.AllocsPerRun(20, func() {
		if _, err := sanitizeResponsesRequest(body); err != nil {
			t.Fatal(err)
		}
	})
	if allocs > 0 {
		t.Fatalf("fast path allocated %v objects per call for a conformant body", allocs)
	}
}

// A real stream_tool_calls member must still be stripped, whatever its
// position or the surrounding formatting.
func TestSanitizeResponsesRequestStripsStreamToolCallsWhenSpaced(t *testing.T) {
	body := []byte(`{"model": "deepseek-chat", "stream_tool_calls" : true, "input": []}`)
	got, err := sanitizeResponsesRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(got, []byte("stream_tool_calls")) {
		t.Fatalf("stream_tool_calls survived: %s", got)
	}
}
