package audit

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"censi/harness/internal/llm"
)

type decisionProviderFunc func(context.Context, string, json.RawMessage) (llm.DecisionResponse, error)

func (f decisionProviderFunc) Decide(ctx context.Context, model string, state json.RawMessage) (llm.DecisionResponse, error) {
	return f(ctx, model, state)
}

func TestDecisionReviewerAllowsOnlyWhenTheDecisionSaysSo(t *testing.T) {
	var seen []byte

	provider := decisionProviderFunc(func(_ context.Context, model string, state json.RawMessage) (llm.DecisionResponse, error) {
		seen = append([]byte(nil), state...)

		if !strings.Contains(string(state), "php artisan test") {
			t.Fatalf("command missing from the state: %s", string(state))
		}
		if !strings.Contains(string(state), "run the suite") {
			t.Fatalf("authorization material missing from the state: %s", string(state))
		}

		return llm.DecisionResponse{Decision: "allow", Confidence: 0.9, Risk: "low", Authorized: true}, nil
	})

	result := NewDecisionReviewer(provider, "pro", "/project")(context.Background(), json.RawMessage(`{"command":"php artisan test","reason":"verify"}`), reviewTranscript{
		{Role: "user", Source: "user", Text: "run the suite"},
	})

	if !result.Allows() {
		t.Fatalf("aligned command was not allowed: %+v", result)
	}

	if len(seen) > DefaultStateBudgetBytes {
		t.Fatalf("state exceeded the fast-mode budget: %d bytes", len(seen))
	}
}

func TestDecisionReviewerRejectsMalformedAnswers(t *testing.T) {
	for _, answer := range []llm.DecisionResponse{
		{Decision: "maybe", Confidence: 0.9, Risk: "low", Authorized: true},
		{Decision: "allow", Confidence: 1.5, Risk: "low", Authorized: true},
		{Decision: "allow", Confidence: 0.9, Risk: "extreme", Authorized: true},
	} {
		provider := decisionProviderFunc(func(context.Context, string, json.RawMessage) (llm.DecisionResponse, error) {
			return answer, nil
		})

		result := NewDecisionReviewer(provider, "pro", "/project")(context.Background(), json.RawMessage(`{"command":"ls"}`), reviewTranscript{
			{Role: "user", Source: "user", Text: "look around"},
		})

		if result.Decision != "" {
			t.Fatalf("malformed answer was accepted: %+v", result)
		}
	}
}

func TestDecisionReviewerFallsThroughOnProviderErrors(t *testing.T) {
	provider := decisionProviderFunc(func(context.Context, string, json.RawMessage) (llm.DecisionResponse, error) {
		return llm.DecisionResponse{}, errors.New("availability")
	})

	result := NewDecisionReviewer(provider, "pro", "/project")(context.Background(), json.RawMessage(`{"command":"ls"}`), reviewTranscript{
		{Role: "user", Source: "user", Text: "look around"},
	})

	if result.Decision != "" {
		t.Fatalf("failed decision was accepted: %+v", result)
	}
}

func TestDecisionReviewerRefusesWhenTheTrimIsMaterial(t *testing.T) {
	calls := 0
	provider := decisionProviderFunc(func(context.Context, string, json.RawMessage) (llm.DecisionResponse, error) {
		calls++

		return llm.DecisionResponse{Decision: "allow", Confidence: 0.9, Risk: "low", Authorized: true}, nil
	})

	messages := make([]Turn, 0, 100)
	for index := 0; index < 100; index++ {
		messages = append(messages, Turn{Role: "user", Source: "user", Text: strings.Repeat("x", 1_000)})
	}

	result := NewDecisionReviewer(provider, "pro", "/project")(context.Background(), json.RawMessage(`{"command":"rm -rf src"}`), reviewTranscript(messages))

	if calls != 0 {
		t.Fatal("the decision model was asked to judge a materially incomplete history")
	}

	if result.Decision != "" {
		t.Fatalf("materially trimmed review produced a decision: %+v", result)
	}
}

func TestFallbackReviewerUsesTheFallbackOnlyWhenPrimaryIsSilent(t *testing.T) {
	fallback := func(got ReviewResult) Reviewer {
		return func(context.Context, json.RawMessage, Transcript) ReviewResult { return got }
	}

	silent := func(context.Context, json.RawMessage, Transcript) ReviewResult { return ReviewResult{} }

	if got := NewFallbackReviewer(silent, fallback(ReviewResult{Decision: "ask"}))(context.Background(), nil, reviewTranscript{{Role: "user", Source: "user", Text: "x"}}); got.Decision != "ask" {
		t.Fatalf("fallback did not run: %+v", got)
	}

	primary := func(context.Context, json.RawMessage, Transcript) ReviewResult {
		return ReviewResult{Decision: "allow", Confidence: 0.95, Risk: "low", Authorized: true}
	}

	if got := NewFallbackReviewer(primary, fallback(ReviewResult{Decision: "stop"}))(context.Background(), nil, reviewTranscript{{Role: "user", Source: "user", Text: "x"}}); got.Decision != "allow" {
		t.Fatalf("fallback overrode a primary decision: %+v", got)
	}
}
