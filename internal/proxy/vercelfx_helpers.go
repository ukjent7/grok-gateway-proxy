package proxy

import (
	"strings"

	"grok-gateway-proxy/internal/numutil"
)

func asString(v any) string {
	return numutil.AsString(v)
}

func asInt64(v any) int64 {
	return numutil.AsInt64(v)
}

func asFloat(v any) (float64, bool) {
	return numutil.AsFloat(v)
}

func toolEventID(ev v3Event) string {
	for _, key := range []string{"id", "toolCallId", "call_id"} {
		if id := asString(ev[key]); id != "" {
			return id
		}
	}
	return ""
}

// mergeToolArgumentSnapshot returns only the suffix not already emitted by
// tool-input-delta events. A terminal tool-call event carries the complete
// input in current versions of the v3 protocol.
func mergeToolArgumentSnapshot(current, snapshot string) string {
	if snapshot == "" || snapshot == current {
		return ""
	}
	if current == "" {
		return snapshot
	}
	if strings.HasPrefix(snapshot, current) {
		return snapshot[len(current):]
	}
	// Do not append an unrelated complete snapshot: that would make the
	// Responses function arguments invalid JSON. The deltas remain authoritative.
	return ""
}
