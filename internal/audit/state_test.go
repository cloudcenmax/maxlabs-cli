package audit

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestReviewStateUnderBudgetIsNotTruncated(t *testing.T) {
	review := CommandReview{
		Command:      "php artisan test",
		Reason:       "verify the fix",
		Workspace:    "/project",
		UserMessages: []string{"Fix the failing login test."},
	}

	state, truncation, err := BuildReviewState(review, DefaultStateBudgetBytes)
	if err != nil {
		t.Fatal(err)
	}

	if truncation.Truncated || truncation.Material || truncation.DroppedMessages != 0 {
		t.Fatalf("unexpected truncation: %+v", truncation)
	}

	var decoded CommandReview
	if err := json.Unmarshal(state, &decoded); err != nil {
		t.Fatal(err)
	}

	if decoded.Command != review.Command || decoded.UserMessages[0] != review.UserMessages[0] {
		t.Fatalf("state was altered: %+v", decoded)
	}
}

func TestReviewStateClampsOneOversizedMessage(t *testing.T) {
	review := CommandReview{
		Command:      "rm -rf build",
		Workspace:    "/project",
		UserMessages: []string{strings.Repeat("a", 5_000)},
	}

	state, truncation, err := BuildReviewState(review, 4_000)
	if err != nil {
		t.Fatal(err)
	}

	if !truncation.Truncated {
		t.Fatal("oversized message was not reported as truncated")
	}

	if len(state) > 4_000 {
		t.Fatalf("state exceeds the budget: %d bytes", len(state))
	}

	if !strings.Contains(string(state), "truncated") {
		t.Fatalf("no truncation marker: %s", string(state))
	}
}

func TestReviewStateDropsMiddleMessagesAndKeepsTheEnds(t *testing.T) {
	messages := make([]string, 0, 100)
	for index := 0; index < 100; index++ {
		messages = append(messages, strings.Repeat("x", 1_000))
	}
	messages[0] = "first-task"
	messages[99] = "latest-correction"

	review := CommandReview{Command: "php artisan test", Workspace: "/project", UserMessages: messages}

	state, truncation, err := BuildReviewState(review, 6_000)
	if err != nil {
		t.Fatal(err)
	}

	if len(state) > 6_000 {
		t.Fatalf("state exceeds the budget: %d bytes", len(state))
	}

	if !truncation.Truncated || truncation.DroppedMessages == 0 {
		t.Fatalf("expected a truncated state: %+v", truncation)
	}

	text := string(state)

	if !strings.Contains(text, "first-task") {
		t.Fatal("the original request was dropped")
	}

	if !strings.Contains(text, "latest-correction") {
		t.Fatal("the latest correction was dropped")
	}

	if !strings.Contains(text, "earlier user messages omitted") {
		t.Fatal("omitted messages were not disclosed to the model")
	}

	if !truncation.Material {
		t.Fatal("dropping most of a 100-message transcript should be material")
	}
}

func TestReviewStateRefusesWhenNothingCanBeDropped(t *testing.T) {
	review := CommandReview{
		Command:      "rm -rf build",
		Workspace:    "/project",
		UserMessages: []string{strings.Repeat("a", 8_000), strings.Repeat("b", 8_000)},
	}

	_, _, err := BuildReviewState(review, 400)
	if !errors.Is(err, ErrStateTooLarge) {
		t.Fatalf("expected ErrStateTooLarge, got %v", err)
	}
}

func TestReviewStateAlwaysFitsTheDefaultBudgetForALongChat(t *testing.T) {
	messages := make([]string, 0, 200)
	for index := 0; index < 200; index++ {
		messages = append(messages, strings.Repeat("z", 2_000))
	}

	review := CommandReview{Command: "go test ./...", Workspace: "/project", UserMessages: messages}

	state, truncation, err := BuildReviewState(review, DefaultStateBudgetBytes)
	if err != nil {
		t.Fatal(err)
	}

	if len(state) > DefaultStateBudgetBytes {
		t.Fatalf("long-chat state exceeds the fast-mode budget: %d bytes", len(state))
	}

	if !truncation.Truncated {
		t.Fatal("expected truncation for a 200-message transcript")
	}
}
