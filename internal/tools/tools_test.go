package tools_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"censi/harness/internal/tools"
)

// stub is a minimal Tool for exercising the registry.
type stub struct {
	def tools.Definition
}

func (s stub) Definition() tools.Definition { return s.def }

func (s stub) Execute(_ context.Context, _ json.RawMessage) (tools.Result, error) {
	return tools.Text("ok"), nil
}

func newStub(name, params string) stub {
	return stub{def: tools.Definition{
		Name:        name,
		Description: "A stub named " + name + ".",
		Parameters:  json.RawMessage(params),
		ReadOnly:    true,
	}}
}

// TestSchemaOrderDoesNotDependOnRegistrationOrder is the load-bearing property.
// Registration order is a load-time artifact; if it leaked into the schema list,
// S0 would reshuffle between processes and invalidate every live session's
// cached prefix.
func TestSchemaOrderDoesNotDependOnRegistrationOrder(t *testing.T) {
	build := func(names ...string) []string {
		r := tools.NewRegistry()
		for _, n := range names {
			if err := r.Register(newStub(n, `{"type":"object"}`)); err != nil {
				t.Fatalf("register %s: %v", n, err)
			}
		}

		out := make([]string, 0)
		for _, s := range r.Schemas() {
			out = append(out, s.Name)
		}
		return out
	}

	a := build("write", "bash", "read", "grep")
	b := build("grep", "read", "write", "bash")

	if strings.Join(a, ",") != strings.Join(b, ",") {
		t.Fatalf("schema order depends on registration order:\n %v\n %v", a, b)
	}
	if strings.Join(a, ",") != "bash,grep,read,write" {
		t.Fatalf("expected code-unit order, got %v", a)
	}
}

// TestParameterJSONIsCanonicalised proves the declaration bytes are a function
// of the value, not of how the literal happened to be written. Without this, a
// reformatted schema literal would silently change S0.
func TestParameterJSONIsCanonicalised(t *testing.T) {
	// Same schema, different key order, different whitespace.
	verbose := `{
		"required": ["path"],
		"type": "object",
		"properties": { "path": { "type": "string" } }
	}`
	compact := `{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`

	first := tools.NewRegistry()
	if err := first.Register(newStub("read", verbose)); err != nil {
		t.Fatalf("register verbose: %v", err)
	}

	second := tools.NewRegistry()
	if err := second.Register(newStub("read", compact)); err != nil {
		t.Fatalf("register compact: %v", err)
	}

	a, err := json.Marshal(first.Schemas())
	if err != nil {
		t.Fatalf("marshal a: %v", err)
	}
	b, err := json.Marshal(second.Schemas())
	if err != nil {
		t.Fatalf("marshal b: %v", err)
	}

	if string(a) != string(b) {
		t.Fatalf("equivalent schemas produced different bytes:\n %s\n %s", a, b)
	}
	if strings.Contains(string(a), "\n") {
		t.Fatalf("canonical parameters should be compact: %s", a)
	}
}

// TestDuplicateRegistrationIsRejected: a silent replace would change S0
// mid-session, and the caller that lost is unlikely to be the one that notices.
func TestDuplicateRegistrationIsRejected(t *testing.T) {
	r := tools.NewRegistry()
	if err := r.Register(newStub("read", `{"type":"object"}`)); err != nil {
		t.Fatalf("first register: %v", err)
	}

	if err := r.Register(newStub("read", `{"type":"object"}`)); err == nil {
		t.Fatal("a duplicate tool name must be rejected")
	}
	if err := r.Register(newStub("write", `not json`)); err == nil {
		t.Fatal("invalid parameter JSON must be rejected")
	}
	if err := r.Register(newStub("write", `["not","an","object"]`)); err == nil {
		t.Fatal("a non-object parameter schema must be rejected")
	}
	if err := r.Register(stub{def: tools.Definition{Name: "x", Parameters: json.RawMessage(`{}`)}}); err == nil {
		t.Fatal("a tool without a description must be rejected")
	}
}

// TestUnknownToolIsAModelErrorNotAHarnessError: the model may hallucinate a
// tool, and telling it so is the correct response rather than failing the step.
func TestUnknownToolIsAModelErrorNotAHarnessError(t *testing.T) {
	r := tools.NewRegistry()

	result, err := r.Execute(context.Background(), "nope", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("an unknown tool must not be a Go error: %v", err)
	}
	if !result.IsError {
		t.Fatal("an unknown tool must be reported as an error result")
	}
	if !strings.Contains(result.Content[0].Text, "nope") {
		t.Fatalf("the message should name the tool: %q", result.Content[0].Text)
	}
}

// TestDeferLoadSurvivesRegistration keeps the deferred flag on the wire, so a
// rarely-used schema can stay out of the hot prefix.
func TestDeferLoadSurvivesRegistration(t *testing.T) {
	r := tools.NewRegistry()
	def := tools.Definition{
		Name:        "rare",
		Description: "Rarely used.",
		Parameters:  json.RawMessage(`{"type":"object"}`),
		DeferLoad:   true,
	}
	if err := r.Register(stub{def: def}); err != nil {
		t.Fatalf("register: %v", err)
	}

	if !r.Schemas()[0].DeferLoad {
		t.Fatal("the deferred flag must survive registration")
	}
}
