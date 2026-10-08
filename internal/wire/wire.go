// Package wire turns an assembled request into deterministic bytes and
// decomposes it into prefix units so that prefix stability can be tested
// directly.
//
// Determinism is a correctness requirement, not a style preference: a single
// unstable byte anywhere in the prompt converts every later step into a full
// cache miss. Everything here is therefore a pure function of its input.
//
// KV Cache effect: preserves the assembled prefix, by construction. This
// package never reorders, never injects, and never reads the clock.
package wire

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strings"

	"censi/harness/internal/session"
)

// ToolSchema is a model-facing tool declaration. Field order here is
// significant: Go marshals structs in declaration order, and the encoded bytes
// are what the provider caches. Adding a field to this struct is a
// cache-affecting change to every session in flight.
type ToolSchema struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	DeferLoad   bool            `json:"deferLoading,omitempty"`
}

// Request is a fully assembled model request. Nothing mutates it after
// assembly; that is what makes "a violating request is unrepresentable" true
// rather than aspirational.
type Request struct {
	Provider string
	Model    string

	// Tools is segment S0. Frozen and canonically ordered.
	Tools []ToolSchema

	// System is the rendered system prompt: segments S1 and S2. It travels as
	// the leading system message rather than as a request-level field, so a
	// prompt change is a surface operation with known caching consequences.
	System string

	// Messages is the derived history: S3 (instruction snapshots, which live in
	// history by design), S4 (conversation), and any S6 runtime-context
	// snapshot appended at the tail.
	Messages []session.Message

	// ReasoningEffort asks the endpoint to spend more or less time thinking.
	//
	// It is deliberately NOT part of Units, for the same reason as MaxTokens: it
	// is a sampling parameter and does not change the token sequence the provider
	// caches, so it must not perturb the prefix hash. It is also the cheapest
	// lever there is - the same prompt with a lower effort reads the same cached
	// prefix, so changing it costs nothing.
	//
	// Empty means "say nothing and let the endpoint decide", which for at least
	// one live route means reasoning until the output budget runs out.
	ReasoningEffort string

	// MaxTokens caps this request's output.
	//
	// It is deliberately NOT part of Units: a sampling parameter does not change
	// the token sequence the provider caches, so it must not perturb the prefix
	// hash. It is set explicitly rather than left to the endpoint because an
	// endpoint's default reservation can exceed what its own remaining window
	// allows, and the provider then clamps output to nothing rather than
	// complaining - measured as a single output token with finish_reason
	// "length", repeatedly, on a long prompt.
	// WebSearch asks for the answer to be grounded in current web results.
	//
	// Empty is off. "always" grounds every request; "auto" lets the model decide
	// whether to search, and it may search more than once.
	//
	// A capability rather than a mechanism: the client says how much searching
	// it wants and the gateway decides how. Naming a plugin or a tool here would
	// put a vendor's shape into a request that is otherwise portable - and would
	// make the harness unusable against anything else.
	//
	// It is excluded from Units and Hash for the same reason MaxTokens is: it
	// changes what comes back, not what was asked, so it must not alter the
	// prefix a provider caches.
	WebSearch string

	// WebSearchUses bounds searches for THIS request.
	//
	// The provider's caps are per request and an agent turn is many requests, so
	// a per-turn budget can only exist if the caller passes down what is left.
	// Zero means the gateway's own default.
	WebSearchUses int

	// MaxTokens caps the request's output.
	MaxTokens int
}

// UnitKind labels the provenance of a prefix unit, so a prefix-extension
// failure can name the segment that broke rather than only an offset.
type UnitKind string

const (
	UnitTools   UnitKind = "tools"
	UnitSystem  UnitKind = "system"
	UnitMessage UnitKind = "message"
)

// Unit is one independently-encoded piece of the prompt, in wire order.
type Unit struct {
	Kind  UnitKind
	Index int // message index for UnitMessage, otherwise -1
	Bytes []byte
}

// Outbound is the provider-facing projection of a session message.
//
// It deliberately is NOT session.Message. Two harness-internal facts must never
// reach a provider:
//
//   - Source, the provenance tag the assembler uses to recognise instruction and
//     runtime-context snapshots. It is bookkeeping, and no provider schema
//     defines it. Reconstruction reads the log, not the wire, so nothing is lost
//     by withholding it.
//   - the block array, in the common case. See encodeContent.
type Outbound struct {
	Role    session.Role `json:"role"`
	Content any          `json:"content"`

	// ToolCalls is set on an assistant message that requested tools.
	ToolCalls []OutboundCall `json:"tool_calls,omitempty"`

	// ToolCallID is set on a tool message, naming the call it answers. Without
	// it the provider cannot pair a result with its request, and the
	// conversation becomes invalid rather than merely lossy.
	ToolCallID string `json:"tool_call_id,omitempty"`
}

