package session_test

import (
	"errors"
	"testing"

	"censi/harness/internal/session"
)

func clock() session.Clock { return func() int64 { return 1 } }

func msg(role session.Role, text, source string) session.Message {
	return session.Message{Role: role, Content: []session.Block{session.Text(text)}, Source: source}
}

func TestAppendFoldsOntoSurface(t *testing.T) {
	log := session.New(clock())

	sys, err := log.Append(session.EventSystemMessage, msg(session.RoleSystem, "prompt", ""), session.AppendOp())
	if err != nil {
		t.Fatalf("append system: %v", err)
	}
	user, err := log.Append(session.EventUserMessage, msg(session.RoleUser, "hi", "user"), session.AppendOp())
	if err != nil {
		t.Fatalf("append user: %v", err)
	}

	nodes := log.SurfaceNodes()
	if len(nodes) != 2 || nodes[0] != sys.Seq || nodes[1] != user.Seq {
		t.Fatalf("unexpected surface: %v", nodes)
	}
	if log.ReplaceGeneration() != 0 || log.ContentGeneration() != 0 {
		t.Fatalf("append must not advance a generation: replace=%d content=%d",
			log.ReplaceGeneration(), log.ContentGeneration())
	}

	derived := log.DeriveMessages()
	if len(derived) != 2 || derived[0].Content[0].Text != "prompt" {
		t.Fatalf("unexpected derived messages: %+v", derived)
	}
}

func TestReplaceShadowsAndAdvancesBothGenerations(t *testing.T) {
	log := session.New(clock())
	_, _ = log.Append(session.EventSystemMessage, msg(session.RoleSystem, "prompt", ""), session.AppendOp())
	a, _ := log.Append(session.EventUserMessage, msg(session.RoleUser, "one", "user"), session.AppendOp())
	b, _ := log.Append(session.EventUserMessage, msg(session.RoleUser, "two", "user"), session.AppendOp())
	c, _ := log.Append(session.EventUserMessage, msg(session.RoleUser, "three", "user"), session.AppendOp())

	summary, err := log.Append(session.EventUserMessage, msg(session.RoleUser, "[summary]", "compaction"),
		session.ReplaceOp(a.Seq, b.Seq), a.Seq, b.Seq)
	if err != nil {
		t.Fatalf("replace: %v", err)
	}

	nodes := log.SurfaceNodes()
	// Expected surface: the system head, the replacement, and c.
	if len(nodes) != 3 {
		t.Fatalf("expected 3 surface nodes, got %v", nodes)
	}
	if nodes[0] != 0 {
		t.Fatalf("expected the system head to survive at node 0, got %v", nodes)
	}
	if nodes[1] != summary.Seq {
		t.Fatalf("replacement should occupy the range position, got %v", nodes)
	}
	if nodes[2] != c.Seq {
		t.Fatalf("events after the replaced range must survive, got %v", nodes)
	}

	// The log keeps every event; only the projection changed.
	if log.Len() != 5 {
		t.Fatalf("expected 5 durable events, got %d", log.Len())
	}
	if log.ReplaceGeneration() != 1 || log.ContentGeneration() != 1 {
		t.Fatalf("replace must advance both generations: replace=%d content=%d",
			log.ReplaceGeneration(), log.ContentGeneration())
	}
}

