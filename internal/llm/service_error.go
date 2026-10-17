package llm

import (
	"encoding/json"
	"strings"
	"time"
)

// ServiceError exposes a public failure category, never a raw service payload.
type ServiceError struct {
	Code     string
	Message  string
	CanRetry bool
	Window   string
	ResetsAt string
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
		Code     string `json:"code"`
		Type     string `json:"type"`
		Window   string `json:"window"`
		ResetsAt string `json:"resets_at"`
	}
	_ = json.Unmarshal(nested, &fields)
	code := strings.ToLower(fields.Code + " " + fields.Type)
	e := &ServiceError{Code: "service_unavailable", Message: "The service could not complete this request. Please retry shortly.", CanRetry: true}
	switch {
	case strings.Contains(code, "quota"):
		e.Code = "request_limit"
		e.Message = "The request limit has been reached. Check account limits before retrying."
		e.CanRetry = false
		if fields.Window == "h5" || fields.Window == "weekly" {
			e.Window = fields.Window
			label := "weekly"
			if fields.Window == "h5" {
				label = "five-hour"
			}
			e.Message = "The " + label + " allowance cannot cover this request."
			if reset, err := time.Parse(time.RFC3339, fields.ResetsAt); err == nil {
				e.ResetsAt = reset.UTC().Format(time.RFC3339)
				e.Message += " Resets at " + e.ResetsAt + "."
			}
		}
	case strings.Contains(code, "rate_limit"), strings.Contains(code, "concurrency_limit"):
		e.Code = "temporary_rate_limit"
		e.Message = "Request traffic is temporarily limited. Retry after current requests finish or the rate window resets."
		e.CanRetry = true
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
