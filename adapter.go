package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// GatewayAdapter describes one upstream's native protocol. Adapters deliberately
// do not translate Responses and Chat Completions; they validate the fixed
// endpoint and preserve the provider payload, including reasoning and tool data.
type GatewayAdapter interface {
	ID() string
	Protocol() Protocol
	EndpointPath() string
	AcceptsPath(path string) bool
	RejectMessage(path string) string
	ValidateRequest(body []byte) error
	NormalizeError(status int, body []byte) []byte
}

type OpenCodeResponsesAdapter struct{}

func (OpenCodeResponsesAdapter) ID() string           { return "OpenCodeResponsesAdapter" }
func (OpenCodeResponsesAdapter) Protocol() Protocol   { return ProtocolResponses }
func (OpenCodeResponsesAdapter) EndpointPath() string { return "/responses" }
func (a OpenCodeResponsesAdapter) AcceptsPath(path string) bool {
	return path == a.EndpointPath()
}
func (a OpenCodeResponsesAdapter) RejectMessage(path string) string {
	return fmt.Sprintf("%s accepts only %s, got %s", a.ID(), a.EndpointPath(), path)
}
func (OpenCodeResponsesAdapter) ValidateRequest(body []byte) error {
	return validateJSONRequest(body, "OpenCode Responses")
}
func (OpenCodeResponsesAdapter) NormalizeError(status int, body []byte) []byte {
	return normalizeUpstreamError(status, body)
}

type SenseNovaChatAdapter struct{}

func (SenseNovaChatAdapter) ID() string           { return "SenseNovaChatAdapter" }
func (SenseNovaChatAdapter) Protocol() Protocol   { return ProtocolChat }
func (SenseNovaChatAdapter) EndpointPath() string { return "/chat/completions" }
func (a SenseNovaChatAdapter) AcceptsPath(path string) bool {
	return path == a.EndpointPath()
}
func (a SenseNovaChatAdapter) RejectMessage(path string) string {
	return fmt.Sprintf("%s accepts only %s, got %s", a.ID(), a.EndpointPath(), path)
}
func (SenseNovaChatAdapter) ValidateRequest(body []byte) error {
	return validateJSONRequest(body, "SenseNova Chat Completions")
}
func (SenseNovaChatAdapter) NormalizeError(status int, body []byte) []byte {
	return normalizeUpstreamError(status, body)
}

// SenseNova's tool-call history decoder uses function_call for the tool call
// variant, while the client-facing Chat Completions format uses function.
// Keep the conversion scoped to tool_calls so tools[].type remains function.
func (SenseNovaChatAdapter) TransformRequestBody(body []byte) ([]byte, error) {
	return transformToolCallType(body, "function", "function_call")
}

func (SenseNovaChatAdapter) TransformResponseBody(body []byte) ([]byte, error) {
	return transformSenseNovaResponseBody(body)
}

func (SenseNovaChatAdapter) TransformSSE(reader io.Reader) io.Reader {
	return &senseNovaSSEReader{reader: bufio.NewReaderSize(reader, 64*1024)}
}

type payloadTransformer interface {
	TransformRequestBody([]byte) ([]byte, error)
	TransformResponseBody([]byte) ([]byte, error)
}

type streamTransformer interface {
	TransformSSE(io.Reader) io.Reader
}

func transformRequestBody(adapter GatewayAdapter, body []byte) ([]byte, error) {
	if transformer, ok := adapter.(payloadTransformer); ok {
		return transformer.TransformRequestBody(body)
	}
	return body, nil
}

func transformResponseBody(adapter GatewayAdapter, body []byte) ([]byte, error) {
	if transformer, ok := adapter.(payloadTransformer); ok {
		return transformer.TransformResponseBody(body)
	}
	return body, nil
}

func transformToolCallType(body []byte, from, to string) ([]byte, error) {
	var payload any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body, nil
	}
	if !rewriteToolCallTypes(payload, from, to) {
		return body, nil
	}
	result, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return result, nil
}

func transformSenseNovaResponseBody(body []byte) ([]byte, error) {
	var payload any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body, nil
	}
	changed := rewriteToolCallTypes(payload, "function_call", "function")
	changed = rewriteEmptyFinishReasons(payload) || changed
	if !changed {
		return body, nil
	}
	result, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return result, nil
}

func rewriteToolCallTypes(value any, from, to string) bool {
	changed := false
	switch current := value.(type) {
	case []any:
		for _, item := range current {
			changed = rewriteToolCallTypes(item, from, to) || changed
		}
	case map[string]any:
		for key, child := range current {
			if key == "tool_calls" {
				if calls, ok := child.([]any); ok {
					for _, call := range calls {
						if callObject, ok := call.(map[string]any); ok {
							if callType, ok := callObject["type"].(string); ok && callType == from {
								callObject["type"] = to
								changed = true
							}
						}
						changed = rewriteToolCallTypes(call, from, to) || changed
					}
				}
				continue
			}
			changed = rewriteToolCallTypes(child, from, to) || changed
		}
	}
	return changed
}

