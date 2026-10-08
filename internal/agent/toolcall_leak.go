package agent

import (
	"regexp"
	"strings"
)

// leakedToolCall matches tool-invocation markup that a model emitted as prose
// instead of as a structured tool call.
//
// The pattern is deliberately generic rather than tied to one vendor's format:
// what it looks for is a delimited marker immediately followed by an invocation
// keyword. A model that cannot produce a structured call falls back to writing
// the call out in whatever syntax it was trained on, and the harness has no way
// to know which syntax that will be.
//
// It is not parsed back into a call. Recovering an action from text the model
// was not asked to produce means guessing at arguments, and a guessed write is
// worse than a visible failure.
var leakedToolCall = regexp.MustCompile(
	`(?s)[<｜|]\s*[｜|]?\s*\w*\s*[｜|]?\s*(invoke|tool_call|function_call)\b`,
)

// LooksLikeLeakedToolCall reports whether text contains tool-call markup.
//
// Only consulted when the model returned no tool calls at all: a response that
// legitimately quotes such markup alongside a real call is not a fault, and
// flagging it would train the reader to ignore the warning.
func LooksLikeLeakedToolCall(text string) bool {
	if strings.TrimSpace(text) == "" {
		return false
	}

	return leakedToolCall.MatchString(text)
}
