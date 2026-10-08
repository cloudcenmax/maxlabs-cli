// Package meter accumulates provider-reported usage across a session.
//
// Its one subtlety is replacement: a step can be attempted more than once, and a
// later attempt supersedes the earlier one's sample rather than adding to it —
// except when a retry explicitly opens a new billed attempt, in which case both
// were genuinely billed and both count.
//
// KV Cache effect: none. Measurement observes; it never assembles or alters a
// request.
package meter

import "censi/harness/internal/llm"

// position identifies one model attempt within a session.
type position struct {
	Turn int
	Step int
}

// Meter folds usage samples into running totals.
//
// The zero value is ready to use.
type Meter struct {
	totals llm.Usage

	// last is the sample a subsequent attempt at the same position would
	// supersede. A retry clears it, so the next sample is added instead.
	last      *position
	lastUsage llm.Usage

	first    llm.Usage
	hasFirst bool
	attempts int
}

// Record folds one attempt's usage.
//
// Repeated attempts at the same position replace the earlier sample, so a
// provider that reports only its final attempt does not double-count. Call
// RetryStarted to declare that the next attempt is separately billed.
func (m *Meter) Record(turn, step int, usage llm.Usage) {
	pos := position{Turn: turn, Step: step}

	if m.last != nil && *m.last == pos {
		m.totals = subtract(m.totals, m.lastUsage)
	}

	m.totals = add(m.totals, usage)

	if !m.hasFirst {
		m.first = usage
		m.hasFirst = true
	}

	m.attempts++
	m.last = &pos
	m.lastUsage = usage
}

// RetryStarted declares that the next attempt at this position is a distinct
// billed attempt rather than a replacement of the previous sample.
func (m *Meter) RetryStarted(turn, step int) {
	if m.last != nil && *m.last == (position{Turn: turn, Step: step}) {
		m.last = nil
	}
}

// Totals returns whole-session usage.
func (m *Meter) Totals() llm.Usage { return m.totals }

// Attempts returns how many samples were folded.
func (m *Meter) Attempts() int { return m.attempts }

// SeriesTotals returns usage with the first attempt excluded.
//
// The first request of a session has nothing to reuse, so including it would
// dilute the very figure the harness exists to move. Cache behaviour is a
// property of the series, not of the cold start.
func (m *Meter) SeriesTotals() llm.Usage {
	if !m.hasFirst {
		return llm.Usage{}
	}
	return subtract(m.totals, m.first)
}

// SeriesHitRate is the cache hit rate of every attempt after the first.
func (m *Meter) SeriesHitRate() float64 { return m.SeriesTotals().HitRate() }

func add(a, b llm.Usage) llm.Usage {
	return llm.Usage{
		UncachedInputTokens: a.UncachedInputTokens + b.UncachedInputTokens,
		CacheReadTokens:     a.CacheReadTokens + b.CacheReadTokens,
		CacheWriteTokens:    a.CacheWriteTokens + b.CacheWriteTokens,
		OutputTokens:        a.OutputTokens + b.OutputTokens,
	}
}

// subtract removes b from a, clamping at zero so a superseded sample can never
// drive a counter negative.
func subtract(a, b llm.Usage) llm.Usage {
	return llm.Usage{
		UncachedInputTokens: clamp(a.UncachedInputTokens - b.UncachedInputTokens),
		CacheReadTokens:     clamp(a.CacheReadTokens - b.CacheReadTokens),
		CacheWriteTokens:    clamp(a.CacheWriteTokens - b.CacheWriteTokens),
		OutputTokens:        clamp(a.OutputTokens - b.OutputTokens),
	}
}

func clamp(v int) int {
	if v < 0 {
		return 0
	}
	return v
}
