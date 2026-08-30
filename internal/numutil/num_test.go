package numutil

import (
	"encoding/json"
	"testing"
)

func TestFirstNumberOKAcceptsJSONNumberTypes(t *testing.T) {
	tests := []struct {
		name  string
		value any
		want  int64
		found bool
	}{
		{name: "float64 truncates toward zero", value: float64(12.9), want: 12, found: true},
		{name: "json.Number integer", value: json.Number("42"), want: 42, found: true},
		{name: "int64", value: int64(7), want: 7, found: true},
		{name: "int", value: int(9), want: 9, found: true},
		{name: "unsupported type is skipped", value: "12", want: 0, found: false},
		{name: "nil is skipped", value: nil, want: 0, found: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := FirstNumberOK(map[string]any{"n": test.value}, "n")
			if ok != test.found || got != test.want {
				t.Fatalf("FirstNumberOK(%#v) = (%d, %v), want (%d, %v)",
					test.value, got, ok, test.want, test.found)
			}
		})
	}
}

// A json.Number that is not an integer (or is out of range) must be skipped
// rather than silently truncated to zero, so the caller can fall through to
// the next candidate key.
func TestFirstNumberOKSkipsUnparsableJSONNumber(t *testing.T) {
	values := map[string]any{
		"fractional": json.Number("1.5"),
		"fallback":   float64(30),
	}
	if got, ok := FirstNumberOK(values, "fractional", "fallback"); !ok || got != 30 {
		t.Fatalf("expected the fractional json.Number to be skipped, got (%d, %v)", got, ok)
	}
	if got, ok := FirstNumberOK(map[string]any{"n": json.Number("1.5")}, "n"); ok || got != 0 {
		t.Fatalf("expected no value, got (%d, %v)", got, ok)
	}
}

func TestFirstNumberOKPrefersFirstPresentKey(t *testing.T) {
	values := map[string]any{"a": float64(1), "b": float64(2), "c": float64(3)}
	if got, ok := FirstNumberOK(values, "b", "a"); !ok || got != 2 {
		t.Fatalf("expected the first listed key to win, got (%d, %v)", got, ok)
	}
	// A key that is present but holds a non-number does not stop the search.
	if got, ok := FirstNumberOK(values, "missing", "b", "c"); !ok || got != 2 {
		t.Fatalf("expected the search to continue past absent/non-numeric keys, got (%d, %v)", got, ok)
	}
}

func TestFirstNumberReturnsZeroWhenNothingMatches(t *testing.T) {
	if got := FirstNumber(nil, "a", "b"); got != 0 {
		t.Fatalf("expected 0 for a nil map, got %d", got)
	}
	if got := FirstNumber(map[string]any{"a": "x"}, "a"); got != 0 {
		t.Fatalf("expected 0 for a non-numeric value, got %d", got)
	}
}
