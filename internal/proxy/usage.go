package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"

	"grok-gateway-proxy/internal/config"
	"grok-gateway-proxy/internal/numutil"
	"grok-gateway-proxy/internal/store"
)

// ExtractUsage reads usage metrics from either a single JSON response body or
// an SSE stream. For streams, usage may be carried by several events (e.g. the
// Responses API emits `response.created` with an all-zero usage object before
// the terminal event), so the last usage-bearing event wins: intermediate
// events are zeroed placeholders, while the terminal event holds the
// cumulative totals the client would bill on.
func ExtractUsage(body []byte, protocol config.Protocol) store.UsageMetrics {
	var root map[string]any
	if json.Unmarshal(body, &root) == nil {
		return extractUsageFromRoot(root, protocol)
	}
	var last store.UsageMetrics
	scanner := bufio.NewScanner(bytes.NewReader(body))
	const maxLine = 1024 * 1024
	buf := make([]byte, 64*1024)
	scanner.Buffer(buf, maxLine)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
			continue
		}
		// Each event needs a fresh map. encoding/json merges into a non-nil
		// map instead of clearing it, so reusing one across iterations keeps
		// top-level keys the later event omits. That resurrects an earlier
		// event's `response` envelope, and since extractUsageFromRoot checks
		// it before the root-level `usage`, the stale figures win.
		var event map[string]any
		if json.Unmarshal(data, &event) == nil {
			if metrics := extractUsageFromRoot(event, protocol); metrics.UsagePresent {
				last = metrics
			}
		}
	}
	return last
}

// usageTracker accumulates usage metrics from an SSE stream chunk by chunk as
// the bytes are forwarded, rather than re-scanning a buffer after the fact.
// The buffered approach reported zero tokens whenever the terminal
// usage-bearing event fell outside the capture window (the capture is capped)
// or whenever a single `data:` line exceeded bufio.Scanner's 1 MiB line limit,
// which stopped the scan outright. Neither can happen here: the terminal event
// is seen as it flows past, and an incomplete line is carried in tail until
// the rest arrives, so there is no line-length limit.
type usageTracker struct {
	protocol config.Protocol
	tail     []byte
	last     store.UsageMetrics
}

func newUsageTracker(protocol config.Protocol) *usageTracker {
	return &usageTracker{protocol: protocol}
}

// observe feeds the next chunk of stream bytes, splitting it into lines.
func (t *usageTracker) observe(chunk []byte) {
	buf := chunk
	if len(t.tail) > 0 {
		t.tail = append(t.tail, chunk...)
		buf = t.tail
	}
	consumed := 0
	for {
		index := bytes.IndexByte(buf[consumed:], '\n')
		if index < 0 {
			break
		}
		t.observeLine(buf[consumed : consumed+index])
		consumed += index + 1
	}
	// Carry the trailing fragment. buf may alias t.tail here; append writes
	// forward through copy, which tolerates the overlap.
	t.tail = append(t.tail[:0], buf[consumed:]...)
}

func (t *usageTracker) observeLine(line []byte) {
	data := bytes.TrimSpace(line)
	if !bytes.HasPrefix(data, []byte("data:")) {
		return
	}
	data = bytes.TrimSpace(data[len("data:"):])
	if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
		return
	}
	var root map[string]any
	if json.Unmarshal(data, &root) != nil {
		return
	}
	if metrics := extractUsageFromRoot(root, t.protocol); metrics.UsagePresent {
		t.last = metrics
	}
}

// usage returns the metrics of the last usage-bearing event seen, the same
// "terminal event wins" rule ExtractUsage applies to buffered bodies.
func (t *usageTracker) usage() store.UsageMetrics { return t.last }

