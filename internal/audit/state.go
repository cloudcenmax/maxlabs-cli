package audit

import (
	"encoding/json"
	"errors"
	"fmt"
)

// The decision model (TypeSafe's Jev) answers in two modes. Requests whose text
// stays under roughly 32 KB use its default mode: a couple of hundred
// milliseconds and its most accurate answers. Above that, and for any image, it
// switches to a slower long-context mode that TypeSafe itself describes as less
// accurate. Auto mode runs the auditor serially before every Bash call, so
// paying the slow mode on a long conversation is exactly the wrong trade.
//
// The budget below leaves room for the question criteria and JSON framing inside
// that 32 KB threshold.
const (
	// DefaultStateBudgetBytes is the serialized review-state budget.
	DefaultStateBudgetBytes = 28_000

	// maxStateMessageBytes bounds one user message after redaction. The first
	// and last messages are always kept, so clamping is what makes an
	// otherwise-unbounded chat fit without dropping authorization context.
	maxStateMessageBytes = 2_048

	// maxStateCommandBytes bounds the command under review. A command is one
	// shell invocation; anything larger is a generated blob and is not a
	// legitimate thing to audit.
	maxStateCommandBytes = 8_192

	// maxStateReasonBytes bounds the agent's untrusted explanation.
	maxStateReasonBytes = 2_048
)

// ErrStateTooLarge means the review could not be trimmed into the decision
// model's fast mode even after dropping context. Callers must fall back to the
// chat reviewer or ask the person.
var ErrStateTooLarge = errors.New("audit: review state exceeds the decision-model budget")

// StateTruncation reports what had to be removed to fit the budget.
//
// Material means the removed context was large enough that the decision model
// would be judging a materially incomplete authorization record. The reviewer
// treats a material trim as a reason to fall back rather than decide, because an
// auditor that cannot see what the user actually asked must not say "allow".
type StateTruncation struct {
	Truncated       bool
	Material        bool
	DroppedMessages int
	RemovedBytes    int
}

// BuildReviewState serializes the review into the decision model's fast mode.
//
// It keeps the first user message (the original task) and the most recent ones
// (later corrections override earlier consent), clamps each message, and drops
// middle messages oldest-first when the budget is still exceeded. A marker says
// how many were omitted, so the model is never silently shown a shorter history
// than the trimming implies.
func BuildReviewState(review CommandReview, budget int) (json.RawMessage, StateTruncation, error) {
	if budget <= 0 {
		budget = DefaultStateBudgetBytes
	}

	originalUserBytes := 0
	for _, message := range review.UserMessages {
		originalUserBytes += len(message)
	}

	originalBytes := originalUserBytes + len(review.Command) + len(review.Reason)

	review.Command = clampText(review.Command, maxStateCommandBytes)
	review.Reason = clampText(review.Reason, maxStateReasonBytes)

	kept := make([]string, len(review.UserMessages))
	for index, message := range review.UserMessages {
		kept[index] = clampText(message, maxStateMessageBytes)
	}

	truncation := StateTruncation{}
	dropped := 0

	encode := func() (json.RawMessage, error) {
		review.UserMessages = withOmissionMarker(kept, dropped)

		return json.Marshal(review)
	}

	encoded, err := encode()
	if err != nil {
		return nil, truncation, fmt.Errorf("audit: encoding the review state: %w", err)
	}

	for len(encoded) > budget && len(kept) > 2 {
		// Drop the oldest message after the first one: the first holds the
		// original request, the tail holds the newest corrections.
		kept = append(kept[:1:1], kept[2:]...)
		dropped++

		encoded, err = encode()
		if err != nil {
			return nil, truncation, fmt.Errorf("audit: encoding the review state: %w", err)
		}
	}

	approvalsDropped := false

	if len(encoded) > budget && len(review.ApprovalRequests) > 0 {
		review.ApprovalRequests = nil
		approvalsDropped = true

		encoded, err = encode()
		if err != nil {
			return nil, truncation, fmt.Errorf("audit: encoding the review state: %w", err)
		}
	}

	if len(encoded) > budget {
		return nil, truncation, ErrStateTooLarge
	}

	keptBytes := 0
	for _, message := range kept {
		keptBytes += len(message)
	}

	removed := originalBytes - keptBytes - len(review.Command) - len(review.Reason)
	if removed < 0 {
		removed = 0
	}

	truncation.Truncated = removed > 0 || dropped > 0 || approvalsDropped
	truncation.DroppedMessages = dropped
	truncation.RemovedBytes = removed
	// Material when a quarter or more of the authorization material is gone, or
	// when more than a handful of turns were dropped. Either way the decision
	// model is no longer looking at enough of the conversation to be trusted
	// with an "allow".
	truncation.Material = originalBytes > 0 && removed*4 > originalBytes || dropped > 5

	return encoded, truncation, nil
}

func clampText(text string, limit int) string {
	if limit <= 0 || len(text) <= limit {
		return text
	}

	removed := len(text) - limit

	return text[:limit] + fmt.Sprintf(" …[truncated %d bytes]", removed)
}

func withOmissionMarker(messages []string, dropped int) []string {
	if dropped == 0 || len(messages) == 0 {
		return messages
	}

	marked := make([]string, 0, len(messages)+1)
	marked = append(marked, messages[0])
	marked = append(marked, fmt.Sprintf("…[%d earlier user messages omitted]", dropped))
	marked = append(marked, messages[1:]...)

	return marked
}
