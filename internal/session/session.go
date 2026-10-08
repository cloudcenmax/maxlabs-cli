// Package session implements the append-only event log that is the single
// source of truth for everything the model sees.
//
// The package's load-bearing invariant is:
//
//	Model-visible implies durably referenced.
//
// Nothing reaches a provider request that cannot be reconstructed from this
// log. Every request the harness builds is a pure function of a log prefix,
// which is what makes provider prefix-cache stability emergent rather than
// something the code has to maintain.
//
// KV Cache effect: appended surface entries preserve reusable prefixes; a
// replacement invalidates reuse from the first shadowed message even though the
// underlying log stays append-only. This package never assembles a request.
package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Seq identifies an event's position in the log and, once folded onto the
// surface, its position in the model-visible sequence.
type Seq int64

// Role is a provider-neutral message role.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleDeveloper Role = "developer"
	// RoleTool carries a tool's result back to the model. It is its own role
	// rather than a user message so that the call it answers can be named.
	RoleTool Role = "tool"
)

// Block kinds.
const (
	BlockText       = "text"
	BlockToolCall   = "tool_call"
	BlockToolResult = "tool_result"
)

// Block is one piece of message content.
//
// It is a tagged union rather than a set of parallel types, because a message's
// content is one ordered list: a model may emit text, then a tool call, then
// more text, and the order is part of what the provider caches. Splitting the
// kinds into separate fields would lose that ordering.
//
// Every field is omitempty, so a given block serialises to exactly the bytes its
// kind requires and nothing else.
type Block struct {
	Type string `json:"type"`

	// Text is set for BlockText.
	Text string `json:"text,omitempty"`

	// CallID identifies a tool call, and on a result names the call it answers.
	// It is the join key between the two, so it must survive verbatim.
	CallID string `json:"callId,omitempty"`

	// Name is the tool's registered name, set for BlockToolCall.
	Name string `json:"name,omitempty"`

	// Arguments is the model's raw argument JSON, kept as bytes rather than a
	// decoded value: re-encoding it would change the bytes the model produced,
	// and those bytes are part of the cached prefix.
	Arguments json.RawMessage `json:"arguments,omitempty"`

	// IsError marks a tool result that reports a failure. The model needs to
	// distinguish "the tool ran and failed" from "the tool ran and said this".
	IsError bool `json:"isError,omitempty"`
}

// Text builds a text block.
func Text(s string) Block { return Block{Type: BlockText, Text: s} }

// ToolCall builds a tool-call block from a model response.
func ToolCall(callID, name string, arguments json.RawMessage) Block {
	if len(arguments) == 0 {
		arguments = json.RawMessage("{}")
	}
	return Block{Type: BlockToolCall, CallID: callID, Name: name, Arguments: arguments}
}

// ToolResult builds a tool-result block.
func ToolResult(callID string, isError bool, text string) Block {
	return Block{
		Type:    BlockToolResult,
		CallID:  callID,
		Text:    text,
		IsError: isError,
	}
}

// Message is a model-visible message. Source records provenance so the
// assembler can tell a workspace-instruction snapshot from ordinary user input
// without depending on position.
type Message struct {
	Role    Role    `json:"role"`
	Content []Block `json:"content"`
	Source  string  `json:"source,omitempty"`
}

// ToolCalls returns the tool calls in a message, in order.
func (m Message) ToolCalls() []Block {
	var out []Block
	for _, b := range m.Content {
		if b.Type == BlockToolCall {
			out = append(out, b)
		}
	}
	return out
}

// Event type names.
const (
	EventSessionCreated   = "session/created"
	EventSystemMessage    = "system/message"
	EventDeveloperMessage = "developer/message"
	EventUserMessage      = "user/message"
	EventAssistantMessage = "assistant/message"
	EventToolResult       = "tool/result"
	EventRequestHeader    = "request/header"
	EventCompaction       = "compaction/replace"
)

