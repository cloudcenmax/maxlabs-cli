package assemble_test

import (
	"testing"

	"censi/harness/internal/assemble"
	"censi/harness/internal/session"
	"censi/harness/internal/wire"
)

// fixedClock keeps the log byte-deterministic for these assertions. Note that
// event Time never reaches the wire, so even a real clock could not affect the
// prefix; this is belt and braces.
func fixedClock() session.Clock { return func() int64 { return 1_700_000_000_000 } }

func baseConfig() assemble.Config {
	return assemble.Config{
		Provider: "local",
		Model:    "default",
		Identity: "You are an AI coding agent.",
		Guidance: []assemble.Section{
			{Name: "tool:bash", Order: 1000, Text: "Prefer reading files before editing them."},
		},
		Tools: []wire.ToolSchema{
			{Name: "bash", Description: "Run a shell command.", Parameters: []byte(`{"type":"object"}`)},
			{Name: "read", Description: "Read a file.", Parameters: []byte(`{"type":"object"}`)},
		},
	}
}

// step drives one model step: reconcile the prompt, assemble, and record the
// assistant reply as history.
//
// The system head is reserved BEFORE any user message, exactly as the loop does:
// "the first admitted step reserves the system head even for an empty prompt".
// Reserving second would leave a user message at node 0 and forfeit the head
// protection that keeps compaction away from the cached prefix.
func step(t *testing.T, log *session.Log, cfg assemble.Config, userText string) wire.Request {
	t.Helper()

	if err := assemble.SyncSystem(log, assemble.RenderSystem(cfg)); err != nil {
		t.Fatalf("sync system: %v", err)
	}

	if userText != "" {
		if _, err := log.Append(session.EventUserMessage, session.Message{
			Role:    session.RoleUser,
			Content: []session.Block{session.Text(userText)},
			Source:  "user",
		}, session.AppendOp()); err != nil {
			t.Fatalf("append user message: %v", err)
		}
	}

	req, err := assemble.Assemble(log, cfg)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	return req
}

// TestPrefixExtensionProperty is the M0 acceptance test: within a request
// series, every request's unit sequence must extend its predecessor's.
//
// This single property catches essentially every cache regression - prompt
// interpolation, tool reordering, message rewriting, and truncation drift.
func TestPrefixExtensionProperty(t *testing.T) {
	log := session.New(fixedClock())
	cfg := baseConfig()

	var prev []wire.Unit
	for stepNo := 0; stepNo < 40; stepNo++ {
		userText := ""
		if stepNo == 0 {
			userText = "Say hello."
		} else {
			userText = "Continue."
		}

		req := step(t, log, cfg, userText)

		units, err := wire.Units(req)
		if err != nil {
			t.Fatalf("step %d: units: %v", stepNo, err)
		}

		if prev != nil {
			ok, divergeAt := wire.Extends(prev, units)
			if !ok {
				t.Fatalf("step %d: prefix-extension property violated at unit %d (%s)",
					stepNo, divergeAt, describeUnit(units, divergeAt))
			}
		}
		prev = units

		// The model answers; the reply becomes history for the next step.
		if _, err := log.Append(session.EventAssistantMessage, session.Message{
			Role:    session.RoleAssistant,
			Content: []session.Block{session.Text("ok")},
		}, session.AppendOp()); err != nil {
			t.Fatalf("append assistant: %v", err)
		}
	}
}

// TestRequestDeterminism proves L2: the same log and config reconstruct the same
// request bytes. This is what makes resume safe and cache behaviour auditable.
func TestRequestDeterminism(t *testing.T) {
	build := func() wire.Request {
		log := session.New(fixedClock())
		cfg := baseConfig()
		req := step(t, log, cfg, "Do the thing.")
		if _, err := log.Append(session.EventAssistantMessage, session.Message{
			Role:    session.RoleAssistant,
			Content: []session.Block{session.Text("done")},
		}, session.AppendOp()); err != nil {
			t.Fatalf("append assistant: %v", err)
		}
		req = step(t, log, cfg, "Again.")
		return req
	}

	a, b := build(), build()

	ha, err := wire.Hash(a)
	if err != nil {
		t.Fatalf("hash a: %v", err)
	}
	hb, err := wire.Hash(b)
	if err != nil {
		t.Fatalf("hash b: %v", err)
	}
	if ha != hb {
		t.Fatalf("request is not deterministic:\n a=%s\n b=%s", ha, hb)
	}
}

// TestVolatileContextGoesToTail is the mutation test for L6: a value that
// changes every step must be appended at the tail, never interpolated into the
// head. Appending preserves the prefix; interpolating would not.
func TestVolatileContextGoesToTail(t *testing.T) {
	log := session.New(fixedClock())
	cfg := baseConfig()

	first := step(t, log, cfg, "Hello.")
	firstUnits, err := wire.Units(first)
	if err != nil {
		t.Fatalf("units: %v", err)
	}

	// Two steps, each with a different runtime-context snapshot.
	for _, snapshot := range []string{"cwd=/repo branch=main", "cwd=/repo branch=feat"} {
		appended, err := assemble.NoteRuntimeContext(log, snapshot)
		if err != nil {
			t.Fatalf("note runtime context: %v", err)
		}
		if !appended {
			t.Fatalf("expected %q to be appended", snapshot)
		}
		if _, err := log.Append(session.EventAssistantMessage, session.Message{
			Role:    session.RoleAssistant,
			Content: []session.Block{session.Text("ok")},
		}, session.AppendOp()); err != nil {
			t.Fatalf("append assistant: %v", err)
		}
	}

	after, err := assemble.Assemble(log, cfg)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	afterUnits, err := wire.Units(after)
	if err != nil {
		t.Fatalf("units: %v", err)
	}

	ok, divergeAt := wire.Extends(firstUnits, afterUnits)
	if !ok {
		t.Fatalf("volatile context broke the prefix at unit %d; it must be appended at the tail", divergeAt)
	}
}

