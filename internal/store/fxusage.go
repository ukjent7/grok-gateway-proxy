package store

import (
	"bufio"
	"bytes"
	"encoding/json"

	"grok-gateway-proxy/internal/numutil"
)

// ExtractFXUsage reads the original v3 usage object rather than the normalized
// Responses usage object. The latter must always contain cached_tokens: 0 for
// strict clients, which would incorrectly mark an upstream response as
// cache-supported when the gateway did not report cache information.
func ExtractFXUsage(body []byte) UsageMetrics {
	return extractFXUsage(body)
}

func extractFXUsage(body []byte) UsageMetrics {
	var root map[string]any
	if json.Unmarshal(body, &root) == nil {
		if usage, ok := root["usage"].(map[string]any); ok {
			return extractFXUsageMap(usage)
		}
		return UsageMetrics{}
	}

	var last UsageMetrics
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
		if json.Unmarshal(data, &root) != nil {
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
			cacheWrite = value
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

// Helpers delegate to the shared numutil package so the extraction logic
// stays single-sourced across proxy and store.

func fxFirstNumber(m map[string]any, keys ...string) int64 {
	return numutil.FirstNumber(m, keys...)
}

func fxFirstNumberOK(m map[string]any, keys ...string) (int64, bool) {
	return numutil.FirstNumberOK(m, keys...)
}

func fxMaxInt64(a, b int64) int64 {
	return numutil.MaxInt64(a, b)
}
