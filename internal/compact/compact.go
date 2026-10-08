// Package compact keeps a long session inside its context window without
// letting the cost of doing so cascade.
//
// Compaction is the one operation that MUST break the cached prefix: the
// alternative is exceeding the window. The goal is therefore to break it
// rarely, late, and minimally, which is what the rules in this package encode:
//
//   - The system node is structurally unreachable. Range selection starts after
//     it, and the session log independently refuses a replacement that would
//     cover it.
//   - One compaction mutates the surface exactly once. The number of
//     cache-breaking seams equals the number of compactions, never more.
//   - Pruning is tried before summarizing, because it is cheap, needs no model
//     call, and may drop pressure below the threshold on its own.
//
// KV Cache effect: a replacement invalidates reuse from the first shadowed
// history token. The prefix before the range remains reusable.
package compact

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"censi/harness/internal/session"
)

// Defaults. thresholdRatio and headroomTokens are deliberately conservative:
// compacting early costs a cache break, and compacting late costs a failed
// request.
const (
	DefaultThresholdRatio = 0.8
	DefaultHeadroomTokens = 65_536
	DefaultRetainRatio    = 0.16
	DefaultSummaryTokens  = 4_096

	// Pruning budgets. A tool result above thresholdBytes is replaced by its
	// head and tail plus a marker.
	DefaultPruneThresholdBytes = 8_192
	DefaultPruneHeadBytes      = 4_096
	DefaultPruneTailBytes      = 1_024
)

// CharsPerToken is the estimation heuristic.
//
// It is approximate and knowingly so: it under-prices CJK text and JSON
// schemas, where a character carries far more information than the ratio
// assumes. It exists because no provider reports the size of a prompt that has
// not been sent yet. Anything that must be exact uses provider-reported usage
// instead, and the hard backstop for an under-estimate is the provider's own
// context-window error.
const CharsPerToken = 4

// MessageOverheadTokens approximates the per-message structural cost that a
// character count misses.
const MessageOverheadTokens = 4

// Policy decides when to compact and how much to keep.
type Policy struct {
	// ContextWindow is the routed model's capacity.
	ContextWindow int

	// OutputReserve is the output budget held back from the window.
	OutputReserve int

	// HeadroomTokens is additional slack beyond the output reservation.
	HeadroomTokens int

	// ThresholdRatio is the window fraction at which compaction triggers.
	ThresholdRatio float64

	// RetainRatio keeps this fraction of the message budget verbatim. Mutually
	// exclusive with RetainTokens.
	RetainRatio float64

	// RetainTokens overrides RetainRatio with an absolute budget.
	RetainTokens int

	// SummaryTokens caps the summary the model may produce.
	SummaryTokens int

	PruneThresholdBytes int
	PruneHeadBytes      int
	PruneTailBytes      int
}

// withDefaults fills unset fields and normalises the retention form.
func (p Policy) withDefaults() Policy {
	if p.ThresholdRatio <= 0 {
		p.ThresholdRatio = DefaultThresholdRatio
	}
	if p.HeadroomTokens <= 0 {
		p.HeadroomTokens = DefaultHeadroomTokens
	}
	if p.RetainRatio <= 0 && p.RetainTokens <= 0 {
		p.RetainRatio = DefaultRetainRatio
	}
	if p.SummaryTokens <= 0 {
		p.SummaryTokens = DefaultSummaryTokens
	}
	if p.PruneThresholdBytes <= 0 {
		p.PruneThresholdBytes = DefaultPruneThresholdBytes
	}
	if p.PruneHeadBytes <= 0 {
		p.PruneHeadBytes = DefaultPruneHeadBytes
	}
	if p.PruneTailBytes <= 0 {
		p.PruneTailBytes = DefaultPruneTailBytes
	}
	return p
}

