package wire_test

import (
	"testing"

	"censi/harness/internal/session"
	"censi/harness/internal/wire"
)

func toolCall(id, name string) session.Block {
	return session.ToolCall(id, name, nil)
}

func toolResult(id string) session.Block {
	return session.ToolResult(id, false, "output")
}

// TestAnUnansweredToolCallIsDroppedFromTheRequest is the fix for a model that
// returned nothing at all.
//
// A turn interrupted between the model asking for a tool and the tool running
// leaves an assistant message whose calls have no results. One provider
// tolerates that; another rejects the entire request with a 400 naming no field,
// so every later turn in that conversation fails - and because the log is
// append-only, it fails forever.
func TestAnUnansweredToolCallIsDroppedFromTheRequest(t *testing.T) {
	req := wire.Request{
		Messages: []session.Message{
			{Role: session.RoleUser, Content: []session.Block{session.Text("read it")}},
			{
				Role:    session.RoleAssistant,
				Content: []session.Block{toolCall("c1", "read")},
			},
			{Role: session.RoleUser, Content: []session.Block{session.Text("you there?")}},
		},
	}

	out := wire.Messages(req)

	for _, message := range out {
		if len(message.ToolCalls) > 0 {
			t.Fatalf("an unanswered tool call survived: %+v", message.ToolCalls)
		}
	}

	// The user messages are untouched.
	var text int

	for _, message := range out {
		if message.Role == session.RoleUser {
			text++
		}
	}

	if text != 2 {
		t.Fatalf("user messages = %d, want 2", text)
	}
}

// A paired call is the ordinary case and must be left alone.
func TestAPairedToolCallIsKept(t *testing.T) {
	req := wire.Request{
		Messages: []session.Message{
			{Role: session.RoleUser, Content: []session.Block{session.Text("read it")}},
			{Role: session.RoleAssistant, Content: []session.Block{toolCall("c1", "read")}},
			{Role: session.RoleTool, Content: []session.Block{toolResult("c1")}},
		},
	}

	out := wire.Messages(req)

	var calls, results int

	for _, message := range out {
		calls += len(message.ToolCalls)

		if message.Role == session.RoleTool {
			results++
		}
	}

	if calls != 1 || results != 1 {
		t.Fatalf("calls = %d, results = %d, want 1 and 1", calls, results)
	}
}

// Only the unanswered call is removed; a turn that made two and got one answer
// keeps the answered one.
func TestOnlyTheUnansweredCallIsDropped(t *testing.T) {
	req := wire.Request{
		Messages: []session.Message{
			{Role: session.RoleUser, Content: []session.Block{session.Text("both")}},
			{
				Role: session.RoleAssistant,
				Content: []session.Block{
					session.Text("Looking."),
					toolCall("c1", "read"),
					toolCall("c2", "grep"),
				},
			},
			{Role: session.RoleTool, Content: []session.Block{toolResult("c1")}},
		},
	}

	out := wire.Messages(req)

	var kept []string

	for _, message := range out {
		for _, call := range message.ToolCalls {
			kept = append(kept, call.ID)
		}
	}

	if len(kept) != 1 || kept[0] != "c1" {
		t.Fatalf("kept = %v, want just c1", kept)
	}
}

// An orphan result is equally invalid: the provider cannot pair it with a call.
func TestAnOrphanResultIsDropped(t *testing.T) {
	req := wire.Request{
		Messages: []session.Message{
			{Role: session.RoleUser, Content: []session.Block{session.Text("hi")}},
			{Role: session.RoleTool, Content: []session.Block{toolResult("ghost")}},
		},
	}

	out := wire.Messages(req)

	for _, message := range out {
		if message.Role == session.RoleTool {
			t.Fatal("an orphan tool result survived")
		}
	}
}

// A message left with neither content nor calls is itself a rejected shape, so
// it goes rather than being sent empty.
func TestAnEmptiedAssistantTurnIsRemoved(t *testing.T) {
	req := wire.Request{
		Messages: []session.Message{
			{Role: session.RoleUser, Content: []session.Block{session.Text("hi")}},
			{Role: session.RoleAssistant, Content: []session.Block{toolCall("c1", "read")}},
		},
	}

	out := wire.Messages(req)

	for _, message := range out {
		if message.Role == session.RoleAssistant {
			t.Fatalf("an empty assistant turn survived: %+v", message)
		}
	}
}

// An assistant turn that also said something keeps its prose.
func TestProseSurvivesTheCallBeingDropped(t *testing.T) {
	req := wire.Request{
		Messages: []session.Message{
			{Role: session.RoleUser, Content: []session.Block{session.Text("hi")}},
			{
				Role: session.RoleAssistant,
				Content: []session.Block{
					session.Text("I was about to look."),
					toolCall("c1", "read"),
				},
			},
		},
	}

	out := wire.Messages(req)

	for _, message := range out {
		if message.Role != session.RoleAssistant {
			continue
		}

		if len(message.ToolCalls) != 0 {
			t.Fatal("the unanswered call survived")
		}

		if message.Content != "I was about to look." {
			t.Fatalf("content = %v, want the prose", message.Content)
		}

		return
	}

	t.Fatal("the assistant turn was dropped entirely")
}
