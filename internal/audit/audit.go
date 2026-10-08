// Package audit decides whether a tool call may run without a person looking at
// it.
//
// Three properties matter more than the rules themselves, because they are what
// make the rules worth having:
//
// It sees the call and the user's own words, and nothing else.
//
// That second part is the subtle one. An auditor that reads the whole
// conversation is useless, because the conversation is what gets poisoned - the
// agent writes an argument into the transcript and then cites it. So the
// auditor reads the transcript itself and keeps only the turns a PERSON
// authored. It is not handed a summary: a caller that supplies "the intent" can
// supply a false one, and the agent is a caller.
//
// Nothing here is an LLM. An auditor that reads the same poisoned context as
// the agent it audits is persuaded by the same argument, and one that
// occasionally hallucinates an approval is not an auditor.
//
// It is deterministic. The same call always produces the same verdict, so a
// decision can be explained, tested, and reviewed. A model that occasionally
// hallucinates an approval is not an auditor.
//
// It fails closed. Anything not recognised is risky, which asks the person. The
// cost of asking about something harmless is a keystroke; the cost of approving
// something harmful is not recoverable.
package audit

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Verdict is what the auditor concluded about a call.
type Verdict string

const (
	// Safe means the call may run without asking.
	Safe Verdict = "safe"

	// Risky means a person decides. It covers both "this is dangerous" and
	// "this is not recognised", which are the same answer for different reasons.
	Risky Verdict = "risky"

	// Refused means no person is asked, because there is no answer worth
	// offering.
	//
	// One case earns this: credential material bound for something that can
	// reach the network. Asking "may I send your AWS key to this address?" gives
	// a person a question whose only safe answer is no, and a tired reader
	// eventually says yes. The control is the refusal, not the prompt.
	Refused Verdict = "refused"
)

// Decision is a verdict with its reasoning.
type Decision struct {
	Verdict Verdict

	// Reasons explain the verdict in the terms of the rule that produced it, so
	// a person can judge the judgement rather than only obey it.
	Reasons []string

	// Task is what the user asked for, echoed so a person can see the call
	// beside the request it is supposedly serving. A destructive command that
	// does not match the task is the shape of a mistake worth catching, and it
	// is only visible with both in view.
	Task string
}

// WithTask attaches the user's request to a decision.
func (d Decision) WithTask(intent string) Decision {
	d.Task = clampIntent(intent)

	return d
}

// clampIntent bounds what is carried into a prompt.
//
// A user's request can be long, and an approval prompt that scrolls is one
// nobody reads. The end is kept rather than the start: a correction comes after
// the instruction it corrects.
func clampIntent(intent string) string {
	intent = strings.Join(strings.Fields(intent), " ")

	const limit = 240

	if runes := []rune(intent); len(runes) > limit {
		return "..." + string(runes[len(runes)-limit:])
	}

	return intent
}

// Allowed reports whether the call may run unattended.
func (d Decision) Allowed() bool { return d.Verdict == Safe }

// RefusedNow reports whether the call must not run at all.
func (d Decision) RefusedNow() bool { return d.Verdict == Refused }

// Explain renders the reasons as one line for a prompt.
func (d Decision) Explain() string {
	if len(d.Reasons) == 0 {
		return string(d.Verdict)
	}

	return strings.Join(d.Reasons, "; ")
}

// safe returns a Safe decision carrying why.
func safe(reason string) Decision {
	return Decision{Verdict: Safe, Reasons: []string{reason}}
}

// refused returns a Refused decision carrying why.
func refused(reason string) Decision {
	return Decision{Verdict: Refused, Reasons: []string{reason}}
}

// describe names what was found, without repeating it.
func describe(found []Credential) string {
	if len(found) == 1 {
		return found[0].What
	}

	return found[0].What + " and " + strings.Join(namesOf(found[1:]), ", ")
}

func namesOf(found []Credential) []string {
	out := make([]string, 0, len(found))

	for _, credential := range found {
		out = append(out, credential.What)
	}

	return out
}

// risky returns a Risky decision carrying why.
func risky(reason string) Decision {
	return Decision{Verdict: Risky, Reasons: []string{reason}}
}

// Assess judges one tool call against what the user asked for.
//
// transcript is read directly, and only the turns a person authored survive -
// the auditor makes that decision itself. It informs the EXPLANATION rather
// than the verdict: a destructive command is not made safe by appearing to
// match a task, and deciding otherwise would mean guessing at intent, which is
// where an auditor starts hallucinating.
func Assess(tool string, args json.RawMessage, transcript Transcript) Decision {
	intent := userRequest(transcript)

	// Checked before anything else, so no allow rule below can reach past it.
	// This is the one control the measurements support, and it is a refusal
	// rather than a question for the reason above.
	if found := FindCredentials(tool, args); len(found) > 0 {
		return refused(
			"this call would send " + describe(found) + " to " + tool +
				", which can reach the network",
		).WithTask(intent)
	}

	switch tool {
	// Reading cannot change anything, which is a property of the tool rather
	// than of what it was asked to read.
	case "read", "glob", "grep":
		return safe("reads only").WithTask(intent)

	// These change the workspace. A write inside the working directory is the
	// ordinary case and still worth a look: the file may be outside it.
	case "write", "edit":
		return assessWrite(tool, args).WithTask(intent)

	// Fetching a page the model chose. The fetch itself is read-only and the
	// address is checked before it is reached, so the risk is not the download -
	// it is what the page says. A person decides.
	case "webfetch":
		return risky("fetches a page the model chose, whose contents are untrusted").WithTask(intent)

	// A subagent runs tools of its own, so its own call is only as safe as the
	// worst thing it might be asked to do - which is not knowable from here.
	case "task":
		return risky("a subagent can run any tool, so it is judged on its own calls").WithTask(intent)

	case "bash":
		return assessBash(args).WithTask(intent)

	default:
		// An unrecognised tool is not automatically harmless. A new tool added
		// later would otherwise be auto-approved by default, which is the wrong
		// way for that mistake to fail.
		return risky(fmt.Sprintf("%q is not a tool this auditor knows", tool)).WithTask(intent)
	}
}

// assessWrite judges a file write.
func assessWrite(tool string, args json.RawMessage) Decision {
	var parsed struct {
		Path string `json:"path"`
	}

	if err := json.Unmarshal(args, &parsed); err != nil {
		return risky("the arguments could not be read, so the target is unknown")
	}

	if strings.TrimSpace(parsed.Path) == "" {
		return risky("no path was given")
	}

	// A write is a mutation. It is not auto-approved however ordinary the path
	// looks: the tool already knows where its workspace is, and deciding here
	// whether a path is inside it would duplicate that rule less reliably.
	return risky(fmt.Sprintf("%s writes to %s", tool, parsed.Path))
}
