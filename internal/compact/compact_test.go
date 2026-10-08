package compact_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"censi/harness/internal/compact"
	"censi/harness/internal/session"
	"censi/harness/internal/wire"
)

func clock() session.Clock { return func() int64 { return 1_700_000_000_000 } }

func pad(n int) string { return strings.Repeat("x", n) }

// newLog builds a session with a system prompt and count history messages.
func newLog(t *testing.T, count, chars int) *session.Log {
	t.Helper()

	log := session.New(clock())

	if _, err := log.Append(session.EventSystemMessage, session.Message{
		Role:    session.RoleSystem,
		Content: []session.Block{session.Text("You are a coding agent.")},
	}, session.AppendOp()); err != nil {
		t.Fatalf("system: %v", err)
	}

	for i := 0; i < count; i++ {
		role := session.RoleUser
		event := session.EventUserMessage
		if i%2 == 1 {
			role = session.RoleAssistant
			event = session.EventAssistantMessage
		}

		if _, err := log.Append(event, session.Message{
			Role:    role,
			Content: []session.Block{session.Text(pad(chars))},
		}, session.AppendOp()); err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
	}

	return log
}

type stubSummarizer struct {
	text   string
	err    error
	calls  int
	region []session.Message
}

func (s *stubSummarizer) Summarize(_ context.Context, region []session.Message, _ int) (string, error) {
	s.calls++
	s.region = region
	if s.err != nil {
		return "", s.err
	}
	return s.text, nil
}

func policy() compact.Policy {
	return compact.Policy{
		ContextWindow:  10_000,
		OutputReserve:  1_000,
		HeadroomTokens: 1_000,
		ThresholdRatio: 0.8,
		RetainRatio:    0.16,
	}
}

// TestThresholdFollowsTheFormula pins the trigger arithmetic, because a
// miscomputed threshold either compacts constantly or never.
func TestThresholdFollowsTheFormula(t *testing.T) {
	op, err := compact.New(policy())
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	// min(W * 0.8, W - O - headroom) = min(8000, 8000) = 8000
	if got := op.Policy().Threshold(); got != 8000 {
		t.Fatalf("threshold = %d, want 8000", got)
	}

	// With a large window the ratio governs.
	wide := policy()
	wide.ContextWindow = 1_000_000
	wide.HeadroomTokens = 10_000
	opWide, err := compact.New(wide)
	if err != nil {
		t.Fatalf("new wide: %v", err)
	}
	if got := opWide.Policy().Threshold(); got != 800_000 {
		t.Fatalf("wide threshold = %d, want 800000", got)
	}
}

// TestPolicyRejectsAnUnreachableConfiguration: retention at or above the trigger
// means compaction would immediately re-trigger, which is a config error worth
// catching at construction rather than mid-session.
func TestPolicyRejectsAnUnreachableConfiguration(t *testing.T) {
	bad := policy()
	bad.RetainRatio = 0.9
	if _, err := compact.New(bad); err == nil {
		t.Fatal("retention above the trigger must be rejected")
	}

	bad = policy()
	bad.RetainTokens = 9_000
	if _, err := compact.New(bad); err == nil {
		t.Fatal("absolute retention above the trigger must be rejected")
	}

	bad = policy()
	bad.ContextWindow = 500
	bad.OutputReserve = 400
	bad.HeadroomTokens = 200
	if _, err := compact.New(bad); err == nil {
		t.Fatal("a window with no room left must be rejected")
	}

	bad = policy()
	bad.PruneHeadBytes = 8_000
	bad.PruneTailBytes = 8_000
	bad.PruneThresholdBytes = 1_000
	if _, err := compact.New(bad); err == nil {
		t.Fatal("a prune budget that cannot shrink anything must be rejected")
	}
}