// TestHeadIsStructurallyProtected is the load-bearing guard: a replacement may
// not shadow the system node unless it is itself a system message. Policy can be
// forgotten; this cannot.
func TestHeadIsStructurallyProtected(t *testing.T) {
	log := session.New(clock())
	sys, _ := log.Append(session.EventSystemMessage, msg(session.RoleSystem, "prompt", ""), session.AppendOp())
	user, _ := log.Append(session.EventUserMessage, msg(session.RoleUser, "hi", "user"), session.AppendOp())

	_, err := log.Append(session.EventUserMessage, msg(session.RoleUser, "[summary]", "compaction"),
		session.ReplaceOp(sys.Seq, user.Seq), sys.Seq, user.Seq)
	if !errors.Is(err, session.ErrHeadProtected) {
		t.Fatalf("expected ErrHeadProtected, got %v", err)
	}

	// A rejected append must leave the log untouched.
	if log.Len() != 2 || len(log.SurfaceNodes()) != 2 {
		t.Fatalf("rejected append mutated the log: len=%d nodes=%d", log.Len(), len(log.SurfaceNodes()))
	}

	// The carve-out: a system message may replace the head.
	if _, err := log.Append(session.EventSystemMessage, msg(session.RoleSystem, "new prompt", ""),
		session.ReplaceOp(sys.Seq, sys.Seq), sys.Seq); err != nil {
		t.Fatalf("a system message must be allowed to replace the head: %v", err)
	}
}

func TestSurfaceMarkerIsMandatoryAndExclusive(t *testing.T) {
	log := session.New(clock())

	if _, err := log.Append(session.EventUserMessage, msg(session.RoleUser, "hi", "user"), nil); !errors.Is(err, session.ErrSurfaceMarker) {
		t.Fatalf("surface-eligible without an op should fail, got %v", err)
	}
	if _, err := log.Append(session.EventRequestHeader, map[string]any{"model": "default"}, session.AppendOp()); !errors.Is(err, session.ErrSurfaceMarker) {
		t.Fatalf("log-only event with an op should fail, got %v", err)
	}
	if _, err := log.Append(session.EventRequestHeader, map[string]any{"model": "default"}, nil); err != nil {
		t.Fatalf("log-only event without an op should succeed: %v", err)
	}
	// A log-only event must not appear in model history.
	if n := len(log.SurfaceNodes()); n != 0 {
		t.Fatalf("log-only event reached the surface: %d nodes", n)
	}
}

func TestSourceMustReferenceAnEarlierEvent(t *testing.T) {
	log := session.New(clock())
	a, _ := log.Append(session.EventUserMessage, msg(session.RoleUser, "hi", "user"), session.AppendOp())

	if _, err := log.Append(session.EventUserMessage, msg(session.RoleUser, "x", "user"), session.AppendOp(), session.Seq(99)); !errors.Is(err, session.ErrSourceNotEarlier) {
		t.Fatalf("expected ErrSourceNotEarlier, got %v", err)
	}
	// Referencing the immediately preceding event is fine.
	if _, err := log.Append(session.EventUserMessage, msg(session.RoleUser, "y", "user"), session.AppendOp(), a.Seq); err != nil {
		t.Fatalf("valid source reference rejected: %v", err)
	}
}

func TestReplaceRequiresAnExistingSurfaceRange(t *testing.T) {
	log := session.New(clock())
	a, _ := log.Append(session.EventSystemMessage, msg(session.RoleSystem, "prompt", ""), session.AppendOp())

	if _, err := log.Append(session.EventUserMessage, msg(session.RoleUser, "x", "user"),
		session.ReplaceOp(a.Seq, session.Seq(404))); !errors.Is(err, session.ErrUnknownSurfaceRef) {
		t.Fatalf("expected ErrUnknownSurfaceRef, got %v", err)
	}
	if _, err := log.Append(session.EventUserMessage, msg(session.RoleUser, "x", "user"),
		session.ReplaceOp(session.Seq(5), session.Seq(2))); !errors.Is(err, session.ErrBadRange) {
		t.Fatalf("expected ErrBadRange, got %v", err)
	}
}

func TestSystemHeadReportsNodeZero(t *testing.T) {
	log := session.New(clock())
	if _, ok := log.SystemHead(); ok {
		t.Fatal("empty log should have no system head")
	}
	_, _ = log.Append(session.EventUserMessage, msg(session.RoleUser, "hi", "user"), session.AppendOp())
	if _, ok := log.SystemHead(); ok {
		t.Fatal("a user message at node 0 is not a system head")
	}
}
