package main

import "fmt"

// OpenCodeResponsesAdapter handles the OpenCode Zen gateway (Responses protocol).
// No request/response transformation is needed — the protocol passes through.
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