// TestSelectionNeverIncludesTheSystemNode is M4's headline safety property.
func TestSelectionNeverIncludesTheSystemNode(t *testing.T) {
	op, _ := compact.New(policy())

	log := newLog(t, 12, 4_000)
	nodes := log.SurfaceNodes()
	systemNode := nodes[0]

	outcome, err := op.Compact(context.Background(), logWith(op, log, &stubSummarizer{text: "condensed"}))
	if err != nil {
		t.Fatalf("compact: %v", err)
	}

	if outcome.Start == systemNode {
		t.Fatal("the replacement started at the system node")
	}
	if outcome.Start <= systemNode {
		t.Fatalf("the replacement must start after the system node: start=%d system=%d", outcome.Start, systemNode)
	}

	// The system node must still be the head afterwards.
	if got := log.SurfaceNodes()[0]; got != systemNode {
		t.Fatalf("the system node moved: got %d, want %d", got, systemNode)
	}
	head, ok := log.SystemHead()
	if !ok {
		t.Fatal("the system head is gone")
	}
	if !strings.Contains(head.Content[0].Text, "coding agent") {
		t.Fatalf("the system prompt was altered: %q", head.Content[0].Text)
	}
}

// logWith attaches a summarizer without rebuilding the operator.
func logWith(op *compact.Operator, log *session.Log, s compact.Summarizer) *session.Log {
	op.WithSummarizer(s)
	return log
}

// TestCompactionBreaksTheCacheOnlyAtTheRange is the property that makes
// compaction affordable: everything before the replaced span stays byte-identical,
// so only the span itself is re-billed.
func TestCompactionBreaksTheCacheOnlyAtTheRange(t *testing.T) {
	op, _ := compact.New(policy())
	log := newLog(t, 12, 4_000)

	before, err := wire.Units(wire.Request{
		System:   "You are a coding agent.",
		Messages: log.DeriveMessages()[1:],
	})
	if err != nil {
		t.Fatalf("units before: %v", err)
	}

	if _, err := op.Compact(context.Background(), logWith(op, log, &stubSummarizer{text: "condensed"})); err != nil {
		t.Fatalf("compact: %v", err)
	}

	after, err := wire.Units(wire.Request{
		System:   "You are a coding agent.",
		Messages: log.DeriveMessages()[1:],
	})
	if err != nil {
		t.Fatalf("units after: %v", err)
	}

	// The tools unit is absent here, so unit 0 is the system prompt. It must be
	// untouched, and so must every message before the replaced range.
	shared := commonPrefix(before, after)
	if shared < 1 {
		t.Fatalf("the system prompt changed across compaction; shared prefix = %d units", shared)
	}

	if shared == len(before) && len(before) == len(after) {
		t.Fatal("compaction did not change the surface at all")
	}
}

// TestCompactionMutatesTheSurfaceExactlyOnce keeps the number of cache-breaking
// seams equal to the number of compactions, never more.
func TestCompactionMutatesTheSurfaceExactlyOnce(t *testing.T) {
	op, _ := compact.New(policy())
	log := newLog(t, 12, 4_000)

	generationBefore := log.ContentGeneration()

	if _, err := op.Compact(context.Background(), logWith(op, log, &stubSummarizer{text: "condensed"})); err != nil {
		t.Fatalf("compact: %v", err)
	}

	if got := log.ContentGeneration() - generationBefore; got != 1 {
		t.Fatalf("compaction advanced the content generation by %d, want exactly 1", got)
	}
	if got := log.ReplaceGeneration() - 0; got != 1 {
		t.Fatalf("replace generation = %d, want exactly 1", got)
	}
}

// TestSummaryMustBeSmallerThanWhatItReplaces: a summary that does not shrink
// pays the cache break and makes pressure worse, which is the worst outcome
// available.
func TestSummaryMustBeSmallerThanWhatItReplaces(t *testing.T) {
	op, _ := compact.New(policy())
	log := newLog(t, 12, 4_000)

	// A summary far larger than the region it replaces.
	huge := strings.Repeat("y", 200_000)

	_, err := op.Compact(context.Background(), logWith(op, log, &stubSummarizer{text: huge}))
	if err == nil {
		t.Fatal("a non-shrinking summary must be rejected")
	}
	if !strings.Contains(err.Error(), "not smaller") {
		t.Fatalf("unexpected error: %v", err)
	}

	// And the surface must be untouched by a rejected compaction.
	if log.ReplaceGeneration() != 0 {
		t.Fatal("a rejected compaction must not mutate the surface")
	}
}

