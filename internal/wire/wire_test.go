package wire_test

import (
	"encoding/json"
	"strings"
	"testing"

	"censi/harness/internal/session"
	"censi/harness/internal/wire"
)

func msg(role session.Role, text string) session.Message {
	return session.Message{Role: role, Content: []session.Block{session.Text(text)}}
}

func request(messages ...session.Message) wire.Request {
	return wire.Request{
		Provider: "local",
		Model:    "default",
		System:   "You are a coding agent.",
		Messages: messages,
	}
}

// TestHashIsDeterministic is the core guarantee: identical prompts hash
// identically, which is what makes byte-for-byte reconstruction checkable.
func TestHashIsDeterministic(t *testing.T) {
	a, err := wire.Hash(request(msg(session.RoleUser, "hello")))
	if err != nil {
		t.Fatalf("hash a: %v", err)
	}
	b, err := wire.Hash(request(msg(session.RoleUser, "hello")))
	if err != nil {
		t.Fatalf("hash b: %v", err)
	}
	if a != b {
		t.Fatalf("identical requests hashed differently: %s vs %s", a, b)
	}

	c, err := wire.Hash(request(msg(session.RoleUser, "hello!")))
	if err != nil {
		t.Fatalf("hash c: %v", err)
	}
	if a == c {
		t.Fatal("different prompts must not hash the same")
	}
}

// TestUnitsFramesThePrompt proves the unit decomposition: an empty system prompt
// emits no system unit, so an empty rendering costs no wire bytes.
func TestUnitsFramesThePrompt(t *testing.T) {
	r := request(msg(session.RoleUser, "hi"))
	r.System = ""
	units, err := wire.Units(r)
	if err != nil {
		t.Fatalf("units: %v", err)
	}
	if len(units) != 1 || units[0].Kind != wire.UnitMessage {
		t.Fatalf("expected exactly one message unit, got %+v", units)
	}

	r.System = "prompt"
	r.Tools = []wire.ToolSchema{{Name: "bash", Description: "run"}}
	units, err = wire.Units(r)
	if err != nil {
		t.Fatalf("units: %v", err)
	}
	// Tools are segment S0 and therefore come first on the wire.
	if units[0].Kind != wire.UnitTools {
		t.Fatalf("tools must be the first unit, got %s", units[0].Kind)
	}
	if units[1].Kind != wire.UnitSystem {
		t.Fatalf("the system prompt must follow tools, got %s", units[1].Kind)
	}
	if units[2].Kind != wire.UnitMessage || units[2].Index != 0 {
		t.Fatalf("messages must follow the system prompt, got %+v", units[2])
	}
}

// TestExtendsDetectsAppendOnlyGrowth is the positive case for the property test.
func TestExtendsDetectsAppendOnlyGrowth(t *testing.T) {
	prev, err := wire.Units(request(msg(session.RoleUser, "one")))
	if err != nil {
		t.Fatalf("units: %v", err)
	}
	next, err := wire.Units(request(
		msg(session.RoleUser, "one"),
		msg(session.RoleAssistant, "ok"),
	))
	if err != nil {
		t.Fatalf("units: %v", err)
	}

	ok, at := wire.Extends(prev, next)
	if !ok {
		t.Fatalf("append-only growth must extend the prefix; diverged at %d", at)
	}
}

// TestExtendsDetectsHeadRewrite is the negative case, and it pins the failure
// index: a changed head must be reported at the system unit, not somewhere
// further along.
func TestExtendsDetectsHeadRewrite(t *testing.T) {
	prev, err := wire.Units(request(msg(session.RoleUser, "one")))
	if err != nil {
		t.Fatalf("units: %v", err)
	}

	rewritten := request(msg(session.RoleUser, "one"))
	rewritten.System = "A completely different prompt."
	next, err := wire.Units(rewritten)
	if err != nil {
		t.Fatalf("units: %v", err)
	}

	ok, at := wire.Extends(prev, next)
	if ok {
		t.Fatal("a rewritten system prompt must break prefix extension")
	}
	if at != 0 {
		t.Fatalf("expected divergence at unit 0 (the system prompt), got %d", at)
	}
}

