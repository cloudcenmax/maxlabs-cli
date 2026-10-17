package llm

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
)

var errorMarkup = regexp.MustCompile(`<[!/?a-zA-Z][^>]*`)

// responseError retains useful API errors without surfacing HTML pages or
// arbitrary JSON debug envelopes in conversations and worker evidence.
func (p *HTTPProvider) responseError(operation string, response *http.Response) error {
	limit := p.MaxErrorBodyBytes
	if limit <= 0 {
		limit = defaultMaxErrorBody
	}
	body, _ := io.ReadAll(io.LimitReader(response.Body, int64(limit)))
	detail := http.StatusText(response.StatusCode)
	if detail == "" {
		detail = "Request failed"
	}
	text := strings.TrimSpace(string(body))
	if strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "text/html") || errorMarkup.MatchString(text) {
		detail += ". The gateway returned an HTML error page. Please retry shortly."
	} else {
		var envelope map[string]json.RawMessage
		if json.Unmarshal(body, &envelope) == nil && envelope != nil {
			fields := envelope
			if nested, ok := envelope["error"]; ok {
				var parsed map[string]json.RawMessage
				if json.Unmarshal(nested, &parsed) == nil && parsed != nil {
					fields = parsed
				} else {
					fields = map[string]json.RawMessage{"message": nested}
				}
			}
			var message string
			if json.Unmarshal(fields["message"], &message) == nil && strings.TrimSpace(message) != "" {
				detail = message
			}
			for _, key := range []string{"code", "type", "window", "resets_at"} {
				var value string
				if json.Unmarshal(fields[key], &value) == nil && strings.TrimSpace(value) != "" {
					detail += "; " + key + "=" + value
				}
			}
		} else if text != "" && !strings.HasPrefix(text, "{") && !strings.HasPrefix(text, "[") {
			detail = text
		}
	}
	if errorMarkup.MatchString(detail) {
		detail = http.StatusText(response.StatusCode) + ". The gateway returned an HTML error page. Please retry shortly."
	}
	detail = strings.Join(strings.Fields(detail), " ")
	runes := []rune(detail)
	if len(runes) > 512 {
		detail = string(runes[:509]) + "..."
	}
	return fmt.Errorf("llm: %s returned %d: %s", operation, response.StatusCode, detail)
}