// surfaceEligible is the closed set of types that appear in model history:
// exactly these may carry a surface operation, and every one of them must. A
// bookkeeping or maintenance event therefore cannot accidentally become
// model-visible.
var surfaceEligible = map[string]bool{
	EventSystemMessage:    true,
	EventDeveloperMessage: true,
	EventUserMessage:      true,
	EventAssistantMessage: true,
	EventToolResult:       true,
}

// OpKind distinguishes growth from rewrite. Append preserves the reusable
// prefix; Replace does not, and must be batched deliberately.
type OpKind uint8

const (
	// OpAppend extends the surface. Cache-preserving.
	OpAppend OpKind = iota + 1
	// OpReplace shadows an inclusive range of surface nodes. Cache-breaking
	// from the first replaced node.
	OpReplace
)

// SurfaceOp is the surface operation carried by an event.
type SurfaceOp struct {
	Kind  OpKind
	Start Seq // inclusive; OpReplace only
	End   Seq // inclusive; OpReplace only
}

// AppendOp returns an append operation.
func AppendOp() *SurfaceOp { return &SurfaceOp{Kind: OpAppend} }

// ReplaceOp returns a replacement over the inclusive range [start, end].
func ReplaceOp(start, end Seq) *SurfaceOp { return &SurfaceOp{Kind: OpReplace, Start: start, End: end} }

// Event is one durable log record.
//
// Payload for a message event is a Message. Other event types carry their own
// payload shapes.
type Event struct {
	Seq             Seq
	Type            string
	Time            int64
	Payload         any
	Surface         *SurfaceOp
	SourceEventSeqs []Seq
}

// Message returns the event's message when it carries one.
func (e Event) Message() (Message, bool) {
	m, ok := e.Payload.(Message)
	return m, ok
}

// Sentinel errors. Callers route on these rather than on message text.
var (
	ErrNotContiguous     = errors.New("session: event sequence is not contiguous")
	ErrSurfaceMarker     = errors.New("session: surface marker does not match event type")
	ErrUnknownSurfaceRef = errors.New("session: replacement references a node that is not on the surface")
	ErrBadRange          = errors.New("session: replacement range is invalid")
	ErrSourceNotEarlier  = errors.New("session: sourceEventSeqs must reference earlier events")
	ErrHeadProtected     = errors.New("session: a replacement may not cover the system node")
)

// Clock supplies event timestamps. Injectable so that tests are deterministic;
// note that Time never enters a model request, so it cannot affect the prefix.
type Clock func() int64

// Log is an append-only event log with its derived surface projection.
//
// The zero value is not usable; construct with New.
type Log struct {
	observer func(Event)

	events []Event
	clock  Clock

	// surface projection
	nodes             []Seq
	projected         map[Seq]Message
	replaceGeneration int
	contentGeneration int
}

// New returns an empty log. A nil clock uses the wall clock.
func New(clock Clock) *Log {
	if clock == nil {
		clock = func() int64 { return time.Now().UnixMilli() }
	}
	return &Log{clock: clock, projected: map[Seq]Message{}}
}

// Events returns the log's events. The returned slice must not be mutated.
// SetObserver installs a callback invoked after every committed event.
//
// It is how durability is attached without the agent knowing anything about
// storage: the log announces what it committed, and a listener decides what to
// do about it. Nothing may be appended from inside the callback.
func (l *Log) SetObserver(fn func(Event)) {
	l.observer = fn
}

func (l *Log) Events() []Event { return l.events }

// Len returns the number of committed events.
func (l *Log) Len() int { return len(l.events) }

// SurfaceNodes returns the ordered surface node sequence.
func (l *Log) SurfaceNodes() []Seq { return l.nodes }

// ReplaceGeneration counts folded positional replacements.
func (l *Log) ReplaceGeneration() int { return l.replaceGeneration }

// ContentGeneration counts every committed change to existing model-visible
// content: positional replacements and plugin-owned message projections. This
// is the invalidation epoch; the assembler and the token meter both key on it.
func (l *Log) ContentGeneration() int { return l.contentGeneration }

