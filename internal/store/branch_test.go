package store_test

import (
	"testing"

	"censi/harness/internal/session"
	"censi/harness/internal/store"
)

// seed builds a session with alternating user and assistant turns.
func seed(t *testing.T, st *store.Store, workspace string, turns int) *store.Project {
	t.Helper()

	project, err := st.Project(workspace)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}

	for i := 0; i < turns; i++ {
		err := project.Append("session-one",
			session.Event{
				Type: session.EventUserMessage,
				Payload: session.Message{
					Role: session.RoleUser, Content: []session.Block{session.Text("question")},
				},
				Surface: session.AppendOp(),
			},
			session.Event{
				Type: session.EventAssistantMessage,
				Payload: session.Message{
					Role: session.RoleAssistant, Content: []session.Block{session.Text("answer")},
				},
				Surface: session.AppendOp(),
			},
		)
		if err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	return project
}

// TestBranchIsAPrefixOfItsParent is the property the whole design rests on.
//
// A branch has to be byte-identical to its parent up to the branch point, or the
// child cannot reuse the cache the parent already built and would pay to re-read
// a conversation it has already seen.
func TestBranchIsAPrefixOfItsParent(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	project := seed(t, st, "/tmp/ws", 4)

	parent, err := project.Load("session-one")
	if err != nil {
		t.Fatalf("Load parent: %v", err)
	}

	const at = session.Seq(3)

	branchID, err := project.Branch("session-one", at)
	if err != nil {
		t.Fatalf("Branch: %v", err)
	}

	child, err := project.Load(branchID)
	if err != nil {
		t.Fatalf("Load child: %v", err)
	}

	if child.Len() != int(at)+1 {
		t.Fatalf("child has %d events, want %d", child.Len(), int(at)+1)
	}

	parentEvents := parent.Events()

	for i, event := range child.Events() {
		if event.Seq != parentEvents[i].Seq {
			t.Fatalf("event %d: seq = %d, want %d", i, event.Seq, parentEvents[i].Seq)
		}
		if event.Type != parentEvents[i].Type {
			t.Fatalf("event %d: type = %s, want %s", i, event.Type, parentEvents[i].Type)
		}
	}

	// And the derived messages - what actually goes to the provider - match the
	// parent's PREFIX. Comparing against the whole parent would be wrong: the
	// child is deliberately shorter.
	parentMessages := parent.DeriveMessages()
	childMessages := child.DeriveMessages()

	if len(childMessages) == 0 {
		t.Fatal("the child derives no messages")
	}

	if len(childMessages) > len(parentMessages) {
		t.Fatalf("child derives %d messages, more than the parent's %d", len(childMessages), len(parentMessages))
	}

	for i, message := range childMessages {
		want := parentMessages[i]

		if message.Role != want.Role {
			t.Fatalf("message %d: role = %s, want %s", i, message.Role, want.Role)
		}
		if len(message.Content) != len(want.Content) {
			t.Fatalf("message %d: %d blocks, want %d", i, len(message.Content), len(want.Content))
		}
	}
}

// The parent must be untouched. Branching is a read of a prefix plus a write
// elsewhere, never an edit.
func TestBranchLeavesTheParentAlone(t *testing.T) {
	st, _ := store.Open(t.TempDir())
	project := seed(t, st, "/tmp/ws", 3)

	before, err := project.Load("session-one")
	if err != nil {
		t.Fatalf("Load parent: %v", err)
	}

	count := before.Len()

	if _, err := project.Branch("session-one", 2); err != nil {
		t.Fatalf("Branch: %v", err)
	}

	after, err := project.Load("session-one")
	if err != nil {
		t.Fatalf("Load parent after branching: %v", err)
	}

	if after.Len() != count {
		t.Fatalf("parent grew from %d to %d events", count, after.Len())
	}
}

