package main

import (
	"encoding/json"
	"testing"
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
	usage := ExtractUsage(body, ProtocolResponses)
	if usage.InputTokens != 110 || usage.CacheReadTokens != 50 || usage.PromptTokens != 160 {
		t.Fatalf("unexpected usage: %+v", usage)
	}
	if !usage.CacheSupported || usage.OutputTokens != 12 || usage.ReasoningTokens != 4 {
		t.Fatalf("unexpected usage details: %+v", usage)
	}
	if usage.HitRate() == nil || *usage.HitRate() != 31.25 {
		t.Fatalf("unexpected hit rate: %v", usage.HitRate())
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
	usage := ExtractUsage(body, ProtocolChat)
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
	usage := ExtractUsage(body, ProtocolResponses)
	if !usage.UsagePresent || usage.InputTokens != 75 || usage.CacheReadTokens != 25 {
		t.Fatalf("unexpected SSE usage: %+v", usage)
	}
}

// The Responses API emits response.created / response.in_progress with an
// all-zero usage object before the terminal response.completed carries the
// cumulative totals. Extraction must prefer the last (terminal) event so the
// metrics reflect what the client is actually billed on.
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
	usage := ExtractUsage(body, ProtocolResponses)
	if !usage.UsagePresent || usage.InputTokens != 110 || usage.CacheReadTokens != 50 || usage.OutputTokens != 12 || usage.ReasoningTokens != 4 {
		t.Fatalf("expected terminal event usage, got: %+v", usage)
	}
}

func TestUsageWithoutCacheFieldsIsUnsupported(t *testing.T) {
	body := mustJSON(t, map[string]any{"usage": map[string]any{"prompt_tokens": 100, "completion_tokens": 8}})
	usage := ExtractUsage(body, ProtocolChat)
	if usage.CacheSupported || usage.HitRate() != nil {
		t.Fatalf("expected unavailable cache metrics: %+v", usage)
	}
}
