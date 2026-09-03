package proxy

import (
	"encoding/json"
	"strings"
	"testing"

	"grok-gateway-proxy/internal/config"
)

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestExtractResponsesUsage(t *testing.T) {
	body := mustJSON(t, map[string]any{
		"usage": map[string]any{
			"input_tokens":          160,
			"input_tokens_details":  map[string]any{"cached_tokens": 50},
			"output_tokens":         12,
			"output_tokens_details": map[string]any{"reasoning_tokens": 4},
		},
	})
	usage := ExtractUsage(body, config.ProtocolResponses)
	if usage.InputTokens != 110 || usage.CacheReadTokens != 50 || usage.PromptTokens != 160 {
		t.Fatalf("unexpected usage: %+v", usage)
	}
	if !usage.CacheSupported || usage.OutputTokens != 12 || usage.ReasoningTokens != 4 {
		t.Fatalf("unexpected usage details: %+v", usage)
	}

	if hitRate := float64(usage.CacheReadTokens) / float64(usage.PromptTokens) * 100; hitRate != 31.25 {
		t.Fatalf("unexpected hit rate: %v%%", hitRate)
	}
}

func TestExtractChatCacheHitMissUsage(t *testing.T) {
	body := mustJSON(t, map[string]any{
		"usage": map[string]any{
			"prompt_tokens":            160,
			"prompt_cache_hit_tokens":  50,
			"prompt_cache_miss_tokens": 110,
			"completion_tokens":        12,
		},
	})
	usage := ExtractUsage(body, config.ProtocolChat)
	if usage.InputTokens != 110 || usage.CacheReadTokens != 50 || usage.PromptTokens != 160 {
		t.Fatalf("unexpected usage: %+v", usage)
	}
	if !usage.CacheSupported {
		t.Fatal("expected cache metrics to be supported")
	}
}

func TestExtractSSEUsage(t *testing.T) {
	event := mustJSON(t, map[string]any{
		"response": map[string]any{
			"usage": map[string]any{
				"input_tokens":         100,
				"input_tokens_details": map[string]any{"cached_tokens": 25},
				"output_tokens":        8,
			},
		},
	})
	body := append([]byte("event: response.completed\ndata: "), event...)
	body = append(body, []byte("\n\n")...)
	usage := ExtractUsage(body, config.ProtocolResponses)
	if !usage.UsagePresent || usage.InputTokens != 75 || usage.CacheReadTokens != 25 {
		t.Fatalf("unexpected SSE usage: %+v", usage)
	}
}

func TestExtractSSEUsagePrefersTerminalEvent(t *testing.T) {
	created := mustJSON(t, map[string]any{
		"type": "response.created",
		"response": map[string]any{
			"usage": map[string]any{
				"input_tokens":         0,
				"input_tokens_details": map[string]any{"cached_tokens": 0},
				"output_tokens":        0,
			},
		},
	})
	completed := mustJSON(t, map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"usage": map[string]any{
				"input_tokens":          160,
				"input_tokens_details":  map[string]any{"cached_tokens": 50},
				"output_tokens":         12,
				"output_tokens_details": map[string]any{"reasoning_tokens": 4},
			},
		},
	})
	body := append([]byte("event: response.created\ndata: "), created...)
	body = append(body, []byte("\n\n")...)
	body = append(body, []byte("event: response.completed\ndata: ")...)
	body = append(body, completed...)
	body = append(body, []byte("\n\n")...)
	usage := ExtractUsage(body, config.ProtocolResponses)
	if !usage.UsagePresent || usage.InputTokens != 110 || usage.CacheReadTokens != 50 || usage.OutputTokens != 12 || usage.ReasoningTokens != 4 {
		t.Fatalf("expected terminal event usage, got: %+v", usage)
	}
}

func TestExtractSSEUsageDoesNotLeakAcrossEvents(t *testing.T) {
	enveloped := mustJSON(t, map[string]any{
		"type": "response.in_progress",
		"response": map[string]any{
			"usage": map[string]any{
				"prompt_tokens":            10,
				"completion_tokens":        2,
				"prompt_cache_hit_tokens":  3,
				"prompt_cache_miss_tokens": 7,
			},
		},
	})
	rootLevel := mustJSON(t, map[string]any{
		"usage": map[string]any{
			"prompt_tokens":            100,
			"completion_tokens":        20,
			"prompt_cache_hit_tokens":  30,
			"prompt_cache_miss_tokens": 70,
		},
	})
	body := append([]byte("data: "), enveloped...)
	body = append(body, []byte("\n\ndata: ")...)
	body = append(body, rootLevel...)
	body = append(body, []byte("\n\n")...)
	usage := ExtractUsage(body, config.ProtocolChat)
	if !usage.UsagePresent || usage.InputTokens != 70 || usage.CacheReadTokens != 30 || usage.OutputTokens != 20 {
		t.Fatalf("expected the terminal root-level usage to win, got: %+v", usage)
	}
}