func extractUsageFromRoot(root map[string]any, protocol config.Protocol) store.UsageMetrics {
	if response, ok := root["response"].(map[string]any); ok {
		if responseUsage, ok := response["usage"].(map[string]any); ok {
			root = map[string]any{"usage": responseUsage}
		}
	}
	usage, ok := root["usage"].(map[string]any)
	if !ok {
		return store.UsageMetrics{}
	}

	result := store.UsageMetrics{
		OutputTokens:    firstNumber(usage, "output_tokens", "completion_tokens"),
		ReasoningTokens: firstNestedNumber(usage, "output_tokens_details", "reasoning_tokens"),
		UsagePresent:    true,
	}
	if result.ReasoningTokens == 0 {
		result.ReasoningTokens = firstNumber(usage, "reasoning_tokens")
	}
	if result.ReasoningTokens == 0 {
		result.ReasoningTokens = firstNestedNumber(usage, "completion_tokens_details", "reasoning_tokens")
	}

	if protocol == config.ProtocolChat {
		result = extractChatUsage(usage, result)
	} else {
		result = extractResponsesUsage(usage, result)
	}
	result.PromptTokens = result.InputTokens + result.CacheReadTokens + result.CacheWriteTokens
	if result.PromptTokens == 0 {
		result.PromptTokens = firstNumber(usage, "prompt_tokens", "input_tokens")
	}
	return result
}

func extractChatUsage(usage map[string]any, result store.UsageMetrics) store.UsageMetrics {
	hit, hitOK := firstNumberOK(usage, "prompt_cache_hit_tokens", "cache_read_input_tokens", "cache_read_tokens")
	miss, missOK := firstNumberOK(usage, "prompt_cache_miss_tokens", "input_tokens", "uncached_input_tokens")
	write, writeOK := firstNumberOK(usage, "cache_write_input_tokens", "cache_write_tokens")

	if details, ok := usage["prompt_tokens_details"].(map[string]any); ok {
		if !hitOK {
			hit, hitOK = firstNumberOK(details, "cached_tokens", "cache_read_tokens")
		}
		if !writeOK {
			write, _ = firstNumberOK(details, "cache_write_tokens")
		}
	}
	total, totalOK := firstNumberOK(usage, "prompt_tokens")
	if hitOK && missOK {
		result.CacheReadTokens = hit
		result.InputTokens = miss
		result.CacheWriteTokens = write
		result.CacheSupported = true
		result.CacheSource = "chat.prompt_cache_hit_miss"
		return result
	}
	if hitOK && totalOK {
		result.CacheReadTokens = hit
		result.CacheWriteTokens = write
		result.InputTokens = max(0, total-hit-write)
		result.CacheSupported = true
		result.CacheSource = "chat.prompt_tokens_details"
		return result
	}
	if totalOK {
		result.InputTokens = total
	}
	return result
}

func extractResponsesUsage(usage map[string]any, result store.UsageMetrics) store.UsageMetrics {
	total, totalOK := firstNumberOK(usage, "input_tokens", "prompt_tokens")
	hit, hitOK := firstNumberOK(usage, "cache_read_input_tokens", "cache_read_tokens")
	write, writeOK := firstNumberOK(usage, "cache_write_input_tokens", "cache_write_tokens")
	if details, ok := usage["input_tokens_details"].(map[string]any); ok {
		if !hitOK {
			hit, hitOK = firstNumberOK(details, "cached_tokens", "cache_read_tokens")
		}
		if !writeOK {
			write, writeOK = firstNumberOK(details, "cache_write_tokens")
		}
	}
	if totalOK {
		result.InputTokens = total
	}
	if hitOK {
		result.CacheReadTokens = hit
	}
	if writeOK {
		result.CacheWriteTokens = write
	}
	if totalOK && hitOK {
		result.InputTokens = max(0, total-hit-write)
		result.CacheSupported = true
		result.CacheSource = "responses.input_tokens_details"
	}
	return result
}

func firstNumber(m map[string]any, keys ...string) int64 {
	return numutil.FirstNumber(m, keys...)
}

func firstNumberOK(m map[string]any, keys ...string) (int64, bool) {
	return numutil.FirstNumberOK(m, keys...)
}

func firstNestedNumber(m map[string]any, parent, key string) int64 {
	child, ok := m[parent].(map[string]any)
	if !ok {
		return 0
	}
	return firstNumber(child, key)
}

func ParseModel(body []byte) string {
	var root struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &root) == nil {
		return strings.TrimSpace(root.Model)
	}
	return ""
}
