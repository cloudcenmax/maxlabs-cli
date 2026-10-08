// Package assemble is the only component permitted to construct a model
// request.
//
// Nothing else may add, reorder, or format bytes sent to a provider. That
// structural rule is what makes the prefix-extension property testable and what
// makes a cache-hostile request unrepresentable rather than merely discouraged.
//
// The segment layout below is the cache strategy. Segments are ordered
// most-stable first, and anything that changes every step is pushed to the
// tail, where its invalidation cost is bounded to itself.
//
//	S0 tools        immutable          frozen order, never reordered
//	S1 identity     session-immutable  harness identity + persona prefix
//	S2 guidance     session-immutable  static guidance + mode policy
//	S3 instructions append-only        workspace instructions, IN HISTORY
//	S4 history      append-only        conversation
//	S5 summary      session-stable     compaction marker, below S1
//	S6 volatile     volatile           runtime context, ALWAYS LAST
//
// S3 is in history rather than in the system prompt on purpose: a mid-session
// AGENTS.md edit then costs an append instead of a head rewrite. Some providers
// treat a cached system instruction as immutable and cannot carry a dynamic
// tail, which makes this ordering mandatory rather than stylistic.
//
// KV Cache effect: authoritative. This package owns the prefix contract. It is
// prefix-stable while identity, persona, section text and order render
// identically, and while history only grows.
package assemble

import (
	"fmt"
	"sort"

	"censi/harness/internal/session"
	"censi/harness/internal/wire"
)

// Provenance tags. The assembler uses these to recognise instruction and
// runtime-context snapshots without depending on their position, so the
// ordering rules survive compaction and resume.
const (
	SourceInstructions   = "agent-instructions"
	SourceRuntimeContext = "runtime-context"
)

// Section orders. Entering plan mode perturbs S2 from order 500 onward;
// identity and persona keep their prefix. Note that Go's string "<" is a
// code-unit comparison, so the ordering is locale-independent and therefore
// identical on every machine - a locale-sensitive compare here would make the
// cached prefix machine-dependent.
const (
	OrderHarnessIdentity = -1000
	OrderPersonaPrefix   = 0
	OrderPlanPolicy      = 500
	OrderGuidance        = 1000
	OrderPersonaSuffix   = 10200
)

// Section is one ordered contribution to the system prompt.
type Section struct {
	Name  string
	Order int
	Text  string
}

// Config is the session-stable half of a request. Changing any field between
// steps is a cache-affecting change and belongs under the declaration rule.
type Config struct {
	Provider string
	Model    string

	// S0. Already canonically ordered; see CanonicalizeTools.
	Tools []wire.ToolSchema

	// S1.
	Identity      string
	PersonaPrefix string

	// S2.
	Guidance []Section

	// S1, order 10200.
	PersonaSuffix string

	// ReasoningEffort asks the endpoint to spend more or less time thinking.
	// Empty leaves the choice to the endpoint, which is not recommended: the
	// default on at least one route is to reason until the output budget is gone.
	ReasoningEffort string

	// MaxTokens caps each request's output. Leaving it zero defers to the
	// endpoint, which is not recommended: an endpoint's default reservation can
	// exceed what its own remaining window allows, and the provider then clamps
	// output to nothing rather than reporting a problem.
	MaxTokens int

	// WebSearch asks the gateway to ground the answer in current results.
	WebSearch string

	// WebSearchUses is how many searches remain in this turn.
	WebSearchUses int
}

// RenderSystem renders segments S1 and S2 into the single system string.
//
// Sections are ordered by ascending Order, ties broken by code-unit name
// comparison; empty sections are dropped before joining; the join separator is
// exactly "\n\n".
func RenderSystem(cfg Config) string {
	sections := make([]Section, 0, len(cfg.Guidance)+3)

	if cfg.Identity != "" {
		sections = append(sections, Section{Name: "harness:identity", Order: OrderHarnessIdentity, Text: cfg.Identity})
	}
	if cfg.PersonaPrefix != "" {
		sections = append(sections, Section{Name: "deployment:persona-prefix", Order: OrderPersonaPrefix, Text: cfg.PersonaPrefix})
	}
	sections = append(sections, cfg.Guidance...)
	if cfg.PersonaSuffix != "" {
		sections = append(sections, Section{Name: "deployment:persona-suffix", Order: OrderPersonaSuffix, Text: cfg.PersonaSuffix})
	}

	sort.SliceStable(sections, func(i, j int) bool {
		if sections[i].Order != sections[j].Order {
			return sections[i].Order < sections[j].Order
		}
		return sections[i].Name < sections[j].Name
	})

	rendered := ""
	for _, s := range sections {
		if s.Text == "" {
			continue
		}
		if rendered != "" {
			rendered += "\n\n"
		}
		rendered += s.Text
	}
	return rendered
}

// CanonicalizeTools returns the tool declarations in a fixed model-facing
// order.
//
// Registration order is a load-time artifact: it varies with init order,
// conditional mounting, and configuration reloads, any of which would silently
// reshuffle S0 and invalidate the cache. Canonicalising makes the prefix a
// function of the tool *set* rather than of the order modules happened to
// mount.
//
// order is an explicit ordered list of names; any name not listed is appended
// in code-unit order after the listed ones. An unknown name in order is an
// error rather than a silent omission.
func CanonicalizeTools(tools []wire.ToolSchema, order []string) ([]wire.ToolSchema, error) {
	byName := make(map[string]wire.ToolSchema, len(tools))
	for _, t := range tools {
		if _, dup := byName[t.Name]; dup {
			return nil, fmt.Errorf("assemble: duplicate tool %q", t.Name)
		}
		byName[t.Name] = t
	}

	seen := make(map[string]bool, len(order))
	out := make([]wire.ToolSchema, 0, len(tools))
	for _, name := range order {
		if seen[name] {
			return nil, fmt.Errorf("assemble: tool order lists %q twice", name)
		}
		seen[name] = true
		t, ok := byName[name]
		if !ok {
			return nil, fmt.Errorf("assemble: tool order names unregistered tool %q", name)
		}
		out = append(out, t)
	}

	rest := make([]string, 0, len(tools))
	for name := range byName {
		if !seen[name] {
			rest = append(rest, name)
		}
	}
	sort.Strings(rest) // code-unit order, locale-independent
	for _, name := range rest {
		out = append(out, byName[name])
	}

	return out, nil
}

