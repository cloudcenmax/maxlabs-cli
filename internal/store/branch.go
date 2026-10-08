package store

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"time"

	"censi/harness/internal/session"
)

// Origin records where a branched conversation came from.
//
// Kept beside the log rather than inside it. A branch is a prefix of its parent,
// and writing a marker event into that prefix would make the two differ at the
// very first token - which is precisely where a provider's cache begins. The
// lineage is therefore metadata, not history.
type Origin struct {
	SessionID string `json:"session_id"`
	AtSeq     int    `json:"at_seq"`
	At        string `json:"at"`
}

// Branch starts a new session from a prefix of an existing one.
//
// The parent is never modified. The log is append-only, so branching is a read
// of a prefix plus a write elsewhere - there is no operation here that could
// alter the conversation it came from.
//
// The child's events are the parent's events up to and including atSeq, in the
// same order and with the same sequence numbers, so the child's prefix is
// byte-identical to the parent's. That is what makes a branch continue from the
// cache the parent had already built rather than paying to re-read it.
func (p *Project) Branch(sourceID string, atSeq session.Seq) (string, error) {
	source, err := p.Load(sourceID)
	if err != nil {
		return "", err
	}

	events := source.Events()

	if len(events) == 0 {
		return "", fmt.Errorf("store: session %s has no events to branch from", sourceID)
	}

	// A branch point beyond the end is a client mistake, and silently taking the
	// whole log would hide it.
	if int(atSeq) >= len(events) {
		return "", fmt.Errorf("store: no event %d in session %s", atSeq, sourceID)
	}

	// Clamp to the end of the last complete message, so a branch cannot land
	// between a tool call and its result. A log cut mid-pair replays into a
	// request the provider rejects, and the user would see a failure they did
	// not cause.
	atSeq = lastCompleteBoundary(events, atSeq)

	branchID := newBranchID()

	prefix := make([]session.Event, 0, int(atSeq)+1)
	for _, event := range events {
		if event.Seq > atSeq {
			break
		}

		prefix = append(prefix, event)
	}

	// Re-appending rather than copying bytes: the events go through the same
	// path that built the live log, so a branched session cannot differ from its
	// parent in any way the reader could notice.
	if err := p.Append(branchID, prefix...); err != nil {
		return "", err
	}

	// Review evidence is not model-visible history, but it is part of the
	// person's audit trail. Copy only records whose associated assistant message
	// lies inside the branched prefix; a running/orphaned record cannot describe
	// the new conversation safely.
	if reviews, err := p.Reviews(sourceID); err == nil {
		for _, review := range reviews {
			if review.AssistantSeq < 0 || session.Seq(review.AssistantSeq) > atSeq {
				continue
			}
			if err := p.AppendReview(branchID, review); err != nil {
				// The branch's model history is durable. Like lineage metadata below,
				// a review copy failure must not throw away the branch the user asked
				// us to preserve.
				break
			}
		}
	}

	origin := Origin{
		SessionID: sourceID,
		AtSeq:     int(atSeq),
		At:        time.Now().UTC().Format(time.RFC3339),
	}

	if err := writeJSONFile(p.branchPath(branchID), origin); err != nil {
		// The conversation exists; only its provenance was lost. Failing the
		// whole branch would throw away work the user asked for over metadata
		// they can live without.
		return branchID, nil
	}

	return branchID, nil
}

// BranchOrigin reports where a session was branched from.
func (p *Project) BranchOrigin(sessionID string) (Origin, bool) {
	var origin Origin

	if err := readJSONFile(p.branchPath(sessionID), &origin); err != nil {
		return Origin{}, false
	}

	return origin, true
}

// lastCompleteBoundary walks back to the last point that is not waiting on a
// tool result.
//
// Two shapes are incomplete. Ending ON a tool result is fine - the pair is
// closed. Ending on an assistant turn whose tool calls are answered afterwards
// is not: the prefix would contain a call with no result, which replays into a
// request the provider rejects, surfacing as a failure the user did not cause.
func lastCompleteBoundary(events []session.Event, at session.Seq) session.Seq {
	for i := int(at); i >= 0 && i < len(events); i-- {
		message, ok := events[i].Message()

		if !ok {
			return session.Seq(i)
		}

		unanswered := message.Role == session.RoleAssistant &&
			len(message.ToolCalls()) > 0 &&
			i+1 < len(events) &&
			events[i+1].Type == session.EventToolResult

		if unanswered {
			continue
		}

		return session.Seq(i)
	}

	return at
}

// newBranchID names a branched session distinctly from a fresh one, so a
// branch is recognisable in a file listing.
func newBranchID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("branch-%d", time.Now().UnixNano())
	}

	return "branch-" + hex.EncodeToString(buf)
}

func (p *Project) branchPath(sessionID string) string {
	return filepath.Join(p.dir, sessionID+".branch.json")
}
