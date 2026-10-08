package agent_test

import (
	"testing"

	"censi/harness/internal/agent"
)

// TestLeakedToolCallIsRecognised covers the case where a model writes a tool
// call out as text instead of calling a tool.
//
// The turn succeeds and reads as gibberish, which is the worst shape a failure
// can take: nothing is broken enough to report itself. The output limit was the
// cause - a large file edit truncated at the cap, and the fragment surfaced as
// prose - but the detection stays as a net for whatever produces it next.
func TestLeakedToolCallIsRecognised(t *testing.T) {
	cases := []string{
		`Sure, I'll write that.<｜DSML｜ invoke name="write">`,
		`<|tool_call|>{"name":"read"}`,
		`Let me do that. <| invoke name="bash">ls`,
		"prose before\n｜invoke name=\"edit\"\nmore",
	}

	for _, text := range cases {
		if !agent.LooksLikeLeakedToolCall(text) {
			t.Fatalf("not recognised as a leaked tool call: %q", text)
		}
	}
}

// Ordinary prose must not be flagged. A warning that fires on normal output
// trains the reader to ignore it.
func TestOrdinaryProseIsNotFlagged(t *testing.T) {
	cases := []string{
		"Both paths are wired and the existing suite is green.",
		"I invoked the function and it returned.",
		"The function_call field is part of the API.",
		"",
		"   ",
		"I'll write the file now.",
	}

	for _, text := range cases {
		if agent.LooksLikeLeakedToolCall(text) {
			t.Fatalf("ordinary prose was flagged: %q", text)
		}
	}
}
