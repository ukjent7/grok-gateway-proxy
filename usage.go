package main

import (
	"encoding/json"
	"strings"
)

// ExtractUsage reads usage metrics from either a single JSON response body or
// an SSE stream. For streams, usage may be carried by several events (e.g. the
// Responses API emits `response.created` with an all-zero usage object before
// the terminal event), so the last usage-bearing event wins: intermediate
// events are zeroed placeholders, while the terminal event holds the
// cumulative totals the client would bill on.
func ExtractUsage(body []byte, protocol Protocol) UsageMetrics {
	var root map[string]any
	if json.Unmarshal(body, &root) == nil {
		return extractUsageFromRoot(root, protocol)
	}
	var last UsageMetrics
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		if json.Unmarshal([]byte(data), &root) == nil {
			if metrics := extractUsageFromRoot(root, protocol); metrics.UsagePresent {
				last = metrics
			}
		}
	}
	return last
}

func extractUsageFromRoot(root map[string]any, protocol Protocol) UsageMetrics {
	if response, ok := root["response"].(map[string]any); ok {
		if responseUsage, ok := response["usage"].(map[string]any); ok {
			root = map[string]any{"usage": responseUsage}
		}
	}
	usage, ok := root["usage"].(map[string]any)
	if !ok {
		return UsageMetrics{}
	}

	result := UsageMetrics{
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

	if protocol == ProtocolChat {
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

func extractChatUsage(usage map[string]any, result UsageMetrics) UsageMetrics {
	hit, hitOK := firstNumberOK(usage, "prompt_cache_hit_tokens", "cache_read_input_tokens", "cache_read_tokens")
	miss, missOK := firstNumberOK(usage, "prompt_cache_miss_tokens", "input_tokens", "uncached_input_tokens")
	write, writeOK := firstNumberOK(usage, "cache_write_input_tokens", "cache_write_tokens")

	if details, ok := usage["prompt_tokens_details"].(map[string]any); ok {
		if !hitOK {
			hit, hitOK = firstNumberOK(details, "cached_tokens", "cache_read_tokens")
		}
		if !writeOK {
			write, writeOK = firstNumberOK(details, "cache_write_tokens")
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
		result.InputTokens = maxInt64(0, total-hit-write)
		result.CacheSupported = true
		result.CacheSource = "chat.prompt_tokens_details"
		return result
	}
	if totalOK {
		result.InputTokens = total
	}
	return result
}

func extractResponsesUsage(usage map[string]any, result UsageMetrics) UsageMetrics {
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
		result.InputTokens = maxInt64(0, total-hit-write)
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

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
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
