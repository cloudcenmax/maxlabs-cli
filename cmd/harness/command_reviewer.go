package main

import (
	"context"
	"encoding/json"

	"censi/harness/internal/audit"
	"censi/harness/internal/llm"
)

// commandReviewer builds the two-stage reviewer used by auto mode.
//
// The decision model answers first because it is fast and cheap. When its
// answer does not clear the gate — typical for opaque command text such as a
// Python heredoc — a chat model reads the same isolated state. The second
// opinion only resolves uncertainty; it never overturns a stop.
//
// "worker" is the public model key for the second opinion. Using the tier the
// turn happens to run on would make the same uncertain command cost seconds on
// Pro for no better judgement.
func commandReviewer(provider llm.Provider, model, workspace string) func(context.Context, json.RawMessage, audit.Transcript) audit.ReviewResult {
	decisive := audit.NewCommandReviewer(provider, "worker", workspace)

	if decisionProvider, ok := provider.(audit.DecisionProvider); ok {
		return audit.NewSecondOpinionReviewer(
			audit.NewDecisionReviewer(decisionProvider, model, workspace),
			decisive,
		)
	}

	return audit.NewCommandReviewer(provider, model, workspace)
}
