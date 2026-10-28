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

// NewFallbackReviewer runs primary, and only when it produced no decision runs
// fallback. It never mixes the two: a partial answer from one auditor is not
// evidence for the other.
func NewFallbackReviewer(primary, fallback Reviewer) Reviewer {
	return func(ctx context.Context, args json.RawMessage, transcript Transcript) ReviewResult {
		if primary != nil {
			if result := primary(ctx, args, transcript); result.Decision != "" {
				return result
			}
		}

		if fallback != nil {
			return fallback(ctx, args, transcript)
		}

		return ReviewResult{}
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
