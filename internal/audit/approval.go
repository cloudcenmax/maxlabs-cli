package audit

import (
	"encoding/json"
	"fmt"
)

// ApprovalRequired ends the current turn without executing the blocked call.
// Its question is recorded by the host as approval_request, so later reviews
// can interpret a user's answer without trusting ordinary assistant prose.
type ApprovalRequired struct{ Question string }

func (e *ApprovalRequired) Error() string {
	return "explicit user approval is required before this activity"
}
func RequireApproval(args json.RawMessage, workspace string) *ApprovalRequired {
	var input struct {
		Command string `json:"command"`
	}
	_ = json.Unmarshal(args, &input)
	question := fmt.Sprintf("May I run this command? Please explicitly approve or decline this activity before I proceed.\n\n```sh\n%s\n```", Redact(input.Command))
	if workspace != "" {
		question += "\n\nWorking folder: " + workspace
	}
	return &ApprovalRequired{Question: question}
}

type ApprovalContext struct {
	Question         string `json:"question"`
	AfterUserMessage int    `json:"after_user_message"`
}

func ApprovalRequests(transcript Transcript) []ApprovalContext {
	var requests []ApprovalContext
	count := 0
	if transcript != nil {
		for _, turn := range transcript.Turns() {
			if turn.Role == "user" && turn.Source == "user" {
				count++
			}
			if turn.Role == "assistant" && turn.Source == "approval_request" {
				requests = append(requests, ApprovalContext{Redact(turn.Text), count})
			}
		}
	}
	return requests
}
