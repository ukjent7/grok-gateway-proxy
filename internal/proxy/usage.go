package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"

	"grok-gateway-proxy/internal/config"
	"grok-gateway-proxy/internal/store"
)

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
		var event map[string]any
		if json.Unmarshal(data, &event) == nil {
			if metrics := extractUsageFromRoot(event, protocol); metrics.UsagePresent {
				last = metrics
			}
		}
	}
	return last
}

// maxUsageTailBytes caps the fragment held between observe() calls while
// waiting for a newline. It is deliberately independent of maxBodyBytes
// (32 MiB) and the per-column capture limit (256 KiB): it guards the
// streaming line splitter, not audit storage. A single SSE data line larger
// than 4 MiB cannot be a usage event (usage JSON is < 2 KiB); holding more
// would pin arbitrary upstream payload. Must stay < maxBodyBytes.
const maxUsageTailBytes = 4 << 20

type usageTracker struct {
	protocol config.Protocol
	tail     []byte
	last     store.UsageMetrics
	skipping bool
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
		if !t.skipping {
			t.observeLine(buf[consumed : consumed+index])
		}
		t.skipping = false
		consumed += index + 1
	}
	remaining := buf[consumed:]
	if len(remaining) > maxUsageTailBytes {
		// A fragment this far past the cap has no newline in sight, so drop
		// it and skip to the end of the line it belongs to. A single frame
		// that large cannot carry a usage object anyone can act on, and
		// skipping only that line leaves the rest of the stream metered —
		// bailing out for good would silently zero an otherwise fine request.
		remaining = nil
		t.skipping = true
	}
	// Carry the trailing fragment. buf may alias t.tail here; append writes
	// forward through copy, which tolerates the overlap.
	t.tail = append(t.tail[:0], remaining...)
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

// firstNumber returns the first numeric value found under the given keys, or
// zero when none of them carries one.
func firstNumber(m map[string]any, keys ...string) int64 {
	n, _ := firstNumberOK(m, keys...)
	return n
}

// firstNumberOK returns the first present numeric value and whether it was
// found. It accepts float64 (the default json numbers), json.Number, int and
// int64 so callers do not need to handle type switches themselves.
func firstNumberOK(m map[string]any, keys ...string) (int64, bool) {
	for _, key := range keys {
		value, ok := m[key]
		if !ok {
			continue
		}
		switch n := value.(type) {
		case float64:
			return int64(n), true
		case json.Number:
			// A fractional or out-of-range json.Number is skipped rather than
			// truncated, so the search falls through to the next candidate key.
			if parsed, err := n.Int64(); err == nil {
				return parsed, true
			}
		case int64:
			return n, true
		case int:
			return int64(n), true
		}
	}
	return 0, false
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
