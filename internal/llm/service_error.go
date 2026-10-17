package llm

import (
	"encoding/json"
	"strings"
)

// ServiceError exposes a public failure category, never a raw service payload.
type ServiceError struct {
	Code     string
	Message  string
	CanRetry bool
}

func (e *ServiceError) Error() string   { return "MaxLabs: " + e.Message }
func (e *ServiceError) Retryable() bool { return e.CanRetry }

func eventError(raw []byte) error {
	var payload map[string]json.RawMessage
	if json.Unmarshal(raw, &payload) != nil {
		return nil
	}
	nested, exists := payload["error"]
	if !exists || string(nested) == "null" {
		return nil
	}
	var fields struct {
		Code string `json:"code"`
		Type string `json:"type"`
	}
	_ = json.Unmarshal(nested, &fields)
	code := strings.ToLower(fields.Code + " " + fields.Type)
	e := &ServiceError{Code: "service_unavailable", Message: "The service could not complete this request. Please retry shortly.", CanRetry: true}
	switch {
	case strings.Contains(code, "quota"), strings.Contains(code, "rate_limit"):
		e.Code = "request_limit"
		e.Message = "The request limit has been reached. Check account limits before retrying."
		e.CanRetry = false
	case strings.Contains(code, "auth"), strings.Contains(code, "api_key"), strings.Contains(code, "permission"), strings.Contains(code, "access_denied"):
		e.Code = "access_denied"
		e.Message = "The request was not authorized. Check your sign-in and model access."
		e.CanRetry = false
	case strings.Contains(code, "context_length"), strings.Contains(code, "context_window"):
		e.Code = "context_limit"
		e.Message = "This conversation exceeds the model context limit. Reduce the context before continuing."
		e.CanRetry = false
	case strings.Contains(code, "invalid_request"), strings.Contains(code, "invalid_argument"):
		e.Code = "invalid_request"
		e.Message = "The service could not accept this request. Review the request before retrying."
		e.CanRetry = false
	}
	return e
}