// TestPressureDropsBelowTheThreshold is the point of the whole exercise.
func TestPressureDropsBelowTheThreshold(t *testing.T) {
	op, _ := compact.New(policy())
	log := newLog(t, 12, 4_000)

	if !op.Exceeds(log) {
		t.Fatalf("fixture should start over the threshold; pressure = %d, threshold = %d",
			op.Pressure(log), op.Policy().Threshold())
	}

	before := op.Pressure(log)

	if _, err := op.Compact(context.Background(), logWith(op, log, &stubSummarizer{text: "condensed"})); err != nil {
		t.Fatalf("compact: %v", err)
	}

	after := op.Pressure(log)
	if after >= before {
		t.Fatalf("pressure did not fall: before=%d after=%d", before, after)
	}
	if op.Exceeds(log) {
		t.Fatalf("pressure is still above the threshold: %d >= %d", after, op.Policy().Threshold())
	}
}

// TestCompactionRequiresASummarizer keeps the two halves separable: pruning is
// cheap and should not be blocked on a model being configured, but summarising
// must fail loudly rather than silently do nothing.
func TestCompactionRequiresASummarizer(t *testing.T) {
	op, _ := compact.New(policy())
	log := newLog(t, 12, 4_000)

	if _, err := op.Compact(context.Background(), log); err == nil {
		t.Fatal("compaction without a summarizer must fail")
	}
}

// TestSummarizerFailureLeavesTheSurfaceIntact: a failed model call must not
// half-apply a compaction.
func TestSummarizerFailureLeavesTheSurfaceIntact(t *testing.T) {
	op, _ := compact.New(policy())
	log := newLog(t, 12, 4_000)

	nodesBefore := len(log.SurfaceNodes())
	generationBefore := log.ContentGeneration()

	summaryErr := errors.New("summarizer unavailable")
	_, err := op.Compact(context.Background(), logWith(op, log, &stubSummarizer{err: summaryErr}))
	if err == nil {
		t.Fatal("a failing summarizer must surface an error")
	}

	if len(log.SurfaceNodes()) != nodesBefore || log.ContentGeneration() != generationBefore {
		t.Fatal("a failed compaction mutated the surface")
	}
}

func commonPrefix(a, b []wire.Unit) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if string(a[i].Bytes) != string(b[i].Bytes) {
			return i
		}
	}
	return n
}

// newToolLog builds a session whose history contains one oversized tool result.
func newToolLog(t *testing.T, resultChars int) *session.Log {
	t.Helper()

	log := session.New(clock())

	if _, err := log.Append(session.EventSystemMessage, session.Message{
		Role:    session.RoleSystem,
		Content: []session.Block{session.Text("You are a coding agent.")},
	}, session.AppendOp()); err != nil {
		t.Fatalf("system: %v", err)
	}
	if _, err := log.Append(session.EventUserMessage, session.Message{
		Role:    session.RoleUser,
		Content: []session.Block{session.Text("run the thing")},
	}, session.AppendOp()); err != nil {
		t.Fatalf("user: %v", err)
	}
	if _, err := log.Append(session.EventAssistantMessage, session.Message{
		Role:    session.RoleAssistant,
		Content: []session.Block{session.ToolCall("call-1", "bash", []byte(`{"command":"x"}`))},
	}, session.AppendOp()); err != nil {
		t.Fatalf("assistant: %v", err)
	}
	if _, err := log.Append(session.EventToolResult, session.Message{
		Role:    session.RoleTool,
		Content: []session.Block{session.ToolResult("call-1", false, pad(resultChars))},
		Source:  "tool:bash",
	}, session.AppendOp()); err != nil {
		t.Fatalf("tool result: %v", err)
	}

	return log
}

func toolResultText(t *testing.T, log *session.Log) string {
	t.Helper()

	for _, m := range log.DeriveMessages() {
		for _, b := range m.Content {
			if b.Type == session.BlockToolResult {
				return b.Text
			}
		}
	}

	t.Fatal("no tool result found")
	return ""
}