func rewriteEmptyFinishReasons(value any) bool {
	changed := false
	switch current := value.(type) {
	case []any:
		for _, item := range current {
			changed = rewriteEmptyFinishReasons(item) || changed
		}
	case map[string]any:
		for key, child := range current {
			if key == "finish_reason" {
				if reason, ok := child.(string); ok && reason == "" {
					current[key] = nil
					changed = true
				}
				continue
			}
			changed = rewriteEmptyFinishReasons(child) || changed
		}
	}
	return changed
}

type senseNovaSSEReader struct {
	reader  *bufio.Reader
	pending bytes.Buffer
	done    bool
	err     error
}

func (r *senseNovaSSEReader) Read(p []byte) (int, error) {
	if r.pending.Len() > 0 {
		return r.pending.Read(p)
	}
	if r.done {
		return 0, r.err
	}
	line, err := r.reader.ReadBytes('\n')
	if len(line) > 0 {
		r.pending.Write(transformSenseNovaSSELine(line))
	}
	if err != nil {
		r.done = true
		r.err = err
	}
	if r.pending.Len() > 0 {
		return r.pending.Read(p)
	}
	return 0, err
}

func transformSenseNovaSSELine(line []byte) []byte {
	lineEnd := []byte(nil)
	content := line
	if bytes.HasSuffix(content, []byte("\r\n")) {
		lineEnd = []byte("\r\n")
		content = content[:len(content)-2]
	} else if bytes.HasSuffix(content, []byte("\n")) {
		lineEnd = []byte("\n")
		content = content[:len(content)-1]
	} else if bytes.HasSuffix(content, []byte("\r")) {
		lineEnd = []byte("\r")
		content = content[:len(content)-1]
	}
	dataIndex := bytes.Index(content, []byte("data:"))
	if dataIndex < 0 || len(bytes.TrimSpace(content[:dataIndex])) != 0 {
		return line
	}
	payload := bytes.TrimSpace(content[dataIndex+len("data:"):])
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return line
	}
	converted, err := transformSenseNovaResponseBody(payload)
	if err != nil || bytes.Equal(converted, payload) {
		return line
	}
	result := make([]byte, 0, len(line)+8)
	result = append(result, content[:dataIndex]...)
	result = append(result, []byte("data: ")...)
	result = append(result, converted...)
	result = append(result, lineEnd...)
	return result
}

type VercelResponsesAdapter struct{}

func (VercelResponsesAdapter) ID() string           { return "VercelResponsesAdapter" }
func (VercelResponsesAdapter) Protocol() Protocol   { return ProtocolResponses }
func (VercelResponsesAdapter) EndpointPath() string { return "/responses" }
func (a VercelResponsesAdapter) AcceptsPath(path string) bool {
	return path == a.EndpointPath()
}
func (a VercelResponsesAdapter) RejectMessage(path string) string {
	return fmt.Sprintf("%s accepts only %s, got %s", a.ID(), a.EndpointPath(), path)
}
func (VercelResponsesAdapter) ValidateRequest(body []byte) error {
	return validateJSONRequest(body, "Vercel Responses")
}
func (VercelResponsesAdapter) NormalizeError(status int, body []byte) []byte {
	return normalizeUpstreamError(status, body)
}

func validateJSONRequest(body []byte, protocolName string) error {
	if len(strings.TrimSpace(string(body))) == 0 {
		return fmt.Errorf("%s request body cannot be empty", protocolName)
	}
	if !json.Valid(body) {
		return fmt.Errorf("%s request body must be valid JSON", protocolName)
	}
	return nil
}

func normalizeUpstreamError(status int, body []byte) []byte {
	message := fmt.Sprintf("upstream returned HTTP %d", status)
	var upstream struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &upstream) == nil && strings.TrimSpace(upstream.Error.Message) != "" {
		message = upstream.Error.Message
	}
	errorBody := map[string]any{
		"type":    "upstream_error",
		"message": message,
		"code":    status,
	}
	if len(body) > 0 {
		if json.Valid(body) {
			errorBody["details"] = json.RawMessage(body)
		} else {
			errorBody["details"] = string(body)
		}
	}
	result, err := json.Marshal(map[string]any{"error": errorBody})
	if err != nil {
		return []byte(`{"error":{"type":"upstream_error","message":"upstream request failed"}}`)
	}
	return result
}

var gatewayAdapters = map[string]GatewayAdapter{
	"oc": OpenCodeResponsesAdapter{},
	"st": SenseNovaChatAdapter{},
	"ve": VercelResponsesAdapter{},
}

func adapterFor(id string) (GatewayAdapter, bool) {
	adapter, ok := gatewayAdapters[id]
	return adapter, ok
}
