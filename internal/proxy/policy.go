package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type blockedModelError struct {
	model string
}

func (e *blockedModelError) Error() string {
	return fmt.Sprintf("model %q is blocked by the proxy", e.model)
}

func isBlockedModel(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	return model == "grok" || strings.HasPrefix(model, "grok-")
}

func writeBlockedModelResponse(w http.ResponseWriter, model string, stream bool) {
	responseID := "resp-blocked-" + strings.TrimPrefix(newRequestID(), "req-")
	response := map[string]any{
		"id":         responseID,
		"object":     "response",
		"created_at": time.Now().Unix(),
		"status":     "completed",
		"model":      model,
		"output":     []any{},
	}
	if !stream {
		WriteJSON(w, http.StatusOK, response)
		return
	}
	created := map[string]any{
		"type":            "response.created",
		"sequence_number": 0,
		"response":        response,
	}
	completed := map[string]any{
		"type":            "response.completed",
		"sequence_number": 1,
		"response":        response,
	}
	createdBody, _ := json.Marshal(created)
	completedBody, _ := json.Marshal(completed)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, "event: response.created\ndata: %s\n\n", createdBody)
	_, _ = fmt.Fprintf(w, "event: response.completed\ndata: %s\n\n", completedBody)
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}
