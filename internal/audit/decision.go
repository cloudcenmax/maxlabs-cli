package audit

import (
	"context"
	"encoding/json"
	"time"

	"censi/harness/internal/llm"
)

// Reviewer is what the permission guard calls for one command. A zero
// ReviewResult means "no usable decision"; the guard then asks the person.
type Reviewer func(context.Context, json.RawMessage, Transcript) ReviewResult

// DecisionProvider asks the gateway for a typed decision about an isolated,
// budgeted review state. Implemented by the HTTP provider that talks to the app
// API, so auto mode never needs a provider key of its own.
type DecisionProvider interface {
	Decide(ctx context.Context, model string, state json.RawMessage) (llm.DecisionResponse, error)
}

// decisionTimeout bounds one decision call. The decision model answers in a few
// hundred milliseconds; a slow call must not hold up the command it is gating.
const decisionTimeout = 4 * time.Second

// NewDecisionReviewer asks the decision model first.
//
// It trims the review state into the model's fast mode. When the trim is
// material — too much of the authorization record would be missing — it refuses
// to decide, because "allow" on a shortened history is exactly the failure this
// gate exists to prevent. The caller chains a chat reviewer behind it.
func NewDecisionReviewer(provider DecisionProvider, model, workspace string) Reviewer {
	return func(ctx context.Context, args json.RawMessage, transcript Transcript) ReviewResult {
		if provider == nil {
			return ReviewResult{}
		}

		review, _, ok := commandReviewFromArgs(args, transcript, workspace)
		if !ok {
			return ReviewResult{}
		}

		state, truncation, err := BuildReviewState(review, DefaultStateBudgetBytes)
		if err != nil || truncation.Material {
			return ReviewResult{}
		}

		ctx, cancel := context.WithTimeout(ctx, decisionTimeout)
		defer cancel()

		answer, err := provider.Decide(ctx, model, state)
		if err != nil {
			return ReviewResult{}
		}

		if !validDecision(answer) {
			return ReviewResult{}
		}

		return ReviewResult{
			ReviewedWorkspace: workspace,
			Decision:          answer.Decision,
			Confidence:        answer.Confidence,
			Risk:              answer.Risk,
			Authorized:        answer.Authorized,
		}
	}
}

// NewSecondOpinionReviewer asks the primary auditor first and, when its answer
// does not settle the call, asks a second one.
//
// The decision model is fast and cheap but deliberately conservative on opaque
// command text: a Python heredoc or a Playwright driver often comes back
// "allow" with confidence just under the gate, which used to become a prompt.
// A chat model reads the same state and, in practice, establishes those
// ordinary steps; asking it only for the unsettled calls keeps routine work
// unattended without making the cheap model the sole authority.
//
// A "stop" is final. The second opinion exists to resolve uncertainty, not to
// overturn a refusal.
func NewSecondOpinionReviewer(primary, second Reviewer) Reviewer {
	return func(ctx context.Context, args json.RawMessage, transcript Transcript) ReviewResult {
		first := ReviewResult{}
		if primary != nil {
			first = primary(ctx, args, transcript)
		}

		if first.Decision == "stop" {
			return first
		}

		if first.Decision != "" && first.Allows() {
			return first
		}

		if second == nil {
			return first
		}

		opposing := second(ctx, args, transcript)

		if opposing.Decision == "stop" {
			return opposing
		}

		if opposing.Allows() {
			return opposing
		}

		if first.Decision != "" {
			return first
		}

		return opposing
	}
}

func validDecision(answer llm.DecisionResponse) bool {
	switch answer.Decision {
	case "allow", "ask", "stop":
	default:
		return false
	}

	switch answer.Risk {
	case "low", "medium", "high":
	default:
		return false
	}

	if answer.Confidence < 0 || answer.Confidence > 1 {
		return false
	}

	return true
}