// The origin is recorded so a branch can be traced back rather than appearing as
// an unrelated conversation.
func TestBranchRecordsItsOrigin(t *testing.T) {
	st, _ := store.Open(t.TempDir())
	project := seed(t, st, "/tmp/ws", 3)

	branchID, err := project.Branch("session-one", 4)
	if err != nil {
		t.Fatalf("Branch: %v", err)
	}

	origin, ok := project.BranchOrigin(branchID)
	if !ok {
		t.Fatal("no origin was recorded")
	}

	if origin.SessionID != "session-one" {
		t.Fatalf("origin = %q", origin.SessionID)
	}
	if origin.AtSeq != 4 {
		t.Fatalf("origin seq = %d, want 4", origin.AtSeq)
	}
}

func TestAFreshSessionHasNoOrigin(t *testing.T) {
	st, _ := store.Open(t.TempDir())
	project := seed(t, st, "/tmp/ws", 1)

	if _, ok := project.BranchOrigin("session-one"); ok {
		t.Fatal("a session that was never branched reports an origin")
	}
}

func TestBranchingBeyondTheEndIsRefused(t *testing.T) {
	st, _ := store.Open(t.TempDir())
	project := seed(t, st, "/tmp/ws", 1)

	// Two events exist; asking for the tenth is a client mistake, and silently
	// taking everything would hide it.
	if _, err := project.Branch("session-one", 99); err == nil {
		t.Fatal("a branch point past the end was accepted")
	}
}

func TestBranchingAnUnknownSessionIsRefused(t *testing.T) {
	st, _ := store.Open(t.TempDir())
	project, _ := st.Project("/tmp/ws")

	if _, err := project.Branch("does-not-exist", 0); err == nil {
		t.Fatal("branching an unknown session succeeded")
	}
}

// A branch id must not collide with a fresh session's, or two unrelated
// conversations would appear related.
func TestBranchIDsAreDistinct(t *testing.T) {
	st, _ := store.Open(t.TempDir())
	project := seed(t, st, "/tmp/ws", 2)

	seen := map[string]bool{}

	for i := 0; i < 5; i++ {
		id, err := project.Branch("session-one", 1)
		if err != nil {
			t.Fatalf("Branch: %v", err)
		}

		if seen[id] {
			t.Fatalf("branch id %q was reused", id)
		}

		seen[id] = true
	}
}

// A log cut between a tool call and its result replays into a request the
// provider rejects, with a failure the user did not cause.
func TestBranchDoesNotSplitAToolPair(t *testing.T) {
	st, _ := store.Open(t.TempDir())
	project, _ := st.Project("/tmp/ws")

	_ = project.Append("session-one",
		session.Event{
			Type: session.EventUserMessage,
			Payload: session.Message{
				Role: session.RoleUser, Content: []session.Block{session.Text("read a file")},
			},
			Surface: session.AppendOp(),
		},
		session.Event{
			Type: session.EventAssistantMessage,
			Payload: session.Message{
				Role: session.RoleAssistant,
				Content: []session.Block{
					session.ToolCall("call-1", "read", nil),
				},
			},
			Surface: session.AppendOp(),
		},
		// Branching here would leave the call unanswered.
		session.Event{
			Type: session.EventToolResult,
			Payload: session.Message{
				Role: session.RoleTool, Content: []session.Block{session.ToolResult("call-1", false, "contents")},
			},
			Surface: session.AppendOp(),
		},
	)

	branchID, err := project.Branch("session-one", 1)
	if err != nil {
		t.Fatalf("Branch: %v", err)
	}

	child, err := project.Load(branchID)
	if err != nil {
		t.Fatalf("Load child: %v", err)
	}

	last, ok := child.Events()[child.Len()-1].Message()
	if !ok {
		t.Fatal("the last event is not a message")
	}

	// The last message must not be an assistant turn whose tool call has no
	// result after it.
	if last.Role == session.RoleAssistant && len(last.ToolCalls()) > 0 {
		t.Fatal("the branch ends on an unanswered tool call")
	}
}
