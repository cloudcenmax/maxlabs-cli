package llm_test

import (
	"context"
	"fmt"
	"testing"

	"censi/harness/internal/assemble"
	"censi/harness/internal/llm"
	"censi/harness/internal/session"
	"censi/harness/internal/wire"
)

// minHitRate is the series cache-hit gate. It is deliberately below the design
// target so ordinary model variance cannot fail a build, while a structural
// regression - a volatile head, a reordered tool list - fails it decisively.
const minHitRate = 0.85

func fixedClock() session.Clock { return func() int64 { return 1_700_000_000_000 } }

func config(volatileHead bool) assemble.Config {
	cfg := assemble.Config{
		Provider: "local",
		Model:    "default",
		Identity: "You are an AI coding agent. Follow instructions exactly. " +
			"Prefer small, verifiable changes. Never invent a file path you have not read. " +
			"Always run the narrowest test that covers your change before claiming success. " +
			"When a tool returns an error, read it carefully before retrying. " +
			"Keep responses terse and factual.",
		Tools: []wire.ToolSchema{
			{Name: "bash", Description: "Run a shell command in the workspace.", Parameters: []byte(`{"type":"object","properties":{"command":{"type":"string"}}}`)},
			{Name: "read", Description: "Read a file from the workspace.", Parameters: []byte(`{"type":"object","properties":{"path":{"type":"string"}}}`)},
			{Name: "edit", Description: "Replace a literal string in a file.", Parameters: []byte(`{"type":"object","properties":{"path":{"type":"string"},"old":{"type":"string"},"new":{"type":"string"}}}`)},
		},
	}
	if volatileHead {
		// The anti-pattern: a value that changes every step, interpolated into
		// the head. This is what the gate must catch.
		cfg.Guidance = []assemble.Section{
			{Name: "runtime:cwd", Order: assemble.OrderGuidance, Text: ""},
		}
	}
	return cfg
}

// runSession drives nSteps model steps and returns the accumulated usage.
//
// When volatileHead is set, the guidance text is rewritten every step, which is
// exactly what interpolating cwd or a timestamp into the system prompt does.
func runSession(t *testing.T, nSteps int, volatileHead bool) llm.Usage {
	t.Helper()

	log := session.New(fixedClock())
	cfg := config(volatileHead)
	stub := llm.NewPrefixCacheStub("ok")

	var total llm.Usage
	// Series accounting: usage after the first request is what the gate is
	// about, so keep the first step's usage separate.
	var firstStep llm.Usage

	for i := 0; i < nSteps; i++ {
		if volatileHead {
			cfg.Guidance[0].Text = fmt.Sprintf("cwd=/repo step=%d", i)
		}

		// Reserve the head before admitting user input, as the loop does.
		if err := assemble.SyncSystem(log, assemble.RenderSystem(cfg)); err != nil {
			t.Fatalf("step %d: sync system: %v", i, err)
		}

		userText := "Continue the task."
		if i == 0 {
			userText = "List the files in this repository."
		}
		if _, err := log.Append(session.EventUserMessage, session.Message{
			Role:    session.RoleUser,
			Content: []session.Block{session.Text(userText)},
			Source:  "user",
		}, session.AppendOp()); err != nil {
			t.Fatalf("step %d: append user: %v", i, err)
		}

		req, err := assemble.Assemble(log, cfg)
		if err != nil {
			t.Fatalf("step %d: assemble: %v", i, err)
		}

		resp, err := stub.Complete(context.Background(), req)
		if err != nil {
			t.Fatalf("step %d: complete: %v", i, err)
		}
		if i == 0 {
			firstStep = resp.Usage
		}
		total = total.Add(resp.Usage)

		if _, err := log.Append(session.EventAssistantMessage, session.Message{
			Role:    session.RoleAssistant,
			Content: []session.Block{session.Text(resp.Message.Content[0].Text)},
		}, session.AppendOp()); err != nil {
			t.Fatalf("step %d: append assistant: %v", i, err)
		}
	}

	// Exclude the unavoidable cold first request from the series measurement.
	seriesTotal := llm.Usage{
		UncachedInputTokens: total.UncachedInputTokens - firstStep.UncachedInputTokens,
		CacheReadTokens:     total.CacheReadTokens - firstStep.CacheReadTokens,
		CacheWriteTokens:    total.CacheWriteTokens - firstStep.CacheWriteTokens,
		OutputTokens:        total.OutputTokens - firstStep.OutputTokens,
	}
	return seriesTotal
}

// TestCacheHitRateGate is the offline cache gate: an append-only harness must
// exceed the hit-rate target once the first request has primed the cache.
func TestCacheHitRateGate(t *testing.T) {
	usage := runSession(t, 40, false)

	if usage.BilledInputTokens() == 0 {
		t.Fatal("no billed input recorded")
	}
	if got := usage.HitRate(); got < minHitRate {
		t.Fatalf("cache hit rate %.3f is below the %.2f gate (cached=%d uncached=%d)",
			got, minHitRate, usage.CacheReadTokens, usage.UncachedInputTokens)
	}
	t.Logf("cache hit rate after step 1: %.3f (cached=%d uncached=%d)",
		usage.HitRate(), usage.CacheReadTokens, usage.UncachedInputTokens)
}

