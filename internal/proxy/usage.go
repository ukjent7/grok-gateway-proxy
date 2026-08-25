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
		if json.Unmarshal(data, &root) == nil {
			if metrics := extractUsageFromRoot(root, protocol); metrics.UsagePresent {
				last = metrics
			}
		}
	}
	return last
}

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
		result.InputTokens = maxInt64(0, total-hit-write)
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

func maxInt64(a, b int64) int64 {
	return numutil.MaxInt64(a, b)
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