func TestUsageWithoutCacheFieldsIsUnsupported(t *testing.T) {
	body := mustJSON(t, map[string]any{"usage": map[string]any{"prompt_tokens": 100, "completion_tokens": 8}})
	usage := ExtractUsage(body, config.ProtocolChat)
	if usage.CacheSupported || usage.CacheReadTokens != 0 {
		t.Fatalf("expected unavailable cache metrics: %+v", usage)
	}
}

func TestUsageTrackerHandlesOversizedLine(t *testing.T) {
	tracker := newUsageTracker(config.ProtocolResponses)
	tracker.observe([]byte(`data: {"type":"response.image_generation_call.partial_image","b64":"`))
	for i := 0; i < 11; i++ {
		tracker.observe([]byte(strings.Repeat("A", 100000)))
	}
	tracker.observe([]byte("\"}\n\n"))
	tracker.observe([]byte(`data: {"type":"response.completed","response":{"usage":{"input_tokens":333,"output_tokens":444}}}` + "\n\n"))

	got := tracker.usage()
	if got.InputTokens != 333 || got.OutputTokens != 444 {
		t.Fatalf("usage after an oversized line: in=%d out=%d, want 333/444", got.InputTokens, got.OutputTokens)
	}
}

func TestUsageTrackerIsChunkBoundaryAgnostic(t *testing.T) {
	stream := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":7,\"output_tokens\":8}}}\n\n"

	whole := newUsageTracker(config.ProtocolResponses)
	whole.observe([]byte(stream))

	split := newUsageTracker(config.ProtocolResponses)
	for i := 0; i < len(stream); i += 3 {
		end := i + 3
		if end > len(stream) {
			end = len(stream)
		}
		split.observe([]byte(stream[i:end]))
	}

	if whole.usage() != split.usage() {
		t.Fatalf("chunking changed usage: whole=%+v split=%+v", whole.usage(), split.usage())
	}
	if split.usage().InputTokens != 7 || split.usage().OutputTokens != 8 {
		t.Fatalf("usage: %+v, want 7/8", split.usage())
	}
}

func TestUsageTrackerPrefersTerminalEvent(t *testing.T) {
	tracker := newUsageTracker(config.ProtocolResponses)
	tracker.observe([]byte(`data: {"type":"response.created","response":{"usage":{"input_tokens":0,"output_tokens":0}}}` + "\n\n"))
	tracker.observe([]byte(`data: {"type":"response.completed","response":{"usage":{"input_tokens":11,"output_tokens":22}}}` + "\n\n"))

	if got := tracker.usage(); got.InputTokens != 11 || got.OutputTokens != 22 {
		t.Fatalf("usage: %+v, want 11/22", got)
	}
}

func TestFirstNumberOKAcceptsJSONNumberTypes(t *testing.T) {
	tests := []struct {
		name  string
		value any
		want  int64
		found bool
	}{
		{name: "float64 truncates toward zero", value: float64(12.9), want: 12, found: true},
		{name: "json.Number integer", value: json.Number("42"), want: 42, found: true},
		{name: "int64", value: int64(7), want: 7, found: true},
		{name: "int", value: int(9), want: 9, found: true},
		{name: "unsupported type is skipped", value: "12", want: 0, found: false},
		{name: "nil is skipped", value: nil, want: 0, found: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := firstNumberOK(map[string]any{"n": test.value}, "n")
			if ok != test.found || got != test.want {
				t.Fatalf("firstNumberOK(%#v) = (%d, %v), want (%d, %v)",
					test.value, got, ok, test.want, test.found)
			}
		})
	}
}

func TestFirstNumberOKSkipsUnparsableJSONNumber(t *testing.T) {
	values := map[string]any{
		"fractional": json.Number("1.5"),
		"fallback":   float64(30),
	}
	if got, ok := firstNumberOK(values, "fractional", "fallback"); !ok || got != 30 {
		t.Fatalf("expected the fractional json.Number to be skipped, got (%d, %v)", got, ok)
	}
	if got, ok := firstNumberOK(map[string]any{"n": json.Number("1.5")}, "n"); ok || got != 0 {
		t.Fatalf("expected no value, got (%d, %v)", got, ok)
	}
}

func TestFirstNumberOKPrefersFirstPresentKey(t *testing.T) {
	values := map[string]any{"a": float64(1), "b": float64(2), "c": float64(3)}
	if got, ok := firstNumberOK(values, "b", "a"); !ok || got != 2 {
		t.Fatalf("expected the first listed key to win, got (%d, %v)", got, ok)
	}

	if got, ok := firstNumberOK(values, "missing", "b", "c"); !ok || got != 2 {
		t.Fatalf("expected the search to continue past absent/non-numeric keys, got (%d, %v)", got, ok)
	}
}

func TestFirstNumberReturnsZeroWhenNothingMatches(t *testing.T) {
	if got := firstNumber(nil, "a", "b"); got != 0 {
		t.Fatalf("expected 0 for a nil map, got %d", got)
	}
	if got := firstNumber(map[string]any{"a": "x"}, "a"); got != 0 {
		t.Fatalf("expected 0 for a non-numeric value, got %d", got)
	}
}