// validate rejects a configuration that could never reach its own threshold.
//
// Failing loudly here is much better than discovering it mid-session, where the
// symptom is a compaction that runs every step and never frees anything.
func (p Policy) validate() error {
	if p.ContextWindow <= 0 {
		return fmt.Errorf("compact: ContextWindow must be positive")
	}
	if p.ThresholdRatio <= 0 || p.ThresholdRatio > 1 {
		return fmt.Errorf("compact: ThresholdRatio must be in (0, 1], got %v", p.ThresholdRatio)
	}
	if p.HeadroomTokens < 0 {
		return fmt.Errorf("compact: HeadroomTokens must not be negative")
	}
	if p.RetainRatio < 0 || p.RetainTokens < 0 {
		return fmt.Errorf("compact: retention must not be negative")
	}

	budget := p.messageBudget()
	if budget <= 0 {
		return fmt.Errorf(
			"compact: the context window (%d) leaves no room after the output reservation (%d) and headroom (%d)",
			p.ContextWindow, p.OutputReserve, p.HeadroomTokens)
	}

	if p.retain() >= p.Threshold() {
		return fmt.Errorf(
			"compact: retention (%d) must be below the trigger (%d), or compaction would immediately re-trigger",
			p.retain(), p.Threshold())
	}

	if p.PruneHeadBytes+p.PruneTailBytes >= p.PruneThresholdBytes {
		return fmt.Errorf(
			"compact: prune head+tail (%d) must be below the threshold (%d), or pruning would not shrink anything",
			p.PruneHeadBytes+p.PruneTailBytes, p.PruneThresholdBytes)
	}

	return nil
}

// messageBudget is the slice of the window available to messages.
func (p Policy) messageBudget() int {
	return p.ContextWindow - p.OutputReserve
}

// Threshold is the pressure at which compaction triggers.
func (p Policy) Threshold() int {
	byRatio := int(float64(p.ContextWindow) * p.ThresholdRatio)
	byHeadroom := p.messageBudget() - p.HeadroomTokens

	if byHeadroom < byRatio {
		return byHeadroom
	}
	return byRatio
}

// retain is the number of tokens kept verbatim at the tail.
func (p Policy) retain() int {
	if p.RetainTokens > 0 {
		return p.RetainTokens
	}
	return int(float64(p.messageBudget()) * p.RetainRatio)
}

// Summarizer produces the replacement text for a shadowed region.
//
// It is an interface rather than a provider so that a summarizer can be stubbed
// in tests, and so the caller can route summarisation to whichever model it
// wants without this package knowing anything about routing.
type Summarizer interface {
	Summarize(ctx context.Context, region []session.Message, maxTokens int) (string, error)
}

// Operator runs compaction against a session log.
type Operator struct {
	policy     Policy
	summarizer Summarizer
}

// New validates a policy and returns an operator.
func New(policy Policy) (*Operator, error) {
	policy = policy.withDefaults()
	if err := policy.validate(); err != nil {
		return nil, err
	}

	return &Operator{policy: policy}, nil
}

// WithSummarizer attaches a summarizer.
//
// Without one, pruning still works and summarisation is unavailable - the split
// is deliberate, because pruning is the cheap half and should not be blocked on
// a model being configured.
func (o *Operator) WithSummarizer(s Summarizer) *Operator {
	o.summarizer = s
	return o
}

// Policy exposes the resolved policy.
func (o *Operator) Policy() Policy { return o.policy }

// Outcome reports what a compaction did.
type Outcome struct {
	// Compacted is true when the surface was mutated.
	Compacted bool

	// PrunedResults is how many tool results were shrunk.
	PrunedResults int

	// Start and End bound the replaced range, inclusive.
	Start session.Seq
	End   session.Seq

	// Shadowed is how many surface nodes the replacement covered.
	Shadowed int

	// ShadowedTokens and FreedTokens describe the effect on pressure.
	ShadowedTokens int
	FreedTokens    int

	// Summary is the replacement text, empty when only pruning ran.
	Summary string
}

// EstimateTokens approximates the token count of a message list.
func EstimateTokens(messages []session.Message) int {
	total := 0

	for _, m := range messages {
		total += MessageOverheadTokens
		for _, b := range m.Content {
			switch b.Type {
			case session.BlockToolCall:
				total += len(b.Name) / CharsPerToken
				total += len(b.Arguments) / CharsPerToken
			default:
				total += len(b.Text) / CharsPerToken
			}
		}
	}

	return total
}

// Pressure reports the estimated pressure of the whole derived history,
// including the system prompt.
func (o *Operator) Pressure(log *session.Log) int {
	return EstimateTokens(log.DeriveMessages())
}