// TestRuntimeContextChangeSuppression proves identical snapshots are not
// re-appended; otherwise history would accumulate duplicates on every step.
func TestRuntimeContextChangeSuppression(t *testing.T) {
	log := session.New(fixedClock())

	appended, err := assemble.NoteRuntimeContext(log, "cwd=/repo")
	if err != nil || !appended {
		t.Fatalf("first snapshot should append: appended=%v err=%v", appended, err)
	}
	appended, err = assemble.NoteRuntimeContext(log, "cwd=/repo")
	if err != nil {
		t.Fatalf("second snapshot: %v", err)
	}
	if appended {
		t.Fatal("an unchanged snapshot must not be appended")
	}
}

// TestGuidanceReorderBreaksPrefix is a mutation test: it proves the
// prefix-extension assertion is actually load-bearing. If this test ever stops
// failing, the property test has been weakened.
func TestGuidanceReorderBreaksPrefix(t *testing.T) {
	log := session.New(fixedClock())
	cfg := baseConfig()

	before := step(t, log, cfg, "Hello.")
	beforeUnits, err := wire.Units(before)
	if err != nil {
		t.Fatalf("units: %v", err)
	}

	// Append a guidance section at an order that precedes the existing one. A
	// new section early in the system prompt must invalidate the prefix from
	// its first changed token - that is the cost the ordering rules exist to
	// make deliberate.
	mutated := baseConfig()
	mutated.Guidance = append(mutated.Guidance, assemble.Section{
		Name:  "mode:plan",
		Order: assemble.OrderPlanPolicy,
		Text:  "You are in plan mode.",
	})

	after := step(t, log, mutated, "")
	afterUnits, err := wire.Units(after)
	if err != nil {
		t.Fatalf("units: %v", err)
	}

	ok, divergeAt := wire.Extends(beforeUnits, afterUnits)
	if ok {
		t.Fatal("adding an earlier guidance section should NOT preserve the prefix; the property test would be vacuous")
	}
	if divergeAt > 1 {
		t.Fatalf("expected divergence within the first two units (tools, system), got unit %d", divergeAt)
	}
}

// TestToolOrderIsCanonical proves S0 is independent of registration order, which
// is a load-time artifact.
func TestToolOrderIsCanonical(t *testing.T) {
	a := []wire.ToolSchema{{Name: "bash"}, {Name: "read"}, {Name: "grep"}}
	b := []wire.ToolSchema{{Name: "grep"}, {Name: "bash"}, {Name: "read"}}

	ca, err := assemble.CanonicalizeTools(a, nil)
	if err != nil {
		t.Fatalf("canonicalize a: %v", err)
	}
	cb, err := assemble.CanonicalizeTools(b, nil)
	if err != nil {
		t.Fatalf("canonicalize b: %v", err)
	}

	if len(ca) != len(cb) {
		t.Fatalf("length mismatch: %d vs %d", len(ca), len(cb))
	}
	for i := range ca {
		if ca[i].Name != cb[i].Name {
			t.Fatalf("tool order depends on registration order: %v vs %v", names(ca), names(cb))
		}
	}

	// An explicit order must be honoured.
	co, err := assemble.CanonicalizeTools(b, []string{"read", "grep", "bash"})
	if err != nil {
		t.Fatalf("canonicalize ordered: %v", err)
	}
	if got := names(co); got[0] != "read" || got[1] != "grep" || got[2] != "bash" {
		t.Fatalf("explicit order not honoured: %v", got)
	}

	// A name that does not exist is an error, not a silent omission.
	if _, err := assemble.CanonicalizeTools(a, []string{"nope"}); err == nil {
		t.Fatal("expected an error for an unregistered tool in the order list")
	}
}

// TestEmptyRenderingProducesNoSystemUnit proves an empty prompt produces no
// system message rather than an empty one, matching the "reserve node 0 without
// a wire message" rule.
func TestEmptyRenderingProducesNoSystemUnit(t *testing.T) {
	log := session.New(fixedClock())
	cfg := assemble.Config{Provider: "local", Model: "default"}

	if err := assemble.SyncSystem(log, ""); err != nil {
		t.Fatalf("sync system: %v", err)
	}
	req, err := assemble.Assemble(log, cfg)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if req.System != "" {
		t.Fatalf("expected no system text, got %q", req.System)
	}

	units, err := wire.Units(req)
	if err != nil {
		t.Fatalf("units: %v", err)
	}
	for _, u := range units {
		if u.Kind == wire.UnitSystem {
			t.Fatal("an empty rendering must not emit a system unit")
		}
	}
}

func names(tools []wire.ToolSchema) []string {
	out := make([]string, len(tools))
	for i, t := range tools {
		out[i] = t.Name
	}
	return out
}

func describeUnit(units []wire.Unit, i int) string {
	if i >= len(units) {
		return "beyond end of request"
	}
	return string(units[i].Kind)
}
