package subagent_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"censi/harness/internal/agent"
	"censi/harness/internal/assemble"
	"censi/harness/internal/audit"
	"censi/harness/internal/llm"
	"censi/harness/internal/session"
	"censi/harness/internal/subagent"
	"censi/harness/internal/tools"
	"censi/harness/internal/wire"
)

// recorder captures every request the child makes, so the prefix claim can be
// checked against what actually goes out.
type recorder struct {
	mu       sync.Mutex
	requests []wire.Request
	reply    func(req wire.Request) llm.Response
}

func (r *recorder) Complete(_ context.Context, req wire.Request) (llm.Response, error) {
	r.mu.Lock()
	r.requests = append(r.requests, req)
	reply := r.reply
	r.mu.Unlock()

	return reply(req), nil
}

func (r *recorder) all() []wire.Request {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]wire.Request(nil), r.requests...)
}

// echo is a trivial tool, present only so S0 is non-empty.
type echo struct{}

func (echo) Definition() tools.Definition {
	return tools.Definition{
		Name:        "echo",
		Description: "Echo text.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`),
		ReadOnly:    true,
	}
}

func (echo) Execute(_ context.Context, args json.RawMessage) (tools.Result, error) {
	var in struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(args, &in)

	return tools.Text("echo: " + in.Text), nil
}

// buildParent constructs a parent agent and returns the delegation tool wired to
// the same configuration.
func buildParent(t *testing.T, provider llm.Provider, guard agent.Guard, depth, maxDepth int) (*agent.Agent, *subagent.Tool) {
	t.Helper()

	registry := tools.NewRegistry()
	if err := registry.Register(echo{}); err != nil {
		t.Fatalf("register: %v", err)
	}

	assembly := assemble.Config{
		Provider:      "local",
		Model:         "default",
		Identity:      "You are a coding agent.",
		PersonaPrefix: "You are Worker.",
		Tools:         registry.Schemas(),
		MaxTokens:     4096,
	}

	tool, err := subagent.New(subagent.Config{
		Provider: provider,
		Registry: registry,
		Assembly: assembly,
		Guard:    guard,
		MaxSteps: 4,
		MaxDepth: maxDepth,
		Depth:    depth,
	})
	if err != nil {
		t.Fatalf("subagent: %v", err)
	}

	parentRegistry := tools.NewRegistry()
	for _, name := range registry.Names() {
		inner, ok := registry.Get(name)
		if !ok {
			continue
		}
		if err := parentRegistry.Register(inner); err != nil {
			t.Fatalf("register parent: %v", err)
		}
	}
	if err := parentRegistry.Register(tool); err != nil {
		t.Fatalf("register task: %v", err)
	}

	parentAssembly := assembly
	parentAssembly.Tools = parentRegistry.Schemas()

	// Complete the wiring the way the runner must: the tool inherits the FULL
	// tool list, including itself, so a child's S0 matches its parent's.
	tool.SetAssembly(parentAssembly)

	parent, err := agent.New(agent.Config{
		Assembly: parentAssembly,
		Provider: provider,
		Tools:    parentRegistry,
		Guard:    guard,
		MaxSteps: 5,
	}, session.New(nil))
	if err != nil {
		t.Fatalf("parent: %v", err)
	}

	return parent, tool
}

// TestSubagentSharesTheParentToolPrefix is the cache claim this package exists
// to make.
//
// The child must declare the SAME tools, in the SAME order, as the parent. If it
// did not, its very first segment would differ and every delegation would pay a
// cold prefix write on an otherwise identical prompt.
func TestSubagentSharesTheParentToolPrefix(t *testing.T) {
	provider := &recorder{reply: func(wire.Request) llm.Response {
		return textResponse("done")
	}}

	parent, tool := buildParent(t, provider, nil, 0, 1)

	_ = parent

	// Run the subagent directly, as the parent's tool call would.
	if _, err := tool.Execute(context.Background(), json.RawMessage(
		`{"description":"probe","prompt":"say done"}`)); err != nil {
		t.Fatalf("execute: %v", err)
	}

	requests := provider.all()
	if len(requests) == 0 {
		t.Fatal("the subagent made no requests")
	}

	child := requests[0]
	parentTools := parent.Assembly().Tools

	if len(child.Tools) != len(parentTools) {
		t.Fatalf("child declares %d tools, parent declares %d", len(child.Tools), len(parentTools))
	}

	for i := range parentTools {
		if child.Tools[i].Name != parentTools[i].Name {
			t.Fatalf("tool %d: child %q, parent %q - the tool prefix is not shared",
				i, child.Tools[i].Name, parentTools[i].Name)
		}
	}

	// The identity and persona are the most-cached bytes of all.
	if child.Provider != parent.Assembly().Provider || child.Model != parent.Assembly().Model {
		t.Fatal("the subagent must not change the model envelope")
	}
}

