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

// MaxInt64 returns the larger of a and b.
func MaxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// AsString returns v as string when it is a string, otherwise "".
func AsString(v any) string {
	s, _ := v.(string)
	return s
}

// AsInt64 converts a loosely-typed numeric value to int64. It is used for
// v3 usage objects that may encode numbers as float64, int, int64 or
// json.Number.
func AsInt64(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int:
		return int64(n)
	case int64:
		return n
	case json.Number:
		i, _ := n.Int64()
		return i
	}
	return 0
}

// AsFloat converts a loosely-typed numeric value to float64.
func AsFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}
