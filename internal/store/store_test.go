package store_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"censi/harness/internal/assemble"
	"censi/harness/internal/session"
	"censi/harness/internal/store"
	"censi/harness/internal/wire"
)

// newStore returns a store rooted in a temporary directory.
func newStore(t *testing.T) (*store.Store, string) {
	t.Helper()

	root := t.TempDir()

	s, err := store.Open(root)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	return s, root
}

// buildSession writes a small conversation into a log.
func buildSession(t *testing.T, log *session.Log) {
	t.Helper()

	// The system head is written the same way the assembler writes it, so the
	// derived history here matches what a live session would have produced.
	if err := assemble.SyncSystem(log, assemble.RenderSystem(
		assemble.Config{Provider: "local", Model: "default", Identity: "You are a coding agent."})); err != nil {
		t.Fatalf("sync system: %v", err)
	}

	for i, text := range []string{"first question", "second question"} {
		if _, err := log.Append(session.EventUserMessage, session.Message{
			Role: session.RoleUser, Content: []session.Block{session.Text(text)},
		}, session.AppendOp()); err != nil {
			t.Fatalf("append user %d: %v", i, err)
		}

		if _, err := log.Append(session.EventAssistantMessage, session.Message{
			Role: session.RoleAssistant, Content: []session.Block{session.Text("answer " + text)},
		}, session.AppendOp()); err != nil {
			t.Fatalf("append assistant %d: %v", i, err)
		}
	}
}