// SyncSystem reconciles the rendered prompt against the surface's system node.
//
// For M0 there is no in-history prompt support, so the render is consolidated
// at node 0: appended when absent, replaced only when the text actually
// differs. The replacement is permitted over node 0 because the replacing event
// is itself a system message - the same carve-out the log enforces.
func SyncSystem(log *session.Log, rendered string) error {
	head, ok := log.SystemHead()
	if !ok {
		if len(log.SurfaceNodes()) > 0 {
			return fmt.Errorf("assemble: surface node 0 is not a system message")
		}
		_, err := log.Append(session.EventSystemMessage, session.Message{
			Role:    session.RoleSystem,
			Content: []session.Block{session.Text(rendered)},
		}, session.AppendOp())
		return err
	}

	if len(head.Content) == 1 && head.Content[0].Type == "text" && head.Content[0].Text == rendered {
		return nil
	}

	seq := log.SurfaceNodes()[0]
	_, err := log.Append(session.EventSystemMessage, session.Message{
		Role:    session.RoleSystem,
		Content: []session.Block{session.Text(rendered)},
	}, session.ReplaceOp(seq, seq), seq)
	return err
}

// Assemble projects the log into a request.
//
// The system prompt is taken from the log rather than re-rendered, so the log
// remains the single authority for what the model saw. Callers that changed the
// configuration are expected to have called SyncSystem first.
func Assemble(log *session.Log, cfg Config) (wire.Request, error) {
	req := wire.Request{
		Provider:        cfg.Provider,
		Model:           cfg.Model,
		Tools:           cfg.Tools,
		MaxTokens:       cfg.MaxTokens,
		WebSearch:       cfg.WebSearch,
		WebSearchUses:   cfg.WebSearchUses,
		ReasoningEffort: cfg.ReasoningEffort,
	}

	derived := log.DeriveMessages()
	for i, m := range derived {
		// The system prompt is node 0. It travels as the request's leading
		// system message rather than as a request-level field, so the request
		// itself carries no system field at all.
		if i == 0 && m.Role == session.RoleSystem {
			req.System = renderBlocks(m.Content)
			continue
		}
		req.Messages = append(req.Messages, m)
	}

	return req, nil
}

// NoteRuntimeContext appends a runtime-context snapshot when it differs from the
// most recent one, and reports whether it appended.
//
// Volatile data belongs at the tail (S6), never in the head: it changes every
// step, so anchoring it early would invalidate everything after it. Change
// suppression keeps the history from accumulating identical snapshots.
func NoteRuntimeContext(log *session.Log, rendered string) (bool, error) {
	var last string
	var found bool
	for _, seq := range log.SurfaceNodes() {
		m, ok := log.MessageAt(seq)
		if !ok || m.Source != SourceRuntimeContext {
			continue
		}
		last = renderBlocks(m.Content)
		found = true
	}
	if found && last == rendered {
		return false, nil
	}

	_, err := log.Append(session.EventUserMessage, session.Message{
		Role:    session.RoleUser,
		Content: []session.Block{session.Text(rendered)},
		Source:  SourceRuntimeContext,
	}, session.AppendOp())
	return err == nil, err
}

// NoteInstructions appends a workspace-instruction snapshot as a sourced
// history message.
//
// Instructions live in history, not in the system prompt, so a mid-session edit
// is an append rather than a head rewrite.
func NoteInstructions(log *session.Log, rendered string) error {
	_, err := log.Append(session.EventUserMessage, session.Message{
		Role:    session.RoleUser,
		Content: []session.Block{session.Text(rendered)},
		Source:  SourceInstructions,
	}, session.AppendOp())
	return err
}

func renderBlocks(blocks []session.Block) string {
	out := ""
	for _, b := range blocks {
		if b.Type != "text" || b.Text == "" {
			continue
		}
		if out != "" {
			out += "\n"
		}
		out += b.Text
	}
	return out
}

// PlanModeSection returns the ordered section that marks plan mode.
//
// The ORDER is the point, not an implementation detail. At OrderPlanPolicy
// (500) the section sits after the harness identity (-1000) and the persona
// prefix (0), and before tool guidance (1000 and up). Entering or leaving plan
// mode therefore leaves every section ahead of it byte-identical, so only the
// cached prefix from order 500 onward is invalidated. Had plan mode been placed
// first, toggling it would discard the entire cached prompt - for a change that
// affects a few hundred trailing tokens.
//
// When inactive the section renders empty and is dropped before joining, so the
// prompt is byte-identical to a session that never had plan mode at all. That
// matters more than it looks: a user who toggles plan mode on and back off must
// not be left with a permanently different prefix.
func PlanModeSection(active bool) Section {
	text := ""
	if active {
		text = "PLAN MODE is active: you are planning, not acting. " +
			"Tools that change the workspace are refused while it is on, so do not spend steps " +
			"attempting them. Investigate with read-only tools, then present a concrete, ordered " +
			"plan and wait for the user to leave plan mode before making any change."
	}

	return Section{Name: "mode:plan", Order: OrderPlanPolicy, Text: text}
}
