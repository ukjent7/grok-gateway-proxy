package proxy

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

func TestExtractFXUsageCacheMissIsCacheSupported(t *testing.T) {
	withoutCache := fxTestEvent(t, map[string]any{
		"type": "finish",
		"usage": map[string]any{
			"inputTokens":  map[string]any{"cacheRead": 0, "noCache": 100, "total": 100},
			"outputTokens": map[string]any{"total": 5},
		},
	}) + "data: [DONE]\n\n"
	usage := extractFXUsage([]byte(withoutCache))
	// cacheRead: 0 + noCache: 100 means cache accounting is supported but the
	// request was a genuine cache miss (0% hit rate), not that caching is
	// unsupported.
	if !usage.UsagePresent || !usage.CacheSupported || usage.CacheReadTokens != 0 || usage.InputTokens != 100 || usage.PromptTokens != 100 {
		t.Fatalf("unexpected cache-miss usage: %+v", usage)
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

// When the v3 upstream omits both cacheRead and noCache, caching is genuinely
// unsupported and must not be reported as a 0% hit.
func TestExtractFXUsageNoCacheFieldsIsUnsupported(t *testing.T) {
	noCacheFields := fxTestEvent(t, map[string]any{
		"type": "finish",
		"usage": map[string]any{
			"inputTokens":  map[string]any{"total": 100},
			"outputTokens": map[string]any{"total": 5},
		},
	}) + "data: [DONE]\n\n"
	usage := extractFXUsage([]byte(noCacheFields))
	if !usage.UsagePresent || usage.CacheSupported || usage.PromptTokens != 100 || usage.InputTokens != 100 {
		t.Fatalf("expected cache-unsupported when no cache fields present: %+v", usage)
	}
}

// TestVercelFXStreamOutputIndexMatchesFinalOutput guards the core invariant of
// the FX stream transform: the output_index assigned to each streamed item must
// equal that item's position in the final response.output array, regardless of
// the order in which text, tool, and reasoning events arrive. Strict clients
// (async-openai) index response.output by output_index, so any divergence
// corrupts the assembled response.
func TestVercelFXStreamOutputIndexMatchesFinalOutput(t *testing.T) {
	// Deliberately interleaved arrival order: text, tool start, tool delta,
	// reasoning, tool delta — different from any fixed category ordering.
	stream := fxTestEvent(t, map[string]any{"type": "text-delta", "delta": "Hi "})
	stream += fxTestEvent(t, map[string]any{"type": "tool-input-start", "id": "call-1", "toolName": "calc"})
	stream += fxTestEvent(t, map[string]any{"type": "tool-input-delta", "id": "call-1", "delta": `{"x":`})
	stream += fxTestEvent(t, map[string]any{"type": "reasoning-delta", "delta": "hmm"})
	stream += fxTestEvent(t, map[string]any{"type": "tool-input-delta", "id": "call-1", "delta": "1}"})
	stream += fxTestEvent(t, map[string]any{
		"type":         "finish",
		"finishReason": map[string]any{"unified": "tool-calls"},
		"usage":        map[string]any{"inputTokens": map[string]any{"total": 10}, "outputTokens": map[string]any{"total": 5}},
	})

	body, err := io.ReadAll(newVercelFXSSEReader(strings.NewReader(stream), "zai/glm-5.2"))
	if err != nil {
		t.Fatal(err)
	}

	added := map[int]map[string]any{}
	var completed map[string]any
	for _, line := range strings.Split(string(body), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			t.Fatal(err)
		}
		switch event["type"] {
		case "response.output_item.added":
			idx := int(event["output_index"].(float64))
			added[idx] = event["item"].(map[string]any)
		case "response.completed":
			completed = event["response"].(map[string]any)
		}
	}
	if completed == nil {
		t.Fatalf("missing response.completed: %s", body)
	}
	output := completed["output"].([]any)
	if len(output) != len(added) {
		t.Fatalf("streamed %d items but final output has %d: %s", len(added), len(output), body)
	}
	wantTypes := []string{"message", "function_call", "reasoning"}
	for i, wantType := range wantTypes {
		finalItem, ok := output[i].(map[string]any)
		if !ok {
			t.Fatalf("output[%d] is not an object: %s", i, body)
		}
		addedItem, ok := added[i]
		if !ok {
			t.Fatalf("no output_item.added for output_index %d: %s", i, body)
		}
		if finalItem["type"] != wantType || addedItem["type"] != wantType {
			t.Fatalf("output_index %d: want type %q, streamed %v, final %v", i, wantType, addedItem["type"], finalItem["type"])
		}
		if addedItem["id"] != finalItem["id"] {
			t.Fatalf("output_index %d: streamed item id %v != final item id %v", i, addedItem["id"], finalItem["id"])
		}
	}
	if got := output[1].(map[string]any)["arguments"]; got != `{"x":1}` {
		t.Fatalf("tool arguments mismatch: %v", got)
	}
	if got := output[2].(map[string]any)["summary"].([]any)[0].(map[string]any)["text"]; got != "hmm" {
		t.Fatalf("reasoning summary mismatch: %v", got)
	}
}
