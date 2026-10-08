// Package tools provides the model-facing tool registry and the tools
// themselves.
//
// The registry exists to make segment S0 (tool declarations) byte-stable. Two
// properties matter more than anything else here:
//
//  1. Declaration order must not depend on registration order. Registration
//     order is a load-time artifact - it varies with init order, conditional
//     mounting, and configuration reloads - and any of those would silently
//     reshuffle S0 and invalidate the cached prefix of every live session.
//  2. A tool's declared schema must serialise identically every time. Parameter
//     JSON is therefore canonicalised at registration rather than trusted as
//     written.
//
// KV Cache effect: prefix-stable while the visible definitions and their order
// are unchanged. Registration or removal may invalidate reuse from the first
// changed schema token.
package tools

import (
	"context"

	"censi/harness/internal/diff"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"censi/harness/internal/session"
	"censi/harness/internal/wire"
)

// Definition is a tool's model-facing declaration.
type Definition struct {
	Name        string
	Description string

	// Parameters is a JSON Schema object. It is canonicalised at registration,
	// so the bytes are stable regardless of how the literal was written.
	Parameters json.RawMessage

	// DeferLoad marks a tool whose declaration is registered but not offered
	// until it is first used. Keeps rarely-used schemas out of the hot prefix.
	DeferLoad bool

	// ReadOnly is a safety classification, not a security boundary. It records
	// whether the tool can change the workspace, which matters for whether a
	// failed call is safe to retry blindly.
	ReadOnly bool
}

// Tool is a callable capability.
//
// Execute receives the model's raw argument JSON. Implementations must not
// mutate the registry or the workspace outside their stated effect, and must
// return a Result rather than writing to stdout.
type Tool interface {
	Definition() Definition
	Execute(ctx context.Context, args json.RawMessage) (Result, error)
}

// Result is one tool's output.
//
// Content is the model-visible payload. Omitted reports what bounding removed,
// so the notice the model sees is derived from the same numbers that were
// actually dropped rather than guessed.
type Result struct {
	Content []session.Block
	Omitted Omitted
	IsError bool

	// Diff is what changed on disk.
	//
	// It is deliberately NOT part of Content. The model gets a short
	// confirmation because a diff of its own edit is noise it already knows,
	// while the person watching gets the thing they actually cannot see - what
	// the model just did to their files.
	Diff []diff.Line

	// DiffTarget names the file the diff applies to, for a heading.
	DiffTarget string
}

// Text builds a text-only result.
func Text(s string) Result {
	return Result{Content: []session.Block{session.Text(s)}}
}

// Error builds a failed result whose text is model-visible.
//
// A tool failure is data for the model, not a harness failure: the caller
// records it and lets the model decide what to do. Only an error the harness
// itself cannot describe should be returned as a Go error.
func Error(s string) Result {
	return Result{Content: []session.Block{session.Text(s)}, IsError: true}
}

// Registry holds the available tools.
//
// The zero value is not usable; construct with NewRegistry.
type Registry struct {
	mu    sync.RWMutex
	tools map[string]registered
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{tools: map[string]registered{}}
}

// Register adds a tool.
//
// Registering a duplicate name fails rather than replacing: a silent replace
// would change S0 mid-session, and the caller that lost is unlikely to be the
// caller that noticed.
func (r *Registry) Register(t Tool) error {
	def := t.Definition()

	if err := validateDefinition(def); err != nil {
		return err
	}

	params, err := canonicalJSON(def.Parameters)
	if err != nil {
		return fmt.Errorf("tools: %q parameters: %w", def.Name, err)
	}
	def.Parameters = params

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.tools[def.Name]; exists {
		return fmt.Errorf("tools: %q is already registered", def.Name)
	}

	r.tools[def.Name] = registered{tool: t, def: def}

	return nil
}

// Get returns a registered tool by name.
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	entry, ok := r.tools[name]
	if !ok {
		return nil, false
	}

	return entry.tool, true
}

// Names returns every registered tool name in code-unit order.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	sort.Strings(names) // code-unit order, locale-independent

	return names
}

// Schemas returns the model-facing declarations in a deterministic order.
//
// Ordering is by name, which is registration-independent. Policy ordering (an
// explicit preferred list) belongs one layer up, where the tool order is
// configuration rather than a property of the registry.
func (r *Registry) Schemas() []wire.ToolSchema {
	r.mu.RLock()
	defer r.mu.RUnlock()

	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]wire.ToolSchema, 0, len(names))
	for _, name := range names {
		entry := r.tools[name]
		out = append(out, wire.ToolSchema{
			Name:        entry.def.Name,
			Description: entry.def.Description,
			Parameters:  entry.def.Parameters,
			DeferLoad:   entry.def.DeferLoad,
		})
	}

	return out
}

// Execute runs a tool by name.
//
// An unknown name is a model error, not a harness error: the model may have
// hallucinated a tool, and the correct response is to tell it so.
func (r *Registry) Execute(ctx context.Context, name string, args json.RawMessage) (Result, error) {
	tool, ok := r.Get(name)
	if !ok {
		return Error(fmt.Sprintf("No tool named %q is available.", name)), nil
	}

	if len(args) == 0 {
		args = json.RawMessage("{}")
	}

	return tool.Execute(ctx, args)
}

// registered pairs a tool with its canonicalised definition.
type registered struct {
	tool Tool
	def  Definition
}

func validateDefinition(def Definition) error {
	if strings.TrimSpace(def.Name) == "" {
		return fmt.Errorf("tools: a tool must have a name")
	}
	if strings.ContainsAny(def.Name, " \t\n") {
		return fmt.Errorf("tools: %q must not contain whitespace", def.Name)
	}
	if strings.TrimSpace(def.Description) == "" {
		return fmt.Errorf("tools: %q must have a description", def.Name)
	}
	if len(def.Parameters) == 0 {
		return fmt.Errorf("tools: %q must declare its parameters", def.Name)
	}

	return nil
}

// canonicalJSON re-encodes JSON so that the bytes are a pure function of the
// value rather than of the source formatting.
//
// This is the difference between "we happened to write the keys in the same
// order twice" and "the declaration cannot vary". Map keys are sorted by the
// encoder, whitespace is removed, and the result is stable across runs,
// platforms, and refactors of the literal.
func canonicalJSON(raw json.RawMessage) (json.RawMessage, error) {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, fmt.Errorf("not valid JSON: %w", err)
	}

	if _, isObject := value.(map[string]any); !isObject {
		return nil, fmt.Errorf("must be a JSON object describing the parameters")
	}

	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}

	return encoded, nil
}