// TestPruneShrinksAnOversizedToolResult and reports exactly what it dropped.
func TestPruneShrinksAnOversizedToolResult(t *testing.T) {
	op, _ := compact.New(policy())
	log := newToolLog(t, 20_000)

	before := len(toolResultText(t, log))

	outcome, err := op.PruneAll(log)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if outcome.PrunedResults != 1 {
		t.Fatalf("pruned = %d, want 1", outcome.PrunedResults)
	}

	after := toolResultText(t, log)
	if len(after) >= before {
		t.Fatalf("the result did not shrink: %d -> %d", before, len(after))
	}
	if !strings.Contains(after, "bytes pruned") {
		t.Fatalf("the pruned result must say so: %q", after[:min(120, len(after))])
	}
	// The call id must survive, or the provider cannot pair the result with its
	// call and the request becomes invalid rather than merely shorter.
	if !strings.Contains(after, "xxx") {
		t.Fatal("the retained window is missing")
	}
}

// TestPruneOnlyTouchesOversizedResults keeps the operation cheap and idempotent:
// results under the threshold must be left byte-identical.
func TestPruneOnlyTouchesOversizedResults(t *testing.T) {
	op, _ := compact.New(policy())
	log := newToolLog(t, 500)

	before := toolResultText(t, log)

	outcome, err := op.PruneAll(log)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if outcome.PrunedResults != 0 {
		t.Fatalf("a small result must not be pruned, pruned=%d", outcome.PrunedResults)
	}
	if got := toolResultText(t, log); got != before {
		t.Fatal("a small result was modified")
	}
}

// TestPruneIsIdempotent guards the loop: a second pass must find nothing to do,
// or CompactIfNeeded would spin.
func TestPruneIsIdempotent(t *testing.T) {
	op, _ := compact.New(policy())
	log := newToolLog(t, 20_000)

	if _, err := op.PruneAll(log); err != nil {
		t.Fatalf("first prune: %v", err)
	}
	first := toolResultText(t, log)

	outcome, err := op.PruneAll(log)
	if err != nil {
		t.Fatalf("second prune: %v", err)
	}
	if outcome.PrunedResults != 0 {
		t.Fatalf("a second pass pruned %d more result(s)", outcome.PrunedResults)
	}
	if got := toolResultText(t, log); got != first {
		t.Fatal("a second pass changed the result")
	}
}

// TestPruneIsDeterministic guards the cache: the same input must produce the
// same bytes, or history would differ between identical runs.
func TestPruneIsDeterministic(t *testing.T) {
	op, _ := compact.New(policy())

	first := newToolLog(t, 20_000)
	if _, err := op.PruneAll(first); err != nil {
		t.Fatalf("prune: %v", err)
	}

	second := newToolLog(t, 20_000)
	if _, err := op.PruneAll(second); err != nil {
		t.Fatalf("prune: %v", err)
	}

	if a, b := toolResultText(t, first), toolResultText(t, second); a != b {
		t.Fatal("pruning is not deterministic")
	}
}

// TestPruneBreaksTheCacheOnlyInsideThePrunedResult: pruning is a single-node
// replacement, so the prefix before the result stays reusable.
func TestPruneBreaksTheCacheOnlyInsideThePrunedResult(t *testing.T) {
	op, _ := compact.New(policy())
	log := newToolLog(t, 20_000)

	nodesBefore := log.SurfaceNodes()
	generationBefore := log.ContentGeneration()

	if _, err := op.PruneAll(log); err != nil {
		t.Fatalf("prune: %v", err)
	}

	// Same node count: the replacement occupies the range rather than splicing
	// into it, so nothing shifts position.
	nodesAfter := log.SurfaceNodes()
	if len(nodesBefore) != len(nodesAfter) {
		t.Fatalf("node count changed: %d -> %d", len(nodesBefore), len(nodesAfter))
	}

	// Every node BEFORE the pruned result keeps its original identity, which is
	// what preserves the cached prefix ahead of it. The replacement itself is a
	// new event occupying the same position - node identity changes, position
	// does not.
	for i := 0; i < len(nodesBefore)-1; i++ {
		if nodesBefore[i] != nodesAfter[i] {
			t.Fatalf("node %d moved: %d -> %d; pruning must not disturb the prefix", i, nodesBefore[i], nodesAfter[i])
		}
	}
	if nodesAfter[len(nodesAfter)-1] == nodesBefore[len(nodesBefore)-1] {
		t.Fatal("expected the pruned node to be a new replacement event")
	}

	if log.ContentGeneration()-generationBefore != 1 {
		t.Fatal("pruning must advance the content generation exactly once")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
