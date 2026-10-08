package store_test

import (
	"testing"

	"censi/harness/internal/store"
)

func TestReviewsReplaceRunningRecordWithTerminalOutcome(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	project, err := st.Project(t.TempDir())
	if err != nil {
		t.Fatalf("project: %v", err)
	}

	if err := project.AppendReview("one", store.Review{
		TurnID: "turn-1", AssistantSeq: -1, Status: "running",
		Verification: store.ReviewVerification{Status: "not_run"},
	}); err != nil {
		t.Fatalf("append running: %v", err)
	}
	if err := project.AppendReview("one", store.Review{
		TurnID: "turn-1", AssistantSeq: 4, Status: "completed",
		Verification: store.ReviewVerification{Status: "passed", Output: "ok"},
		Summary:      store.ReviewSummary{FilesChanged: 1},
	}); err != nil {
		t.Fatalf("append completed: %v", err)
	}

	reviews, err := project.Reviews("one")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(reviews) != 1 {
		t.Fatalf("review count = %d, want 1", len(reviews))
	}
	got := reviews[0]
	if got.Status != "completed" || got.AssistantSeq != 4 || got.Verification.Status != "passed" {
		t.Fatalf("review = %+v", got)
	}

	// Review sidecars are not conversations. A crashed turn can have a running
	// review before its first model-visible event, and must not appear as a ghost
	// chat in the project list.
	sessions, err := project.Sessions()
	if err != nil {
		t.Fatalf("sessions: %v", err)
	}
	if len(sessions) != 0 {
		t.Fatalf("review sidecar appeared as sessions: %+v", sessions)
	}
}
