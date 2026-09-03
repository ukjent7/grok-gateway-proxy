package proxy

import (
	"strings"
	"testing"
)

func TestApplyJSONMemberRewrites(t *testing.T) {
	functionCallToFunction := jsonMemberRewrite{
		key:             typeKey,
		from:            "function_call",
		to:              []byte(`"function"`),
		inToolCallsOnly: true,
	}
	tests := []struct {
		name     string
		doc      string
		rewrites []jsonMemberRewrite
		want     string
	}{
		{
			name:     "tool call entry",
			doc:      `{"tool_calls":[{"type":"function_call"}]}`,
			rewrites: []jsonMemberRewrite{functionCallToFunction},
			want:     `{"tool_calls":[{"type":"function"}]}`,
		},
		{
			name:     "tool definition is out of scope",
			doc:      `{"tools":[{"type":"function_call"}]}`,
			rewrites: []jsonMemberRewrite{functionCallToFunction},
			want:     `{"tools":[{"type":"function_call"}]}`,
		},
		{

			name:     "escaped JSON in a value is not a member",
			doc:      `{"delta":"{\"tool_calls\":[{\"type\":\"function_call\"}]}"}`,
			rewrites: []jsonMemberRewrite{functionCallToFunction},
			want:     `{"delta":"{\"tool_calls\":[{\"type\":\"function_call\"}]}"}`,
		},
		{
			name:     "nested below the array element",
			doc:      `{"choices":[{"delta":{"tool_calls":[{"type":"function_call","function":{}}]}}]}`,
			rewrites: []jsonMemberRewrite{functionCallToFunction},
			want:     `{"choices":[{"delta":{"tool_calls":[{"type":"function","function":{}}]}}]}`,
		},
		{

			name:     "surrounding bytes survive",
			doc:      "{\n  \"a\":1,\n  \"tool_calls\" : [ {  \"type\"  :  \"function_call\" } ]\n}",
			rewrites: []jsonMemberRewrite{functionCallToFunction},
			want:     "{\n  \"a\":1,\n  \"tool_calls\" : [ {  \"type\"  :  \"function\" } ]\n}",
		},
		{
			name:     "every match in the document is rewritten",
			doc:      `{"tool_calls":[{"type":"function_call"}],"x":{"tool_calls":[{"type":"function_call"}]}}`,
			rewrites: []jsonMemberRewrite{functionCallToFunction},
			want:     `{"tool_calls":[{"type":"function"}],"x":{"tool_calls":[{"type":"function"}]}}`,
		},
		{
			name: "several rewrites share one pass",
			doc:  `{"type":"response.reasoning.delta","finish_reason":""}`,
			rewrites: []jsonMemberRewrite{
				{key: typeKey, from: "response.reasoning.delta", to: []byte(`"response.reasoning_text.delta"`)},
				{key: "finish_reason", from: "", to: []byte("null")},
			},
			want: `{"type":"response.reasoning_text.delta","finish_reason":null}`,
		},
		{
			name:     "number literals are untouched",
			doc:      `{"tool_calls":[{"type":"function_call"}],"n":9007199254740993,"f":1.0,"e":1e3}`,
			rewrites: []jsonMemberRewrite{functionCallToFunction},
			want:     `{"tool_calls":[{"type":"function"}],"n":9007199254740993,"f":1.0,"e":1e3}`,
		},
		{
			name:     "malformed document is left for the peer to reject",
			doc:      `{"tool_calls":[{"type":"function_call"`,
			rewrites: []jsonMemberRewrite{functionCallToFunction},
			want:     `{"tool_calls":[{"type":"function_call"`,
		},
		{
			name:     "trailing garbage after a complete value",
			doc:      `{"tool_calls":[{"type":"function_call"}]} nonsense`,
			rewrites: []jsonMemberRewrite{functionCallToFunction},
			want:     `{"tool_calls":[{"type":"function"}]} nonsense`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, changed := applyJSONMemberRewrites([]byte(test.doc), test.rewrites...)
			if string(got) != test.want {
				t.Fatalf("applyJSONMemberRewrites(%q) = %q, want %q", test.doc, got, test.want)
			}
			if changed != (string(got) != test.doc) {
				t.Fatalf("changed = %v, but the result differs from the input only %v", changed, string(got) != test.doc)
			}
		})
	}
}

func TestApplyJSONMemberRewritesLeavesNonMatchingDocumentsByteIdentical(t *testing.T) {
	doc := []byte(`{" model " : "x", "choices":[{"finish_reason":"stop","message":{"content":"a<b>&c"}}]}`)
	got, changed := applyJSONMemberRewrites(doc, senseNovaResponseRewrites...)
	if changed {
		t.Fatalf("nothing should have matched: %q", got)
	}
	if string(got) != string(doc) {
		t.Fatalf("input was re-serialised: %q, want %q", got, doc)
	}
}

func TestTransformSenseNovaResponseBodyFinishReason(t *testing.T) {
	got := transformSenseNovaResponseBody([]byte(`{"choices":[{"finish_reason":"","x":"finish_reason"},{"finish_reason":"stop"}]}`))
	if !strings.Contains(string(got), `"finish_reason":null`) {
		t.Fatalf("empty finish_reason was not normalised: %s", got)
	}
	if !strings.Contains(string(got), `"x":"finish_reason"`) || !strings.Contains(string(got), `"finish_reason":"stop"`) {
		t.Fatalf("a non-empty finish_reason or an unrelated string was changed: %s", got)
	}
}