// Append commits one event.
//
// It validates the whole candidate before mutating anything, so a rejected
// append leaves the log exactly as it was. Validation is total and
// allocation-free up to the commit point: a malformed event fails before any
// mutation, so the same log fails identically on every replay.
func (l *Log) Append(eventType string, payload any, op *SurfaceOp, sources ...Seq) (Event, error) {
	eligible := surfaceEligible[eventType]

	switch {
	case eligible && op == nil:
		return Event{}, fmt.Errorf("%w: %q is surface-eligible and requires a surface operation", ErrSurfaceMarker, eventType)
	case !eligible && op != nil:
		return Event{}, fmt.Errorf("%w: %q is not surface-eligible and cannot carry one", ErrSurfaceMarker, eventType)
	}

	next := Seq(len(l.events))
	for _, s := range sources {
		if s >= next {
			return Event{}, fmt.Errorf("%w: source %d is not before %d", ErrSourceNotEarlier, s, next)
		}
	}

	var startIdx, endIdx int
	if op != nil && op.Kind == OpReplace {
		if op.Start > op.End {
			return Event{}, fmt.Errorf("%w: start %d is after end %d", ErrBadRange, op.Start, op.End)
		}
		var ok bool
		startIdx, ok = indexOf(l.nodes, op.Start)
		if !ok {
			return Event{}, fmt.Errorf("%w: start %d", ErrUnknownSurfaceRef, op.Start)
		}
		endIdx, ok = indexOf(l.nodes, op.End)
		if !ok || endIdx < startIdx {
			return Event{}, fmt.Errorf("%w: end %d", ErrUnknownSurfaceRef, op.End)
		}
		// The system prompt at node 0 is structurally protected. A policy of
		// "never shadow the head" can be forgotten; a rejected append cannot,
		// so the guard lives here rather than in the compaction caller.
		if startIdx == 0 && eventType != EventSystemMessage {
			return Event{}, fmt.Errorf("%w: %d..%d would shadow it", ErrHeadProtected, op.Start, op.End)
		}
	}

	event := Event{
		Seq:             next,
		Type:            eventType,
		Time:            l.clock(),
		Payload:         payload,
		Surface:         op,
		SourceEventSeqs: append([]Seq(nil), sources...),
	}

	// Commit.
	l.events = append(l.events, event)
	if msg, ok := event.Message(); ok {
		l.projected[event.Seq] = msg
	}
	switch {
	case op == nil:
		// log-only
	case op.Kind == OpAppend:
		l.nodes = append(l.nodes, event.Seq)
	case op.Kind == OpReplace:
		replacement := append([]Seq(nil), l.nodes[:startIdx]...)
		replacement = append(replacement, event.Seq)
		replacement = append(replacement, l.nodes[endIdx+1:]...)
		l.nodes = replacement
		l.replaceGeneration++
		l.contentGeneration++
	}

	// Observe after the commit, never before. A callback that runs first could
	// record an event that a subsequent error prevented from happening, which is
	// how a durable log and a live log drift apart.
	if l.observer != nil {
		l.observer(event)
	}

	return event, nil
}

// DeriveMessages projects the surface into the ordered message list the model
// sees. Each surface node is projected through the same pure function every
// time, so an external reconstructor folding a log prefix cannot disagree.
//
// Growth is a tail append: the previous result is a prefix of this one.
func (l *Log) DeriveMessages() []Message {
	out := make([]Message, 0, len(l.nodes))
	for _, seq := range l.nodes {
		if m, ok := l.projected[seq]; ok {
			out = append(out, m)
		}
	}
	return out
}

// SystemHead reports the system node at surface position 0, if any. The
// assembler renders the prompt into exactly this node.
func (l *Log) SystemHead() (Message, bool) {
	if len(l.nodes) == 0 {
		return Message{}, false
	}
	m, ok := l.projected[l.nodes[0]]
	if !ok || m.Role != RoleSystem {
		return Message{}, false
	}
	return m, true
}

// MessageAt returns the projected message for a surface node.
func (l *Log) MessageAt(seq Seq) (Message, bool) {
	m, ok := l.projected[seq]
	return m, ok
}

func indexOf(nodes []Seq, target Seq) (int, bool) {
	for i, n := range nodes {
		if n == target {
			return i, true
		}
	}
	return 0, false
}