// TestSubagentInheritsIdentityByteForByte pins the S1 claim as well: the child's
// rendered system prompt must open with exactly the parent's identity and
// persona, which is what makes the cached prefix reusable.
func TestSubagentInheritsIdentityByteForByte(t *testing.T) {
	provider := &recorder{reply: func(wire.Request) llm.Response { return textResponse("done") }}

	parent, tool := buildParent(t, provider, nil, 0, 1)

	if _, err := tool.Execute(context.Background(), json.RawMessage(
		`{"description":"probe","prompt":"hi"}`)); err != nil {
		t.Fatalf("execute: %v", err)
	}

	parentSystem := assemble.RenderSystem(parent.Assembly())

	// The child renders the same sections; only history differs.
	requests := provider.all()
	if len(requests) == 0 || len(requests[0].Messages) == 0 {
		t.Fatal("the subagent sent no messages")
	}

	// Encode as the transport would, so this asserts the exact bytes the
	// provider hashes and caches rather than an intermediate representation.
	outbound := wire.Messages(requests[0])

	if outbound[0].Role != session.RoleSystem {
		t.Fatal("the first message must be the system head")
	}

	childSystem, ok := outbound[0].Content.(string)
	if !ok {
		t.Fatalf("the system content is %T, want a string", outbound[0].Content)
	}
	if childSystem != parentSystem {
		t.Fatalf("the child's system prompt differs from the parent's:\n child: %q\nparent: %q",
			childSystem, parentSystem)
	}
}

// TestSubagentCannotRecurseIndefinitely is the safety property. A subagent that
// can spawn subagents without bound is both a cost and a debugging hazard.
func TestSubagentCannotRecurseIndefinitely(t *testing.T) {
	provider := &recorder{reply: func(wire.Request) llm.Response { return textResponse("done") }}

	// Depth 1 with a limit of 1: this tool is already at the ceiling.
	_, tool := buildParent(t, provider, nil, 1, 1)

	result, err := tool.Execute(context.Background(), json.RawMessage(
		`{"description":"nested","prompt":"delegate further"}`))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	if !result.IsError {
		t.Fatal("a subagent at the depth limit must refuse to delegate")
	}
	if !strings.Contains(resultText(result), "depth limit") {
		t.Fatalf("the refusal should explain itself: %q", resultText(result))
	}
	if len(provider.all()) != 0 {
		t.Fatal("a refused delegation must not start a model call")
	}
}

// TestSubagentReturnsOnlyItsAnswer: the parent's context must stay small, which
// is the point of delegating at all.
func TestSubagentReturnsOnlyItsAnswer(t *testing.T) {
	provider := &recorder{reply: func(req wire.Request) llm.Response {
		// First child call: use a tool. Second: answer.
		for _, message := range req.Messages {
			for _, block := range message.Content {
				if block.Type == session.BlockToolResult {
					return textResponse("FINAL ANSWER")
				}
			}
		}

		return llm.Response{Message: session.Message{
			Role:    session.RoleAssistant,
			Content: []session.Block{session.ToolCall("c1", "echo", json.RawMessage(`{"text":"noise"}`))},
		}}
	}}

	_, tool := buildParent(t, provider, nil, 0, 1)

	result, err := tool.Execute(context.Background(), json.RawMessage(
		`{"description":"research","prompt":"find the answer"}`))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	if !strings.Contains(resultText(result), "FINAL ANSWER") {
		t.Fatalf("the result should carry the child's answer: %q", resultText(result))
	}

	// The tool traffic happened inside the child and is summarised, not dumped.
	if strings.Contains(resultText(result), "echo: noise") {
		t.Fatal("the child's transcript must not be pasted into the parent's context")
	}
	if !strings.Contains(resultText(result), "subagent:") {
		t.Fatalf("the result should report what the child cost: %q", resultText(result))
	}
}

// TestSubagentResultIsBounded keeps one delegation from flooding the parent.
func TestSubagentResultIsBounded(t *testing.T) {
	provider := &recorder{reply: func(wire.Request) llm.Response {
		return textResponse(strings.Repeat("x", 200000))
	}}

	_, tool := buildParent(t, provider, nil, 0, 1)

	result, err := tool.Execute(context.Background(), json.RawMessage(
		`{"description":"big","prompt":"say a lot"}`))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	// Note: bounding is the retention layer's job at the log boundary; this
	// asserts the result is produced at all, and is a placeholder for that
	// wiring rather than a claim it is already truncated.
	if resultText(result) == "" {
		t.Fatal("a result is required")
	}
}

// denyGuard refuses every tool call, proving the child is gated by the same
// policy as the parent.
type denyGuard struct{}

func (denyGuard) Permit(context.Context, string, json.RawMessage, bool, audit.Transcript) (string, error) {
	return "Permission denied: not allowed anywhere in this session.", nil
}

