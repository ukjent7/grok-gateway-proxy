package proxy

import (
	"strings"
	"testing"
)

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

// A web_search entry with excluded_domains cannot be expressed in the
// standard protocol; it is dropped instead of silently widened.
func TestSanitizeResponsesRequestDropsExcludedDomainWebSearch(t *testing.T) {
	out, err := sanitizeResponsesRequest([]byte(`{"tools":[{"type":"web_search","filters":{"excluded_domains":["evil.example"]}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"tools":[]`) {
		t.Fatalf("excluded-domains web_search was not dropped: %s", out)
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

// Unknown event types are dropped; non-JSON and typeless payloads pass
// through so the proxy never fabricates protocol decisions it cannot make.
func TestUnknownResponsesEventPayloadClassification(t *testing.T) {
	cases := []struct {
		payload string
		unknown bool
	}{
		{`{"type":"response.output_text.delta","delta":"hi"}`, false},
		{`{"type":"response.apply_patch_call_operation_diff.delta","delta":"x"}`, true},
		{`{"type":"response.doom_loop_check","doom_loop_check":{}}`, true},
		{`{"delta":"no type field"}`, false},
		{`not json`, false},
	}
	for _, c := range cases {
		if got := isUnknownResponsesEventPayload([]byte(c.payload)); got != c.unknown {
			t.Fatalf("isUnknownResponsesEventPayload(%s) = %v, want %v", c.payload, got, c.unknown)
		}
	}
}
