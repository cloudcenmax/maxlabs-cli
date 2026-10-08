package audit

import "strings"

// Turn is one message as the auditor sees it.
//
// Source is carried rather than filtered on the way in, and that is the point.
// An interface that returned "the user's messages" would put the provenance
// decision in the hands of whoever implements it - including the agent. The
// auditor applies the rule itself, on the raw record, so supplying the material
// and deciding what counts are different jobs.
type Turn struct {
	// Role is the message's role: user, assistant, tool, system.
	Role string

	// Source records who authored it. "user" means a person typed it. Anything
	// else - a tool result, a subagent, the agent's own prose - is agent-side.
	Source string

	// Text is the message's text.
	Text string
}

// Transcript supplies the material the auditor may read.
//
// Implementations return the conversation as recorded. They do not decide which
// parts are trustworthy; the auditor does that, so that a caller cannot narrow
// or widen the view.
type Transcript interface {
	Turns() []Turn
}

// userRequest returns what the human asked for, taken from the transcript.
//
// Only turns whose Source is exactly "user" are read. Everything else in a
// session was written by the agent or by a tool: the agent's own prose, the
// reasoning that led to the call, the output of every command it ran. An
// auditor that read those could be argued with, because the argument would be
// in the material it was reading.
//
// The threat this addresses is a poisoned context, not a malicious agent. A
// context full of the agent's own confident reasoning is what makes a bad call
// look reasonable, and the user's own words are the part of the record that
// reasoning cannot rewrite.
func userRequest(transcript Transcript) string {
	if transcript == nil {
		return ""
	}

	var requests []string

	for _, turn := range transcript.Turns() {
		if turn.Source != "user" {
			continue
		}

		if text := strings.TrimSpace(turn.Text); text != "" {
			requests = append(requests, text)
		}
	}

	// The most recent few. A request stated ten turns ago and corrected since is
	// better represented by the correction, and an unbounded history makes a
	// prompt nobody reads.
	const keep = 3

	if len(requests) > keep {
		requests = requests[len(requests)-keep:]
	}

	return strings.Join(requests, " / ")
}
