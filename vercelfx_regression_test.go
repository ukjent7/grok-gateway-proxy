package main

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func fxTestEvent(t *testing.T, payload map[string]any) string {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return "data: " + string(body) + "\n\n"
}

func TestVercelFXTerminalToolCallSnapshot(t *testing.T) {
	stream := fxTestEvent(t, map[string]any{
		"type": "tool-input-start", "id": "call-1", "toolName": "calc",
	})
	stream += fxTestEvent(t, map[string]any{
		"type": "tool-input-delta", "id": "call-1", "delta": `{"x":1}`,
	})
	stream += fxTestEvent(t, map[string]any{
		"type": "tool-call", "toolCallId": "call-1", "toolName": "calc",
		"input": map[string]any{"x": 1},
	})
	stream += fxTestEvent(t, map[string]any{
		"type": "finish", "finishReason": map[string]any{"unified": "tool-calls"},
	})

	out, err := vercelFXSSEToResponses("zai/glm-5.2", strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if err := json.Unmarshal(out, &response); err != nil {
		t.Fatal(err)
	}
	output := response["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("expected one function call, got %d: %s", len(output), out)
	}
	call := output[0].(map[string]any)
	if call["type"] != "function_call" || call["arguments"] != `{"x":1}` {
		t.Fatalf("terminal tool snapshot was duplicated or malformed: %+v", call)
	}
}

func TestVercelFXAssignsUniqueOutputIndexesWhenTextPrecedesTool(t *testing.T) {
	stream := fxTestEvent(t, map[string]any{"type": "text-delta", "delta": "before"})
	stream += fxTestEvent(t, map[string]any{"type": "tool-input-start", "id": "call-1", "toolName": "calc"})
	stream += fxTestEvent(t, map[string]any{"type": "tool-input-delta", "id": "call-1", "delta": `{"x":1}`})
	stream += fxTestEvent(t, map[string]any{"type": "finish", "finishReason": map[string]any{"unified": "tool-calls"}})

	body, err := io.ReadAll(newVercelFXSSEReader(strings.NewReader(stream), "zai/glm-5.2"))
	if err != nil {
		t.Fatal(err)
	}
	var indexes []int
	for _, line := range strings.Split(string(body), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			t.Fatal(err)
		}
		if event["type"] != "response.output_item.added" {
			continue
		}
		indexes = append(indexes, int(event["output_index"].(float64)))
	}
	if len(indexes) != 2 || indexes[0] != 0 || indexes[1] != 1 {
		t.Fatalf("output indexes collided or were reordered: %v\n%s", indexes, body)
	}
}

func TestExtractFXUsagePreservesCacheUnsupportedStateRegression(t *testing.T) {
	withoutCache := fxTestEvent(t, map[string]any{
		"type": "finish",
		"usage": map[string]any{
			"inputTokens":  map[string]any{"cacheRead": 0, "noCache": 100, "total": 100},
			"outputTokens": map[string]any{"total": 5},
		},
	}) + "data: [DONE]\n\n"
	usage := extractFXUsage([]byte(withoutCache))
	if !usage.UsagePresent || usage.CacheSupported || usage.InputTokens != 100 || usage.PromptTokens != 100 {
		t.Fatalf("unexpected unsupported-cache usage: %+v", usage)
	}

	withCache := fxTestEvent(t, map[string]any{
		"type": "finish",
		"usage": map[string]any{
			"inputTokens":  map[string]any{"total": 100, "cacheRead": 60},
			"outputTokens": map[string]any{"total": 5},
		},
	}) + "data: [DONE]\n\n"
	usage = extractFXUsage([]byte(withCache))
	if !usage.CacheSupported || usage.CacheReadTokens != 60 || usage.InputTokens != 40 || usage.PromptTokens != 100 {
		t.Fatalf("unexpected cached usage: %+v", usage)
	}
}
