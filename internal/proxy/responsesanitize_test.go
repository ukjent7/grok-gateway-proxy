package proxy

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"

	"grok-gateway-proxy/internal/config"
)

func TestResponsesFilterKeepsTerminatorAfterDroppedEvent(t *testing.T) {
	for _, input := range []string{

		"data: ping\ndata: {\"type\":\"response.completed\"}\n\n",

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

		{`{"type":"response.created`, true},
		{`not json`, false},

		{`[1,2]`, true},
		{`null`, true},

		{`ping`, false},
		{`[DONE]`, false},
	}
	for _, c := range cases {
		if got := isUnknownResponsesEventPayload([]byte(c.payload)); got != c.unknown {
			t.Fatalf("isUnknownResponsesEventPayload(%s) = %v, want %v", c.payload, got, c.unknown)
		}
	}
}

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

	if strings.Contains(string(out), "no_inline_citations") {
		t.Fatalf("non-standard include survived: %s", out)
	}
}

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

func TestSanitizeResponsesRequestKeepsToolEntriesItCannotParse(t *testing.T) {
	for _, body := range []string{
		`{"model":"m","tools":["web_search"]}`,
		`{"model":"m","tools":[{"type":"web_search","filters":"none"}]}`,
	} {
		got, err := sanitizeResponsesRequest([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(got, []byte("web_search")) {
			t.Fatalf("declared tool was silently dropped: %s -> %s", body, got)
		}
	}

	got, err := sanitizeResponsesRequest([]byte(`{"model":"m","tools":[{"type":"x_search"},{"type":"function","name":"f"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(got, []byte("x_search")) {
		t.Fatalf("non-standard tool survived: %s", got)
	}
	if !bytes.Contains(got, []byte(`"name":"f"`)) {
		t.Fatalf("function tool was lost: %s", got)
	}
}

func TestSSEFilterStatsArePerStream(t *testing.T) {
	stream := "data: ping\n\n" +
		"event: response.reasoning.delta\ndata: {\"type\":\"response.reasoning.delta\",\"delta\":\"x\"}\n\n" +
		"data: {\"type\":\"response.not_a_real_event\"}\n\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"y\"}\n\n"

	counts := func() sseFilterStats {
		reader := newResponsesSSEFilter(strings.NewReader(stream))
		filter, ok := reader.(streamStatsReporter)
		if !ok {
			t.Fatal("responses SSE filter reports no per-stream stats")
		}
		out, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(out, []byte("response.not_a_real_event")) {
			t.Fatalf("unknown event survived: %s", out)
		}
		return filter.stats()
	}

	first := counts()
	if first.droppedPings != 1 || first.droppedUnknown != 1 || first.renamedLegacy != 1 {
		t.Fatalf("unexpected first-stream tally: %+v", first)
	}
	if second := counts(); second != first {
		t.Fatalf("a new stream inherited earlier counts: %+v vs %+v", second, first)
	}
	if first.isZero() {
		t.Fatal("a stream that filtered three events reported no activity")
	}
}

func TestSSETwoLineFramingDropsEventAndData(t *testing.T) {
	stream := "" +
		"event: response.created\ndata: {\"type\":\"response.created\"}\n\n" +
		"event: response.unknown_foobar\ndata: {\"type\":\"response.unknown_foobar\",\"delta\":\"x\"}\n\n" +
		"data: \n\n" +
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n" +
		"data: [DONE]\n\n"
	out, err := io.ReadAll(newResponsesSSEFilter(strings.NewReader(stream)))
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if strings.Contains(got, "unknown_foobar") {
		t.Fatalf("unknown two-line frame survived: %q", got)
	}
	if strings.Contains(got, "data: \n") {
		t.Fatalf("empty data frame survived: %q", got)
	}
	if !strings.Contains(got, "response.created") || !strings.Contains(got, "response.output_text.delta") {
		t.Fatalf("valid framing events were lost: %q", got)
	}
	if !strings.Contains(got, "[DONE]") {
		t.Fatalf("[DONE] sentinel lost: %q", got)
	}

	reader := newResponsesSSEFilter(strings.NewReader(stream))
	filter, ok := reader.(streamStatsReporter)
	if !ok {
		t.Fatal("filter has no stats")
	}
	if _, err := io.ReadAll(reader); err != nil {
		t.Fatal(err)
	}
	stats := filter.stats()
	if stats.droppedUnknown < 2 {
		t.Fatalf("expected at least 2 droppedUnknown (unknown+empty), got %+v", stats)
	}
}

func TestBuildUpstreamHeadersSessionAffinityOff(t *testing.T) {
	src := http.Header{}
	src.Set("X-Grok-Session-Id", "sess-999")
	src.Set("Authorization", "Bearer tok")
	dst := http.Header{}
	buildUpstreamHeaders(dst, src, config.GatewayConfig{SessionAffinity: config.SessionAffinityOff}, "req-1", false)
	if got := dst.Get("session_id"); got != "" {
		t.Fatalf("Off mode leaked session_id %q", got)
	}
	if got := dst.Get("X-Client-Request-Id"); got != "" {
		t.Fatalf("Off mode leaked X-Client-Request-Id %q", got)
	}
	if got := dst.Get("X-Session-Id"); got != "" {
		t.Fatalf("Off mode leaked X-Session-Id %q", got)
	}
	if got := dst.Get("Authorization"); got != "Bearer tok" {
		t.Fatalf("allowlisted Authorization lost in Off mode: %q", got)
	}
}