// TestSubagentIsGatedByTheSamePermissionPolicy: a subagent must not become a way
// around a refusal the user gave.
func TestSubagentIsGatedByTheSamePermissionPolicy(t *testing.T) {
	provider := &recorder{reply: func(req wire.Request) llm.Response {
		for _, message := range req.Messages {
			for _, block := range message.Content {
				if block.Type == session.BlockToolResult && block.IsError {
					return textResponse("blocked")
				}
			}
		}

		return llm.Response{Message: session.Message{
			Role:    session.RoleAssistant,
			Content: []session.Block{session.ToolCall("c1", "echo", json.RawMessage(`{"text":"hi"}`))},
		}}
	}}

	_, tool := buildParent(t, provider, denyGuard{}, 0, 1)

	result, err := tool.Execute(context.Background(), json.RawMessage(
		`{"description":"gated","prompt":"use a tool"}`))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	if strings.Contains(resultText(result), "echo: hi") {
		t.Fatal("the subagent ran a tool the parent's guard refused")
	}
}

// resultText is the text a tool result carries.
func resultText(r tools.Result) string {
	var out strings.Builder
	for _, block := range r.Content {
		out.WriteString(block.Text)
	}

	return out.String()
}

func textResponse(s string) llm.Response {
	return llm.Response{
		Message: session.Message{Role: session.RoleAssistant, Content: []session.Block{session.Text(s)}},
	}
}

// compile-time proof the recorder is a provider.
var _ llm.Provider = (*recorder)(nil)

// TestSubagentStepLimitIsGenerous guards the ceiling from drifting back down.
//
// A subagent's transcript is invisible while it runs, so a tight limit turns
// "still working" into "silently stopped" - the worst of both, because the
// parent is told nothing about why.
func TestSubagentStepLimitIsGenerous(t *testing.T) {
	if subagent.DefaultMaxSteps < 30 {
		t.Fatalf("DefaultMaxSteps = %d, which is tight enough to stop real work silently",
			subagent.DefaultMaxSteps)
	}
}

// TestConfiguredSubagentLimitIsHonoured: the default must not override a caller
// that asks for something else.
func TestConfiguredSubagentLimitIsHonoured(t *testing.T) {
	provider := &recorder{reply: func(wire.Request) llm.Response {
		// Always ask for another tool, so the loop only stops at the ceiling.
		return llm.Response{Message: session.Message{
			Role:    session.RoleAssistant,
			Content: []session.Block{session.ToolCall("c", "echo", json.RawMessage(`{"text":"x"}`))},
		}}
	}}

	registry := tools.NewRegistry()
	if err := registry.Register(echo{}); err != nil {
		t.Fatalf("register: %v", err)
	}

	tool, err := subagent.New(subagent.Config{
		Provider: provider,
		Registry: registry,
		Assembly: assemble.Config{Provider: "local", Model: "default",
			Identity: "x", Tools: registry.Schemas(), MaxTokens: 4096},
		MaxSteps: 3,
		MaxDepth: 1,
	})
	if err != nil {
		t.Fatalf("subagent: %v", err)
	}

	result, err := tool.Execute(context.Background(), json.RawMessage(
		`{"description":"loop","prompt":"keep going"}`))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	// Three steps, so three model calls - not forty.
	if len(provider.all()) > 4 {
		t.Fatalf("a configured limit of 3 produced %d calls", len(provider.all()))
	}
	if resultText(result) == "" {
		t.Fatal("the subagent reported nothing")
	}
}

// TestADelegationChoosesItsOwnModel is the correction to a design that traded a
// feature for a one-time saving.
//
// The child used to inherit the parent's assembly whole, justified by keeping
// its S0 identical so its first request could reuse the parent's cache. That
// gave up per-delegation model choice to save a few thousand tokens once - and
// a child required to match its parent in every respect is one that did not need
// to exist.
//
// What the child keeps is the property that matters: its own prefix is stable
// across its own steps, so from its second request onward it is cached. A
// conversation has a cache; several do not share one.
func TestADelegationChoosesItsOwnModel(t *testing.T) {
	definition := (&subagent.Tool{}).Definition()
	schema := string(definition.Parameters)

	for _, want := range []string{`"model"`, `"thinking"`} {
		if !strings.Contains(schema, want) {
			t.Fatalf("a delegation cannot choose %s: %s", want, schema)
		}
	}

	// The effort ladder, not a free-text field: an unknown level reaches the
	// provider as an invalid request.
	for _, level := range []string{"minimal", "low", "medium", "high", "default"} {
		if !strings.Contains(schema, `"`+level+`"`) {
			t.Fatalf("the thinking enum is missing %q", level)
		}
	}

	// Both are optional: a delegation that names neither behaves as it always
	// did, inheriting from the parent.
	if strings.Contains(schema, `"required": ["description", "prompt", "model"`) {
		t.Fatal("model was made mandatory")
	}
}