// TestRoundTripPreservesTheDerivedHistory is the baseline: what comes back must
// equal what went in.
func TestRoundTripPreservesTheDerivedHistory(t *testing.T) {
	s, _ := newStore(t)

	project, err := s.Project("/tmp/workspace")
	if err != nil {
		t.Fatalf("project: %v", err)
	}

	original := session.New(nil)
	buildSession(t, original)

	if err := project.Append("s1", original.Events()...); err != nil {
		t.Fatalf("append: %v", err)
	}

	loaded, err := project.Load("s1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	before := original.DeriveMessages()
	after := loaded.DeriveMessages()

	if len(before) != len(after) {
		t.Fatalf("loaded %d messages, wrote %d", len(after), len(before))
	}

	for i := range before {
		if before[i].Role != after[i].Role {
			t.Fatalf("message %d role: %q became %q", i, before[i].Role, after[i].Role)
		}
		if len(before[i].Content) != len(after[i].Content) {
			t.Fatalf("message %d has %d blocks, want %d", i, len(after[i].Content), len(before[i].Content))
		}
		for j := range before[i].Content {
			if before[i].Content[j].Text != after[i].Content[j].Text {
				t.Fatalf("message %d block %d: %q became %q",
					i, j, before[i].Content[j].Text, after[i].Content[j].Text)
			}
		}
	}
}

// TestResumeReproducesAByteIdenticalPrefix is the invariant the whole package
// exists for.
//
// Durability and cache identity are the same claim: if a resumed session cannot
// reconstruct the request prefix the provider cached, then the cache accounting
// is unverifiable and every measurement taken against it is suspect.
func TestResumeReproducesAByteIdenticalPrefix(t *testing.T) {
	s, _ := newStore(t)

	project, err := s.Project("/tmp/workspace")
	if err != nil {
		t.Fatalf("project: %v", err)
	}

	cfg := assemble.Config{
		Provider: "local", Model: "default", Identity: "You are a coding agent.",
		MaxTokens: 4096,
	}

	original := session.New(nil)
	buildSession(t, original)

	// The request the live session would have sent.
	before, err := assemble.Assemble(original, cfg)
	if err != nil {
		t.Fatalf("assemble live: %v", err)
	}

	if err := project.Append("s1", original.Events()...); err != nil {
		t.Fatalf("append: %v", err)
	}

	loaded, err := project.Load("s1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	after, err := assemble.Assemble(loaded, cfg)
	if err != nil {
		t.Fatalf("assemble loaded: %v", err)
	}

	beforeHash, err := wire.Hash(before)
	if err != nil {
		t.Fatalf("hash live: %v", err)
	}
	afterHash, err := wire.Hash(after)
	if err != nil {
		t.Fatalf("hash loaded: %v", err)
	}

	if beforeHash != afterHash {
		t.Fatalf("the resumed session produced a different prefix:\n live: %s\n  resumed: %s",
			beforeHash, afterHash)
	}

	// And byte for byte on the wire, which is what the provider hashes.
	liveWire, err := json.Marshal(wire.Messages(before))
	if err != nil {
		t.Fatalf("marshal live: %v", err)
	}
	resumedWire, err := json.Marshal(wire.Messages(after))
	if err != nil {
		t.Fatalf("marshal resumed: %v", err)
	}

	if string(liveWire) != string(resumedWire) {
		t.Fatalf("the resumed request differs on the wire:\n live: %s\n  resumed: %s",
			liveWire, resumedWire)
	}
}

// TestToolsSurviveResume covers the block types a text-only fixture would miss.
func TestToolsSurviveResume(t *testing.T) {
	s, _ := newStore(t)

	project, err := s.Project("/tmp/workspace")
	if err != nil {
		t.Fatalf("project: %v", err)
	}

	log := session.New(nil)

	if _, err := log.Append(session.EventUserMessage, session.Message{
		Role: session.RoleUser, Content: []session.Block{session.Text("read it")},
	}, session.AppendOp()); err != nil {
		t.Fatalf("append: %v", err)
	}

	if _, err := log.Append(session.EventAssistantMessage, session.Message{
		Role: session.RoleAssistant,
		Content: []session.Block{
			session.ToolCall("call-1", "read", json.RawMessage(`{"path":"a.txt"}`)),
		},
	}, session.AppendOp()); err != nil {
		t.Fatalf("append: %v", err)
	}

	if _, err := log.Append(session.EventToolResult, session.Message{
		Role: session.RoleTool,
		Content: []session.Block{
			session.ToolResult("call-1", false, "contents"),
		},
	}, session.AppendOp()); err != nil {
		t.Fatalf("append: %v", err)
	}

	if err := project.Append("tools", log.Events()...); err != nil {
		t.Fatalf("append: %v", err)
	}

	loaded, err := project.Load("tools")
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	live := log.DeriveMessages()
	resumed := loaded.DeriveMessages()

	if len(live) != len(resumed) {
		t.Fatalf("loaded %d messages, wrote %d", len(resumed), len(live))
	}

	calls := resumed[len(resumed)-2].ToolCalls()
	if len(calls) != 1 {
		t.Fatalf("the tool call was lost: %+v", resumed)
	}
	if calls[0].Name != "read" || calls[0].CallID != "call-1" {
		t.Fatalf("tool call = %+v", calls[0])
	}
	if string(calls[0].Arguments) != `{"path":"a.txt"}` {
		t.Fatalf("arguments = %s", calls[0].Arguments)
	}
}

// TestProjectsAreSeparatePerWorkspace: history is per project, which is the
// whole point of the layout.
func TestProjectsAreSeparatePerWorkspace(t *testing.T) {
	s, _ := newStore(t)

	one, err := s.Project("/tmp/alpha")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	two, err := s.Project("/tmp/beta")
	if err != nil {
		t.Fatalf("project: %v", err)
	}

	if one.ID == two.ID {
		t.Fatal("two workspaces share a project id")
	}

	log := session.New(nil)
	if _, err := log.Append(session.EventUserMessage, session.Message{
		Role: session.RoleUser, Content: []session.Block{session.Text("hello")},
	}, session.AppendOp()); err != nil {
		t.Fatalf("append: %v", err)
	}

	if err := one.Append("a", log.Events()...); err != nil {
		t.Fatalf("append: %v", err)
	}

	sessions, err := two.Sessions()
	if err != nil {
		t.Fatalf("sessions: %v", err)
	}
	if len(sessions) != 0 {
		t.Fatalf("one workspace's session appeared in another: %+v", sessions)
	}
}

// TestSameWorkspaceReachedTwoWaysIsOneProject keeps a symlinked path from
// splitting a project's history in two.
func TestSameWorkspaceReachedTwoWaysIsOneProject(t *testing.T) {
	s, _ := newStore(t)

	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	one, err := s.Project(real)
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	two, err := s.Project(link)
	if err != nil {
		t.Fatalf("project: %v", err)
	}

	if one.ID != two.ID {
		t.Fatalf("a symlinked workspace produced a second project: %s vs %s", one.ID, two.ID)
	}
}

// TestSessionsListing reports what a history sidebar needs.
func TestSessionsListing(t *testing.T) {
	s, _ := newStore(t)

	project, err := s.Project("/tmp/workspace")
	if err != nil {
		t.Fatalf("project: %v", err)
	}

	log := session.New(nil)
	if _, err := log.Append(session.EventUserMessage, session.Message{
		Role: session.RoleUser, Content: []session.Block{session.Text("plan the lending app")},
	}, session.AppendOp()); err != nil {
		t.Fatalf("append: %v", err)
	}

	if err := project.Append("s1", log.Events()...); err != nil {
		t.Fatalf("append: %v", err)
	}

	sessions, err := project.Sessions()
	if err != nil {
		t.Fatalf("sessions: %v", err)
	}

	if len(sessions) != 1 {
		t.Fatalf("got %d sessions, want 1", len(sessions))
	}
	if sessions[0].ID != "s1" {
		t.Fatalf("id = %q", sessions[0].ID)
	}
	if sessions[0].Events != 1 {
		t.Fatalf("events = %d, want 1", sessions[0].Events)
	}
	if !strings.Contains(sessions[0].Preview, "plan the lending app") {
		t.Fatalf("preview = %q", sessions[0].Preview)
	}
}

// TestTruncatedFinalLineKeepsTheRest: a crash mid-write must not discard a whole
// conversation.
func TestTruncatedFinalLineKeepsTheRest(t *testing.T) {
	s, root := newStore(t)

	project, err := s.Project("/tmp/workspace")
	if err != nil {
		t.Fatalf("project: %v", err)
	}

	log := session.New(nil)
	buildSession(t, log)

	if err := project.Append("s1", log.Events()...); err != nil {
		t.Fatalf("append: %v", err)
	}

	path := filepath.Join(root, "projects", project.ID, "sessions", "s1.jsonl")

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	// Simulate a crash partway through the final line.
	trimmed := data[:len(data)-12]
	if err := os.WriteFile(path, trimmed, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	loaded, err := project.Load("s1")
	if err != nil {
		t.Fatalf("a truncated final line must not fail the load: %v", err)
	}

	if len(loaded.Events()) == 0 {
		t.Fatal("the whole log was discarded for one damaged line")
	}
	if len(loaded.Events()) >= len(log.Events()) {
		t.Fatal("the damaged line was somehow kept")
	}
}

// TestSessionIDCannotEscapeTheProject: an id becomes part of a path.
func TestSessionIDCannotEscapeTheProject(t *testing.T) {
	s, root := newStore(t)

	project, err := s.Project("/tmp/workspace")
	if err != nil {
		t.Fatalf("project: %v", err)
	}

	log := session.New(nil)
	if _, err := log.Append(session.EventUserMessage, session.Message{
		Role: session.RoleUser, Content: []session.Block{session.Text("hi")},
	}, session.AppendOp()); err != nil {
		t.Fatalf("append: %v", err)
	}

	if err := project.Append("../../escaped", log.Events()...); err != nil {
		t.Fatalf("append: %v", err)
	}

	// Nothing may exist outside the project's sessions directory.
	var found bool

	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if strings.Contains(path, "escaped") && !strings.Contains(path, "sessions") {
			found = true
		}

		return nil
	})

	if found {
		t.Fatal("a session id escaped the project directory")
	}
}

// TestProjectsListingFindsHistoryTheAppWouldShow.
func TestProjectsListingFindsHistory(t *testing.T) {
	s, _ := newStore(t)

	for _, workspace := range []string{"/tmp/alpha", "/tmp/beta"} {
		if _, err := s.Project(workspace); err != nil {
			t.Fatalf("project %s: %v", workspace, err)
		}
	}

	projects, err := s.Projects()
	if err != nil {
		t.Fatalf("projects: %v", err)
	}

	if len(projects) != 2 {
		t.Fatalf("got %d projects, want 2", len(projects))
	}

	for _, project := range projects {
		if project.Workspace == "" {
			t.Fatal("a project lost its readable workspace path")
		}
	}
}