// TestExtendsDetectsTruncation covers the degenerate case: a shorter request is
// not an extension of a longer one.
func TestExtendsDetectsTruncation(t *testing.T) {
	long, err := wire.Units(request(
		msg(session.RoleUser, "one"),
		msg(session.RoleAssistant, "ok"),
	))
	if err != nil {
		t.Fatalf("units: %v", err)
	}
	short, err := wire.Units(request(msg(session.RoleUser, "one")))
	if err != nil {
		t.Fatalf("units: %v", err)
	}

	if ok, _ := wire.Extends(long, short); ok {
		t.Fatal("a shorter request must not extend a longer one")
	}
}

// TestPromptIsStableAcrossCalls guards against any hidden nondeterminism - a map
// iteration, a clock read - leaking into the prompt bytes.
func TestPromptIsStableAcrossCalls(t *testing.T) {
	r := request(
		msg(session.RoleUser, "one"),
		msg(session.RoleAssistant, "ok"),
		msg(session.RoleUser, "two"),
	)
	first, err := wire.Prompt(r)
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	for i := 0; i < 50; i++ {
		again, err := wire.Prompt(r)
		if err != nil {
			t.Fatalf("prompt %d: %v", i, err)
		}
		if string(again) != string(first) {
			t.Fatalf("prompt bytes are not stable across calls at iteration %d", i)
		}
	}
}

// TestSingleTextBlockEncodesAsAString pins the portability rule. The array form
// of `content` is markedly less portable, and the failures cluster on the system
// role - which is exactly where our reusable prefix lives. Text-only messages
// must therefore travel as plain strings.
func TestSingleTextBlockEncodesAsAString(t *testing.T) {
	r := request(msg(session.RoleUser, "hello"))
	r.System = "You are a coding agent."

	raw, err := json.Marshal(wire.Messages(r))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded []map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(decoded) != 2 {
		t.Fatalf("expected system + user, got %d messages", len(decoded))
	}
	for i, m := range decoded {
		if _, ok := m["content"].(string); !ok {
			t.Fatalf("message %d content is %T, want a plain string", i, m["content"])
		}
	}
	if decoded[0]["content"] != "You are a coding agent." {
		t.Fatalf("unexpected system content: %v", decoded[0]["content"])
	}
}

// TestMultipleBlocksEncodeAsAnArray proves the array form is still available
// when it carries information a string cannot.
func TestMultipleBlocksEncodeAsAnArray(t *testing.T) {
	m := session.Message{
		Role: session.RoleUser,
		Content: []session.Block{
			session.Text("first"),
			session.Text("second"),
		},
	}

	raw, err := json.Marshal(wire.Messages(wire.Request{Messages: []session.Message{m}}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded []map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if _, ok := decoded[0]["content"].([]any); !ok {
		t.Fatalf("content is %T, want an array when there are several blocks", decoded[0]["content"])
	}
}

// TestProvenanceTagNeverReachesTheWire guards two things at once: no provider
// schema defines `source`, and reconstruction reads the log rather than the wire,
// so withholding it loses nothing.
func TestProvenanceTagNeverReachesTheWire(t *testing.T) {
	m := session.Message{
		Role:    session.RoleUser,
		Content: []session.Block{session.Text("cwd=/repo")},
		Source:  "runtime-context",
	}

	raw, err := json.Marshal(wire.Messages(wire.Request{Messages: []session.Message{m}}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if strings.Contains(string(raw), "runtime-context") {
		t.Fatalf("the provenance tag leaked onto the wire: %s", raw)
	}
	if strings.Contains(string(raw), `"source"`) {
		t.Fatalf("no source field may appear on the wire: %s", raw)
	}

	// The unit encoding must agree with the message encoding.
	units, err := wire.Units(wire.Request{Messages: []session.Message{m}})
	if err != nil {
		t.Fatalf("units: %v", err)
	}
	if strings.Contains(string(units[0].Bytes), `"source"`) {
		t.Fatalf("the unit encoding leaked the provenance tag: %s", units[0].Bytes)
	}
}