// Exceeds reports whether pressure has crossed the trigger.
func (o *Operator) Exceeds(log *session.Log) bool {
	return o.Pressure(log) >= o.policy.Threshold()
}

// Prune shrinks oversized tool results in place.
//
// Each replacement is a single-node surface replacement, so the prefix before
// the result stays reusable and only the result's own bytes change. Results
// below the threshold are untouched, which keeps the operation idempotent.
func (o *Operator) Prune(log *session.Log) (Outcome, error) {
	var outcome Outcome

	nodes := log.SurfaceNodes()
	for _, seq := range nodes {
		message, ok := log.MessageAt(seq)
		if !ok || message.Role != session.RoleTool {
			continue
		}

		for _, block := range message.Content {
			if block.Type != session.BlockToolResult || len(block.Text) <= o.policy.PruneThresholdBytes {
				continue
			}

			bounded, omitted := boundToolResult(
				block.Text,
				block.CallID,
				block.IsError,
				o.policy.PruneHeadBytes,
				o.policy.PruneTailBytes,
			)
			if omitted <= 0 {
				continue
			}

			replacement := session.Message{
				Role: session.RoleTool,
				Content: []session.Block{
					session.ToolResult(block.CallID, block.IsError, bounded),
				},
				Source: message.Source,
			}

			if _, err := log.Append(session.EventToolResult, replacement,
				session.ReplaceOp(seq, seq), seq); err != nil {
				return outcome, fmt.Errorf("compact: pruning a tool result: %w", err)
			}

			outcome.PrunedResults++
			outcome.Compacted = true
			return outcome, nil
		}
	}

	return outcome, nil
}

// PruneAll shrinks every oversized tool result in one pass.
func (o *Operator) PruneAll(log *session.Log) (Outcome, error) {
	var total Outcome

	for {
		outcome, err := o.Prune(log)
		if err != nil {
			return total, err
		}
		if outcome.PrunedResults == 0 {
			return total, nil
		}
		total.PrunedResults += outcome.PrunedResults
		total.Compacted = true
	}
}

// selectRange chooses the span to replace.
//
// It returns false when there is nothing safely compactable. The walk starts
// AFTER the system node and moves backwards from the tail, so the retained
// recent history is exactly the newest slice that fits the retention budget.
func (o *Operator) selectRange(log *session.Log) (start, end session.Seq, ok bool) {
	nodes := log.SurfaceNodes()
	if len(nodes) == 0 {
		return 0, 0, false
	}

	derived := log.DeriveMessages()
	if len(derived) != len(nodes) {
		return 0, 0, false
	}

	// Node 0 is the system prompt, and it is never part of a replaced range.
	first := 0
	if derived[0].Role == session.RoleSystem {
		first = 1
	}
	if first >= len(nodes) {
		return 0, 0, false
	}

	// Walk backwards accumulating until the retention budget is met.
	retain := o.policy.retain()
	kept := 0
	keepFrom := len(nodes)

	for i := len(nodes) - 1; i >= first; i-- {
		kept += EstimateTokens([]session.Message{derived[i]})
		keepFrom = i
		if kept >= retain {
			break
		}
	}

	// The range is everything between the system node and the retained tail.
	if keepFrom <= first {
		return 0, 0, false
	}

	return nodes[first], nodes[keepFrom-1], true
}

// CompactIfNeeded prunes, then summarises if pressure is still above the
// trigger.
//
// Pruning runs first on purpose: it needs no model call, and dropping pressure
// with it may avoid the summary - and the summary's cache break - entirely.
func (o *Operator) CompactIfNeeded(ctx context.Context, log *session.Log) (Outcome, error) {
	if !o.Exceeds(log) {
		return Outcome{}, nil
	}

	pruned, err := o.PruneAll(log)
	if err != nil {
		return pruned, err
	}
	if !o.Exceeds(log) {
		return pruned, nil
	}

	summary, err := o.Compact(ctx, log)
	if err != nil {
		return pruned, err
	}

	summary.PrunedResults += pruned.PrunedResults
	return summary, nil
}

