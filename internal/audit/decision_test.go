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

func TestSecondOpinionResolvesAnUnsettledDecision(t *testing.T) {
	// A "stop" is final: the second opinion cannot overturn it.
	stopped := func(context.Context, json.RawMessage, Transcript) ReviewResult {
		return ReviewResult{Decision: "stop", Confidence: 0.9, Risk: "high", Authorized: false}
	}
	allowed := func(context.Context, json.RawMessage, Transcript) ReviewResult {
		return ReviewResult{Decision: "allow", Confidence: 0.9, Risk: "medium", Authorized: true}
	}

	if got := NewSecondOpinionReviewer(stopped, allowed)(context.Background(), nil, reviewTranscript{{Role: "user", Source: "user", Text: "x"}}); got.Decision != "stop" {
		t.Fatalf("a stop was overturned: %+v", got)
	}

	// The decision model said allow but its confidence did not clear the gate;
	// the chat reviewer is asked and its allow settles the call.
	unsure := func(context.Context, json.RawMessage, Transcript) ReviewResult {
		return ReviewResult{Decision: "allow", Confidence: 0.55, Risk: "medium", Authorized: true}
	}
	if got := NewSecondOpinionReviewer(unsure, allowed)(context.Background(), nil, reviewTranscript{{Role: "user", Source: "user", Text: "x"}}); !got.Allows() {
		t.Fatalf("an unsettled allow was not resolved by the second opinion: %+v", got)
	}

	// An ask is also unsettled: a decisive second allow runs the call.
	asked := func(context.Context, json.RawMessage, Transcript) ReviewResult {
		return ReviewResult{Decision: "ask", Confidence: 0.4, Risk: "medium", Authorized: false}
	}
	if got := NewSecondOpinionReviewer(asked, allowed)(context.Background(), nil, reviewTranscript{{Role: "user", Source: "user", Text: "x"}}); !got.Allows() {
		t.Fatalf("an ask was not resolved by the second opinion: %+v", got)
	}

	// If the second opinion also declines, the call stays a question.
	if got := NewSecondOpinionReviewer(asked, asked)(context.Background(), nil, reviewTranscript{{Role: "user", Source: "user", Text: "x"}}); got.Allows() {
		t.Fatalf("two declined reviews allowed the call: %+v", got)
	}

	// A decisive primary allow does not spend a second request.
	calls := 0
	decisive := func(context.Context, json.RawMessage, Transcript) ReviewResult {
		calls++

		return ReviewResult{Decision: "allow", Confidence: 0.95, Risk: "low", Authorized: true}
	}
	if got := NewSecondOpinionReviewer(decisive, allowed)(context.Background(), nil, reviewTranscript{{Role: "user", Source: "user", Text: "x"}}); !got.Allows() || calls != 1 {
		t.Fatalf("a decisive answer was re-reviewed: %+v calls=%d", got, calls)
	}
}
