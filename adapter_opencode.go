package main

import (
	"fmt"
	"strings"
)

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

// ProfileForModel selects the compatibility profile by model-name prefix so
// every model in a family inherits its rules: upstream variants such as
// muse-spark-1.2-contributo match the "muse-spark" family just like
// muse-spark-1.2. The OpenCode gateway convention is:
//   - "muse-spark" prefixed models use the Muse profile;
//   - "deepseek" prefixed models currently pass through unchanged and get
//     their own case here as soon as DeepSeek needs protocol deviations.
func (OpenCodeResponsesAdapter) ProfileForModel(model string) ModelCompatibilityProfile {
	switch normalized := strings.ToLower(strings.TrimSpace(model)); {
	case strings.HasPrefix(normalized, "muse-spark"):
		return MuseSpark12Profile{}
	default:
		return nil
	}
}