// TestVolatileHeadCollapsesHitRate is the counter-test that makes the gate
// load-bearing. It reproduces the single most expensive mistake available to
// this design - interpolating a per-step value into the system prompt - and
// asserts that it is caught.
//
// If this test ever starts passing at a high hit rate, the gate has stopped
// measuring anything.
func TestVolatileHeadCollapsesHitRate(t *testing.T) {
	good := runSession(t, 40, false)
	bad := runSession(t, 40, true)

	if bad.HitRate() >= minHitRate {
		t.Fatalf("a per-step system prompt rewrite must collapse the hit rate; got %.3f", bad.HitRate())
	}
	if bad.HitRate() >= good.HitRate() {
		t.Fatalf("volatile head (%.3f) must be worse than append-only (%.3f)", bad.HitRate(), good.HitRate())
	}
	t.Logf("append-only %.3f vs volatile head %.3f (a %.1fx cost regression)",
		good.HitRate(), bad.HitRate(), good.HitRate()/max(bad.HitRate(), 0.0001))
}

// longPrompt builds a system prompt long enough to clear the cache-block floor.
//
// This mirrors a real constraint: a prefix shorter than the block granularity is not
// cached at all, so a test that uses a short prompt measures nothing. Every
// live cache test must clear the floor deliberately rather than by accident.
func longPrompt(words int) string {
	var b []byte
	for i := 0; i < words; i++ {
		if i > 0 {
			b = append(b, ' ')
		}
		b = append(b, []byte("token")...)
		b = append(b, []byte(fmt.Sprintf("%d", i%10))...)
	}
	return string(b)
}

// TestPrefixCacheMatchesOnlyFromTokenZero mirrors the provider rule that makes
// early perturbations so expensive: a change in the middle never hits, because
// matching starts at token 0.
func TestPrefixCacheMatchesOnlyFromTokenZero(t *testing.T) {
	stub := llm.NewPrefixCacheStub("ok")
	ctx := context.Background()

	base := wire.Request{
		System:   longPrompt(200),
		Messages: []session.Message{{Role: session.RoleUser, Content: []session.Block{session.Text("first")}}},
	}

	if _, err := stub.Complete(ctx, base); err != nil {
		t.Fatalf("prime: %v", err)
	}

	// Same prefix, appended tail: must hit.
	extended := base
	extended.Messages = append(append([]session.Message{}, base.Messages...),
		session.Message{Role: session.RoleAssistant, Content: []session.Block{session.Text("ok")}},
		session.Message{Role: session.RoleUser, Content: []session.Block{session.Text("second")}},
	)
	resp, err := stub.Complete(ctx, extended)
	if err != nil {
		t.Fatalf("extend: %v", err)
	}
	if resp.Usage.CacheReadTokens == 0 {
		t.Fatal("an append-extension must hit the cache")
	}

	// Perturb the SYSTEM prompt, i.e. the head: everything after it is lost.
	perturbed := extended
	perturbed.System = "CHANGED " + base.System
	resp, err = stub.Complete(ctx, perturbed)
	if err != nil {
		t.Fatalf("perturb: %v", err)
	}
	if resp.Usage.CacheReadTokens != 0 {
		t.Fatalf("a changed head must not hit the cache; got %d cached tokens", resp.Usage.CacheReadTokens)
	}
}

// TestShortPrefixIsNotCached documents the cache-block floor explicitly, so the
// behaviour is a deliberate contract rather than a surprise in a live run.
func TestShortPrefixIsNotCached(t *testing.T) {
	stub := llm.NewPrefixCacheStub("ok")
	ctx := context.Background()

	short := wire.Request{
		System:   "be brief",
		Messages: []session.Message{{Role: session.RoleUser, Content: []session.Block{session.Text("hi")}}},
	}

	if _, err := stub.Complete(ctx, short); err != nil {
		t.Fatalf("prime: %v", err)
	}
	again := short
	again.Messages = append(append([]session.Message{}, short.Messages...),
		session.Message{Role: session.RoleAssistant, Content: []session.Block{session.Text("ok")}},
	)
	resp, err := stub.Complete(ctx, again)
	if err != nil {
		t.Fatalf("repeat: %v", err)
	}
	if resp.Usage.CacheReadTokens != 0 {
		t.Fatalf("a prefix below the block floor must not be cached; got %d", resp.Usage.CacheReadTokens)
	}
}

// TestUsageBucketsAreDisjoint guards the accounting invariant: billed input is
// the sum of the three disjoint buckets, and a hit rate above 1 is impossible.
func TestUsageBucketsAreDisjoint(t *testing.T) {
	u := llm.Usage{UncachedInputTokens: 100, CacheReadTokens: 900, CacheWriteTokens: 0, OutputTokens: 50}
	if got := u.BilledInputTokens(); got != 1000 {
		t.Fatalf("billed input = %d, want 1000", got)
	}
	if got := u.HitRate(); got < 0.899 || got > 0.901 {
		t.Fatalf("hit rate = %.3f, want ~0.9", got)
	}
	var zero llm.Usage
	if zero.HitRate() != 0 {
		t.Fatal("empty usage must report a zero hit rate, not NaN")
	}
}

func max(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