// OutboundTool is one tool declaration in wire form.
//
// The provider expects a wrapper object rather than the bare schema, and the
// wrapper's bytes are part of what it caches, so the conversion must be a pure
// function of the schema.
type OutboundTool struct {
	Type     string           `json:"type"`
	Function OutboundToolFunc `json:"function"`
}

// OutboundToolFunc names and describes a callable tool.
type OutboundToolFunc struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// ToolDeclarations converts registry schemas into wire declarations.
//
// Order is preserved exactly, because the order IS the cached prefix: any
// reordering here would invalidate S0 for every live session.
//
// Deferred tools are included. Deferral is declared in the registry but nothing
// activates it yet, so omitting them here would make a deferred tool silently
// unusable rather than merely unlisted.
func ToolDeclarations(schemas []ToolSchema) []OutboundTool {
	if len(schemas) == 0 {
		return nil
	}

	out := make([]OutboundTool, 0, len(schemas))
	for _, s := range schemas {
		out = append(out, OutboundTool{
			Type: "function",
			Function: OutboundToolFunc{
				Name:        s.Name,
				Description: s.Description,
				Parameters:  s.Parameters,
			},
		})
	}

	return out
}

// OutboundCall is one requested tool call in wire form.
type OutboundCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function OutboundFunction `json:"function"`
}

// OutboundFunction names the tool and carries its arguments.
//
// Arguments is a JSON-encoded STRING, not a nested object. That is the wire
// convention, and getting it wrong is rejected outright. The original bytes are
// preserved verbatim rather than re-encoded, because the argument bytes are part
// of what the provider caches.
type OutboundFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// encodeContent chooses the most portable content shape that still carries the
// message's meaning.
//
// A single text block becomes a plain string. The array form is emitted only
// when it carries information a string cannot: several blocks, or a non-text
// block such as an image.
//
// The string form is the default because the array form is markedly less
// portable. The common chat-completions schema permits `content` to be either a
// string or an array, but a schema is not what implementations accept, and the
// failures cluster on the system role - the least tolerant slot, and the one
// holding our reusable prefix. Text-only messages therefore travel as strings,
// and request emission stays byte-identical to what the widest range of
// endpoints already accepts.
func encodeContent(blocks []session.Block) any {
	if len(blocks) == 1 && blocks[0].Type == session.BlockText {
		return blocks[0].Text
	}
	return blocks
}

// encodeMessage projects a session message into its provider-facing form.
//
// Tool calls and tool results take a different shape from ordinary text, and the
// two must agree on the call id or the provider cannot pair them - an
// unpaired result is an invalid request, not a lossy one.
func encodeMessage(m session.Message) Outbound {
	var (
		calls     []OutboundCall
		texts     []string
		resultID  string
		result    string
		hasResult bool
	)

	for _, b := range m.Content {
		switch b.Type {
		case session.BlockToolCall:
			calls = append(calls, OutboundCall{
				ID:       b.CallID,
				Type:     "function",
				Function: OutboundFunction{Name: b.Name, Arguments: string(b.Arguments)},
			})
		case session.BlockToolResult:
			hasResult = true
			resultID = b.CallID
			result = b.Text
		case session.BlockText:
			texts = append(texts, b.Text)
		}
	}

	// A tool result is its own message, addressed to the call it answers.
	if hasResult && len(calls) == 0 {
		return Outbound{Role: session.RoleTool, ToolCallID: resultID, Content: result}
	}

	// An assistant turn that requested tools carries the calls plus any text
	// that accompanied them. Content is left null for a pure tool-call turn,
	// which is the conventional encoding.
	if len(calls) > 0 {
		out := Outbound{Role: m.Role, ToolCalls: calls}
		if len(texts) > 0 {
			out.Content = strings.Join(texts, "\n")
		}
		return out
	}

	return Outbound{Role: m.Role, Content: encodeContent(m.Content)}
}

