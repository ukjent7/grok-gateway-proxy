package proxy

import "grok-gateway-proxy/internal/store"

// extractFXUsage delegates to the canonical store implementation to avoid
// duplicating the v3 usage parsing logic across packages.
func extractFXUsage(body []byte) store.UsageMetrics {
	return store.ExtractFXUsage(body)
}
