package numutil

import "encoding/json"

// FirstNumber returns the first present numeric value for the given keys.
func FirstNumber(m map[string]any, keys ...string) int64 {
	n, _ := FirstNumberOK(m, keys...)
	return n
}

// FirstNumberOK returns the first present numeric value and whether it was
// found. It accepts float64 (the default json numbers), json.Number, int and
// int64 so callers do not need to handle type switches themselves.
func FirstNumberOK(m map[string]any, keys ...string) (int64, bool) {
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