// Compact replaces the oldest safely-compactable span with a summary.
func (o *Operator) Compact(ctx context.Context, log *session.Log) (Outcome, error) {
	if o.summarizer == nil {
		return Outcome{}, fmt.Errorf("compact: no summarizer is configured")
	}

	start, end, ok := o.selectRange(log)
	if !ok {
		return Outcome{}, fmt.Errorf("compact: nothing c an be compacted safely")
	}

	region := regionMessages(log, start, end)
	if len(region) == 0 {
		return Outcome{}, fmt.Errorf("compact: the selected range is empty")
	}

	shadowedTokens := EstimateTokens(region)

	summary, err := o.summarizer.Summarize(ctx, region, o.policy.SummaryTokens)
	if err != nil {
		return Outcome{}, fmt.Errorf("compact: summarising: %w", err)
	}
	if strings.TrimSpace(summary) == "" {
		return Outcome{}, fmt.Errorf("compact: the summarizer returned nothing")
	}

	// Shrink validation. A summary that is not smaller than what it replaces
	// would make pressure worse while still paying the cache break, which is the
	// worst of both.
	framedTokens := EstimateTokens([]session.Message{{
		Role:    session.RoleUser,
		Content: []session.Block{session.Text(summary)},
	}})
	if framedTokens >= shadowedTokens {
		return Outcome{}, fmt.Errorf(
			"compact: the summary (%d tokens) is not smaller than the shadowed region (%d tokens)",
			framedTokens, shadowedTokens)
	}

	// The log-only record. It carries the audit trail - what was shadowed and
	// how much - without touching the surface.
	if _, err := log.Append(session.EventCompaction, map[string]any{
		"shadowedTokens": shadowedTokens,
		"summaryTokens":  framedTokens,
		"start":          int64(start),
		"end":            int64(end),
	}, nil); err != nil {
		return Outcome{}, fmt.Errorf("compact: recording the compaction: %w", err)
	}

	sources := make([]session.Seq, 0, 1)
	for _, seq := range log.SurfaceNodes() {
		if seq >= start && seq <= end {
			sources = append(sources, seq)
		}
	}

	// The single surface mutation. Everything else in this method is
	// bookkeeping, so exactly one cache-breaking seam exists per compaction.
	replacement := session.Message{
		Role:    session.RoleUser,
		Content: []session.Block{session.Text(prefixSummary(summary))},
		Source:  "compaction",
	}
	if _, err := log.Append(session.EventUserMessage, replacement,
		session.ReplaceOp(start, end), sources...); err != nil {
		return Outcome{}, fmt.Errorf("compact: replacing the range: %w", err)
	}

	return Outcome{
		Compacted:      true,
		Start:          start,
		End:            end,
		Shadowed:       len(sources),
		ShadowedTokens: shadowedTokens,
		FreedTokens:    shadowedTokens - framedTokens,
		Summary:        summary,
	}, nil
}

// prefixSummary frames the summary so a later reader - human or model - can tell
// it is a derived artefact rather than something the user said.
func prefixSummary(summary string) string {
	return "<compacted-summary>\n" + strings.TrimSpace(summary) + "\n</compacted-summary>"
}

// regionMessages returns the messages between two surface nodes, inclusive.
func regionMessages(log *session.Log, start, end session.Seq) []session.Message {
	nodes := log.SurfaceNodes()

	var out []session.Message
	for _, seq := range nodes {
		if seq < start || seq > end {
			continue
		}
		if m, ok := log.MessageAt(seq); ok {
			out = append(out, m)
		}
	}

	return out
}

// boundToolResult keeps the head and tail of a tool result.
//
// The marker is a fixed literal and the omitted count is exact, so the same
// result always produces the same bytes. That matters beyond readability: a
// pruned result is part of history, and history that varies between identical
// runs would defeat the prefix cache it was trimmed to protect.
func boundToolResult(text, callID string, isError bool, headBytes, tailBytes int) (string, int) {
	if len(text) <= headBytes+tailBytes {
		return text, 0
	}

	head := runeSafePrefix(text, headBytes)
	tail := runeSafeSuffix(text, tailBytes)
	dropped := len(text) - len(head) - len(tail)

	marker := fmt.Sprintf("\n\n[... %d bytes pruned ...]\n\n", dropped)

	return head + marker + tail, dropped
}

func runeSafePrefix(s string, n int) string {
	if n >= len(s) {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func runeSafeSuffix(s string, n int) string {
	if n >= len(s) {
		return s
	}
	start := len(s) - n
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[start:]
}
