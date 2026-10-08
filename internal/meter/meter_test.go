package meter_test

import (
	"math"
	"testing"

	"censi/harness/internal/llm"
	"censi/harness/internal/meter"
)

// TestRepeatedAttemptAtTheSamePositionReplaces proves a provider stream that
// reports usage more than once for one attempt does not double-count.
func TestRepeatedAttemptAtTheSamePositionReplaces(t *testing.T) {
	var m meter.Meter

	m.Record(1, 1, llm.Usage{UncachedInputTokens: 100, OutputTokens: 10})
	m.Record(1, 1, llm.Usage{UncachedInputTokens: 120, OutputTokens: 12})

	got := m.Totals()
	if got.UncachedInputTokens != 120 || got.OutputTokens != 12 {
		t.Fatalf("a superseding sample must replace, not add: %+v", got)
	}
	if m.Attempts() != 2 {
		t.Fatalf("both samples should be counted as observations, got %d", m.Attempts())
	}
}

// TestRetryStartsANewBilledAttempt is the counter-case: after a declared retry,
// the next sample is genuinely additional spend.
func TestRetryStartsANewBilledAttempt(t *testing.T) {
	var m meter.Meter

	m.Record(1, 1, llm.Usage{UncachedInputTokens: 100})
	m.RetryStarted(1, 1)
	m.Record(1, 1, llm.Usage{UncachedInputTokens: 100})

	got := m.Totals()
	if got.UncachedInputTokens != 200 {
		t.Fatalf("a retry is separately billed and must add: %+v", got)
	}
}

// TestRetryWithoutAPriorSampleIsHarmless guards the ordering where the retry
// notice arrives before anything has been recorded.
func TestRetryWithoutAPriorSampleIsHarmless(t *testing.T) {
	var m meter.Meter

	m.RetryStarted(1, 1)
	m.Record(1, 1, llm.Usage{UncachedInputTokens: 50})

	if got := m.Totals().UncachedInputTokens; got != 50 {
		t.Fatalf("totals = %d, want 50", got)
	}
}

// TestDistinctPositionsAccumulate covers ordinary conversation growth.
func TestDistinctPositionsAccumulate(t *testing.T) {
	var m meter.Meter

	m.Record(1, 1, llm.Usage{UncachedInputTokens: 100, CacheReadTokens: 0})
	m.Record(1, 2, llm.Usage{UncachedInputTokens: 10, CacheReadTokens: 300})
	m.Record(2, 1, llm.Usage{UncachedInputTokens: 10, CacheReadTokens: 400})

	got := m.Totals()
	if got.UncachedInputTokens != 120 || got.CacheReadTokens != 700 {
		t.Fatalf("unexpected totals: %+v", got)
	}
}

// TestSeriesTotalsExcludeTheColdStart is the figure the harness is judged on:
// the first request has nothing to reuse, so including it would understate the
// improvement the design is meant to deliver.
func TestSeriesTotalsExcludeTheColdStart(t *testing.T) {
	var m meter.Meter

	m.Record(1, 1, llm.Usage{UncachedInputTokens: 1000})                     // cold start
	m.Record(1, 2, llm.Usage{UncachedInputTokens: 10, CacheReadTokens: 990}) // warm
	m.Record(2, 1, llm.Usage{UncachedInputTokens: 10, CacheReadTokens: 990}) // warm

	series := m.SeriesTotals()
	if series.UncachedInputTokens != 20 || series.CacheReadTokens != 1980 {
		t.Fatalf("unexpected series totals: %+v", series)
	}

	// Whole-session rate is diluted by the cold start; the series rate is not.
	if whole := m.Totals().HitRate(); math.Abs(whole-1980.0/3000.0) > 1e-9 {
		t.Fatalf("whole-session hit rate = %v", whole)
	}
	if seriesRate := m.SeriesHitRate(); math.Abs(seriesRate-0.99) > 1e-9 {
		t.Fatalf("series hit rate = %v, want 0.99", seriesRate)
	}
}

// TestEmptyMeterIsSafe guards the zero value.
func TestEmptyMeterIsSafe(t *testing.T) {
	var m meter.Meter

	if m.Totals() != (llm.Usage{}) {
		t.Fatalf("totals = %+v", m.Totals())
	}
	if m.SeriesTotals() != (llm.Usage{}) {
		t.Fatalf("series totals = %+v", m.SeriesTotals())
	}
	if m.SeriesHitRate() != 0 {
		t.Fatalf("hit rate = %v, want 0", m.SeriesHitRate())
	}
}

// TestSupersedingCannotDriveCountersNegative covers an out-of-order provider
// that reports a smaller sample second.
func TestSupersedingCannotDriveCountersNegative(t *testing.T) {
	var m meter.Meter

	m.Record(1, 1, llm.Usage{CacheReadTokens: 100})
	m.Record(1, 1, llm.Usage{CacheReadTokens: 20})

	got := m.Totals()
	if got.CacheReadTokens != 20 {
		t.Fatalf("totals = %+v", got)
	}
	if got.CacheReadTokens < 0 || got.UncachedInputTokens < 0 {
		t.Fatalf("a counter went negative: %+v", got)
	}
}
