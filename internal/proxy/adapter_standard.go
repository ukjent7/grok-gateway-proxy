package proxy

import "io"

type StandardResponsesAdapter struct{ baseResponsesAdapter }

func (StandardResponsesAdapter) ValidateRequest(body []byte) error {
	return validateJSONRequest(body, "standard Responses")
}

func (StandardResponsesAdapter) TransformRequestBody(body []byte) ([]byte, error) {
	return sanitizeResponsesRequest(body)
}

func (StandardResponsesAdapter) TransformResponseBody(body []byte) ([]byte, error) {
	return body, nil
}

func (StandardResponsesAdapter) TransformSSE(reader io.Reader) io.Reader {
	return newResponsesSSEFilter(reader)
}
