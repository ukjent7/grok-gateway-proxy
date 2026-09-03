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

		remaining = nil
		t.skipping = true
	}

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

	switch protocol {
	case config.ProtocolChat, config.ProtocolOpenAICompatible:
		result = extractChatUsage(usage, result)
	case config.ProtocolAnthropic:
		result = extractAnthropicUsage(usage, result)
	default:
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

func extractAnthropicUsage(usage map[string]any, result store.UsageMetrics) store.UsageMetrics {
	total, totalOK := firstNumberOK(usage, "input_tokens", "prompt_tokens")
	hit, hitOK := firstNumberOK(usage, "cache_read_input_tokens", "cache_read_tokens", "cached_tokens")
	write, writeOK := firstNumberOK(usage, "cache_creation_input_tokens", "cache_write_tokens")
	if totalOK {
		result.InputTokens = total
	}
	if hitOK {
		result.CacheReadTokens = hit
	}
	if writeOK {
		result.CacheWriteTokens = write
	}
	if totalOK && (hitOK || writeOK) {
		result.CacheSupported = true
		result.CacheSource = "anthropic.input_tokens"
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
	n, _ := firstNumberOK(m, keys...)
	return n
}

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
