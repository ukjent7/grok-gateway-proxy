package proxy

import (
	"encoding/json"
	"net/http"
)

// WriteJSON encodes value as JSON and writes it with the given status.
func WriteJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// WriteError writes a structured error response.
func WriteError(w http.ResponseWriter, status int, err error) {
	WriteJSON(w, status, map[string]any{
		"error": map[string]any{
			"type":    "proxy_error",
			"message": err.Error(),
		},
	})
}
