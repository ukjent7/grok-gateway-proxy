package store

import (
	"encoding/json"
	"strings"
)

// extractFXUsage reads the original v3 usage object rather than the normalized
// Responses usage object. The latter must always contain cached_tokens: 0 for
// strict clients, which would incorrectly mark an upstream response as
// cache-supported when the gateway did not report cache information.
func extractFXUsage(body []byte) UsageMetrics {
	var root map[string]any
	if json.Unmarshal(body, &root) == nil {
		if usage, ok := root["usage"].(map[string]any); ok {
			return extractFXUsageMap(usage)
		}
		return UsageMetrics{}
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
		if json.Unmarshal([]byte(data), &root) != nil {
			continue
		}
		if usage, ok := root["usage"].(map[string]any); ok {
			last = extractFXUsageMap(usage)
		}
	}
	return last
}

func extractFXUsageMap(usage map[string]any) UsageMetrics {
	result := UsageMetrics{UsagePresent: true}
	var total, cached, cacheWrite int64
	var totalOK, cachedOK, cacheWriteOK, noCacheOK bool

	if input, ok := usage["inputTokens"].(map[string]any); ok {
		total, totalOK = fxFirstNumberOK(input, "total", "totalTokens", "inputTokens")
		cached, cachedOK = fxFirstNumberOK(input, "cacheRead", "cache_read", "cacheReadTokens", "cachedTokens")
		cacheWrite, cacheWriteOK = fxFirstNumberOK(input, "cacheWrite", "cache_write", "cacheWriteTokens")
		_, noCacheOK = fxFirstNumberOK(input, "noCache", "no_cache", "uncached")
	} else if value, ok := fxFirstNumberOK(usage, "inputTokens", "input_tokens"); ok {
		total, totalOK = value, true
	}

	if raw, ok := usage["raw"].(map[string]any); ok {
		if value, ok := fxFirstNumberOK(raw, "prompt_tokens", "input_tokens"); ok && !totalOK {
			total, totalOK = value, true
		}
		if value, ok := fxFirstNumberOK(raw, "cache_read_input_tokens", "cache_read_tokens"); ok && !cachedOK {
			cached, cachedOK = value, true
		}
		if value, ok := fxFirstNumberOK(raw, "cache_write_input_tokens", "cache_write_tokens"); ok && !cacheWriteOK {
			cacheWrite, cacheWriteOK = value, true
		}
		if details, ok := raw["prompt_tokens_details"].(map[string]any); ok {
			if value, ok := fxFirstNumberOK(details, "cached_tokens", "cache_read_tokens"); ok && !cachedOK {
				cached, cachedOK = value, true
			}
			if value, ok := fxFirstNumberOK(details, "cache_write_tokens"); ok && !cacheWriteOK {
				cacheWrite, cacheWriteOK = value, true
			}
		}
	}

	if details, ok := usage["input_tokens_details"].(map[string]any); ok {
		if value, ok := fxFirstNumberOK(details, "cached_tokens", "cache_read_tokens"); ok && !cachedOK {
			cached, cachedOK = value, true
		}
		if value, ok := fxFirstNumberOK(details, "cache_write_tokens"); ok && !cacheWriteOK {
			cacheWrite, cacheWriteOK = value, true
		}
	}

	if output, ok := usage["outputTokens"].(map[string]any); ok {
		result.OutputTokens, _ = fxFirstNumberOK(output, "total", "totalTokens", "outputTokens")
		result.ReasoningTokens, _ = fxFirstNumberOK(output, "reasoning", "reasoningTokens")
	} else {
		result.OutputTokens = fxFirstNumber(usage, "output_tokens", "completion_tokens")
		result.ReasoningTokens = fxFirstNumber(usage, "reasoning_tokens")
	}
	if raw, ok := usage["raw"].(map[string]any); ok {
		if value := fxFirstNumber(raw, "completion_tokens", "output_tokens"); value > 0 {
			result.OutputTokens = value
		}
		if value := fxFirstNumber(raw, "reasoning_tokens"); value > 0 {
			result.ReasoningTokens = value
		}
	}

	if totalOK {
		result.PromptTokens = total
		if cachedOK || noCacheOK {
			result.CacheReadTokens = cached
			result.CacheWriteTokens = cacheWrite
			result.InputTokens = fxMaxInt64(0, total-cached-cacheWrite)
			result.CacheSupported = true
			result.CacheSource = "v3.inputTokens"
		} else {
			result.InputTokens = total
		}
	}
	return result
}

// --- local copies of numeric helpers (kept private to avoid proxy dep) ---

func fxFirstNumber(m map[string]any, keys ...string) int64 {
	n, _ := fxFirstNumberOK(m, keys...)
	return n
}

func fxFirstNumberOK(m map[string]any, keys ...string) (int64, bool) {
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

func fxMaxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
