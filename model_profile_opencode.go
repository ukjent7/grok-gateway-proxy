package main

import (
	"encoding/json"
	"io"
)

// MuseSpark12Profile handles the OpenCode Responses quirks observed for
// muse-spark-1.2. The model rejects the client-only stream_tool_calls option.
type MuseSpark12Profile struct{}

func (MuseSpark12Profile) ID() string { return "muse-spark-1.2" }

func (MuseSpark12Profile) TransformRequestBody(body []byte) ([]byte, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return body, nil
	}
	if _, exists := payload["stream_tool_calls"]; !exists {
		return body, nil
	}
	delete(payload, "stream_tool_calls")
	return json.Marshal(payload)
}

func (MuseSpark12Profile) TransformResponseBody(body []byte) ([]byte, error) {
	return body, nil
}

func (MuseSpark12Profile) TransformSSE(reader io.Reader) io.Reader {
	return reader
}
