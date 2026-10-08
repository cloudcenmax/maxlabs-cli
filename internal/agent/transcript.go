package agent

import (
	"censi/harness/internal/audit"
	"censi/harness/internal/session"
)

// transcript exposes the session for the auditor to read.
//
// It returns the record as it stands and makes no judgement about it. Selecting
// which turns are trustworthy is the auditor's job, and inverting that - having
// this hand over "the user's intent" - would put the provenance decision in the
// hands of the agent being audited.
func (a *Agent) transcript() audit.Transcript {
	return logTranscript{log: a.log}
}

type logTranscript struct{ log *session.Log }

func (l logTranscript) Turns() []audit.Turn {
	var out []audit.Turn

	for _, message := range l.log.DeriveMessages() {
		out = append(out, audit.Turn{
			Role:   string(message.Role),
			Source: message.Source,
			Text:   textOf(message),
		})
	}

	return out
}