// Encoding is important here, so state it plainly: providers cache the token
// sequence of the prompt, and their serialization frames the envelope. A naive
// byte-prefix check over the whole JSON body would therefore FAIL even for a
// perfectly append-only request, because the previous body ends in "]}" and the
// next one does not.
//
// Prefix stability is a property of the *sequence of units*, not of the
// serialized envelope. Units makes that property directly checkable.
func Units(r Request) ([]Unit, error) {
	var units []Unit

	if len(r.Tools) > 0 {
		b, err := json.Marshal(struct {
			Tools []ToolSchema `json:"tools"`
		}{r.Tools})
		if err != nil {
			return nil, fmt.Errorf("wire: encoding tools: %w", err)
		}
		units = append(units, Unit{Kind: UnitTools, Index: -1, Bytes: b})
	}

	if r.System != "" {
		b, err := json.Marshal(encodeMessage(session.Message{
			Role:    session.RoleSystem,
			Content: []session.Block{session.Text(r.System)},
		}))
		if err != nil {
			return nil, fmt.Errorf("wire: encoding system prompt: %w", err)
		}
		units = append(units, Unit{Kind: UnitSystem, Index: -1, Bytes: b})
	}

	for i, m := range r.Messages {
		b, err := json.Marshal(encodeMessage(m))
		if err != nil {
			return nil, fmt.Errorf("wire: encoding message %d: %w", i, err)
		}
		units = append(units, Unit{Kind: UnitMessage, Index: i, Bytes: b})
	}

	return units, nil
}

// Messages returns the request's messages in provider-facing order: the system
// prompt first, then the derived history. This is what a transport puts on the
// wire.
func Messages(r Request) []Outbound {
	out := make([]Outbound, 0, len(r.Messages)+1)

	if r.System != "" {
		out = append(out, encodeMessage(session.Message{
			Role:    session.RoleSystem,
			Content: []session.Block{session.Text(r.System)},
		}))
	}

	for _, m := range r.Messages {
		out = append(out, encodeMessage(m))
	}

	return pairToolCalls(out)
}

// pairToolCalls removes tool calls that have no result and results that have no
// call.
//
// Both are invalid requests rather than lossy ones, and the providers disagree
// about saying so: one tolerates an unanswered call, another rejects the whole
// request with a 400 that names no field. A turn interrupted between the model
// asking for a tool and the tool running leaves exactly that shape in the log.
//
// The log is append-only, so this is repaired on the way out rather than
// rewritten in place. The call still appears in the transcript - it did happen -
// and only the request is corrected.
func pairToolCalls(messages []Outbound) []Outbound {
	answered := make(map[string]bool)

	for _, m := range messages {
		if m.Role == session.RoleTool && m.ToolCallID != "" {
			answered[m.ToolCallID] = true
		}
	}

	called := make(map[string]bool)

	for _, m := range messages {
		for _, call := range m.ToolCalls {
			called[call.ID] = true
		}
	}

	out := make([]Outbound, 0, len(messages))

	for _, m := range messages {
		// A result answering a call that is not present is equally invalid.
		if m.Role == session.RoleTool && m.ToolCallID != "" && !called[m.ToolCallID] {
			continue
		}

		if len(m.ToolCalls) > 0 {
			kept := make([]OutboundCall, 0, len(m.ToolCalls))

			for _, call := range m.ToolCalls {
				if answered[call.ID] {
					kept = append(kept, call)
				}
			}

			// An assistant turn that was only a tool call, with the call now
			// removed, has nothing left to say. A message with neither content
			// nor calls is itself a rejected shape.
			if len(kept) == 0 && m.Content == nil {
				continue
			}

			m.ToolCalls = kept

			if len(kept) == 0 {
				m.ToolCalls = nil
			}
		}

		out = append(out, m)
	}

	return out
}

// Prompt returns the concatenated unit bytes: the closest faithful stand-in for
// the token sequence a provider would see, without modelling a tokenizer.
func Prompt(r Request) ([]byte, error) {
	units, err := Units(r)
	if err != nil {
		return nil, err
	}
	var out []byte
	for _, u := range units {
		out = append(out, u.Bytes...)
		out = append(out, '\n')
	}
	return out, nil
}

// Hash returns a stable digest of the request's units. Two requests hash equal
// if and only if they would present the same prompt bytes.
//
// Each unit is length-prefixed before hashing so that concatenation ambiguity
// cannot make two different prompts collide.
func Hash(r Request) (string, error) {
	units, err := Units(r)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	var lenBuf [8]byte
	for _, u := range units {
		binary.BigEndian.PutUint64(lenBuf[:], uint64(len(u.Bytes)))
		h.Write(lenBuf[:])
		h.Write(u.Bytes)
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

// Extends reports whether next's unit sequence has prev's as a prefix, and if
// not, at which unit the two diverge.
//
// This is the prefix-extension property: within a request series, every request
// must extend its predecessor. A failure names the first differing unit, which
// identifies the segment that broke stability.
func Extends(prev, next []Unit) (ok bool, divergeAt int) {
	for i := range prev {
		if i >= len(next) {
			return false, i
		}
		if string(prev[i].Bytes) != string(next[i].Bytes) {
			return false, i
		}
	}
	return true, -1
}
