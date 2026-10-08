package store

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

const reviewFileSuffix = ".review.jsonl"

// Review is durable, person-facing evidence for one completed agent turn.
//
// It deliberately lives beside the model-visible session log instead of inside
// it. Review data (a diff, a command's output, or verification text) must not
// change the conversation the model receives on a later turn, but it must
// survive a restart so a person can audit work after it happened.
//
// AssistantSeq identifies the assistant message that completed the turn. It is
// a session event sequence rather than an array index, so it stays stable when
// the surface is compacted or the client renders only a subset of messages.
type Review struct {
	// TurnID joins the initial running record to its later final record. It is
	// also useful when a process exits before an assistant message exists.
	TurnID       string             `json:"turnId,omitempty"`
	AssistantSeq int                `json:"assistantSeq"`
	Status       string             `json:"status"`
	Stopped      bool               `json:"stopped,omitempty"`
	Recoverable  bool               `json:"recoverable,omitempty"`
	Error        string             `json:"error,omitempty"`
	Warnings     []string           `json:"warnings,omitempty"`
	Verification ReviewVerification `json:"verification"`
	Summary      ReviewSummary      `json:"summary"`
	Tools        []ReviewTool       `json:"tools,omitempty"`
	Diffs        []ReviewDiff       `json:"diffs,omitempty"`
}

// ReviewVerification records whether the workspace's configured check ran and
// what it reported. Status is one of not_run, not_configured, passed, or failed.
type ReviewVerification struct {
	Status string `json:"status"`
	Output string `json:"output,omitempty"`
}

// ReviewSummary is intentionally small enough for a transcript header.
type ReviewSummary struct {
	Steps        int `json:"steps"`
	ToolCalls    int `json:"toolCalls"`
	FilesChanged int `json:"filesChanged"`
	LinesAdded   int `json:"linesAdded"`
	LinesRemoved int `json:"linesRemoved"`
}

// ReviewTool records the human-relevant result of one tool call. Output is the
// same bounded model-visible result, not an unbounded process transcript.
type ReviewTool struct {
	CallID    string `json:"callId,omitempty"`
	Name      string `json:"name"`
	Arguments string `json:"arguments,omitempty"`
	Output    string `json:"output,omitempty"`
	IsError   bool   `json:"isError,omitempty"`
}

// ReviewDiff is one file-level patch. Lines are kept in their original order;
// Kind uses add, del, same, or gap.
type ReviewDiff struct {
	Target  string           `json:"target"`
	Added   int              `json:"added"`
	Removed int              `json:"removed"`
	Lines   []ReviewDiffLine `json:"lines,omitempty"`
}

type ReviewDiffLine struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
}

// AppendReview durably appends one completed-turn review. JSON Lines gives it
// the same crash recovery behaviour as the session event log: a partial final
// record does not hide prior reviews.
func (p *Project) AppendReview(sessionID string, review Review) error {
	file, err := os.OpenFile(p.reviewPath(sessionID), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("store: opening review log: %w", err)
	}
	defer func() { _ = file.Close() }()

	line, err := json.Marshal(review)
	if err != nil {
		return fmt.Errorf("store: encoding review: %w", err)
	}

	if _, err := file.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("store: writing review: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("store: syncing review log: %w", err)
	}

	return nil
}

// Reviews loads the most recent review for each turn. Rewriting
// a review is not currently needed, but keeping the latest record makes this
// format forward-compatible with a later running-to-completed transition.
func (p *Project) Reviews(sessionID string) ([]Review, error) {
	file, err := os.Open(p.reviewPath(sessionID))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: opening review log: %w", err)
	}
	defer func() { _ = file.Close() }()

	byTurn := map[string]Review{}
	order := make([]string, 0)
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var review Review
		if err := json.Unmarshal([]byte(line), &review); err != nil {
			// Match session logs: retain all complete records before a crash-truncated
			// final line rather than making the complete session unavailable.
			break
		}
		key := review.TurnID
		if key == "" {
			// Reviews created before turn IDs existed remain readable.
			key = fmt.Sprintf("assistant-%d", review.AssistantSeq)
		}
		if _, seen := byTurn[key]; !seen {
			order = append(order, key)
		}
		byTurn[key] = review
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("store: reading review log: %w", err)
	}

	reviews := make([]Review, 0, len(order))
	for _, turnID := range order {
		reviews = append(reviews, byTurn[turnID])
	}

	return reviews, nil
}

func (p *Project) reviewPath(sessionID string) string {
	return strings.TrimSuffix(p.sessionPath(sessionID), eventsFileSuffix) + reviewFileSuffix
}
