package tui

import (
	"strings"
	"testing"
)

// TestVisibleWidthIgnoresEscapes: measuring in bytes would cut a coloured line
// short while a plain one fitted.
func TestVisibleWidthIgnoresEscapes(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"hello", 5},
		{"", 0},
		{pink() + "hello" + reset, 5},
		{boldOn + "hi" + boldOff + " there", 8},
		{"\x1b[1A\x1b[K", 0},
	}

	for _, c := range cases {
		if got := visibleWidth(c.in); got != c.want {
			t.Errorf("visibleWidth(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// TestTruncateVisibleClosesStyling: a cut that lands mid-style would leave the
// colour on and bleed into everything written afterwards.
func TestTruncateVisibleClosesStyling(t *testing.T) {
	styled := pink() + "a long coloured line" + reset

	cut := truncateVisible(styled, 6)

	if got := visibleWidth(cut); got > 6 {
		t.Fatalf("truncated to %d columns, want at most 6", got)
	}
	if !strings.HasSuffix(cut, reset) {
		t.Fatalf("the cut left styling open: %q", cut)
	}
	if !strings.Contains(cut, "a long") {
		t.Fatalf("too much was removed: %q", cut)
	}
}

// TestTruncateVisibleLeavesShortLinesAlone keeps it from rewriting what fits.
func TestTruncateVisibleLeavesShortLinesAlone(t *testing.T) {
	in := "short"

	if got := truncateVisible(in, 20); got != in {
		t.Fatalf("got %q, want it unchanged", got)
	}
}

// TestStatusLineNeverExceedsTheWidth is the fix for the scrambling.
//
// The pinned row is addressed by moving the cursor up one row. If the status is
// wider than the terminal it wraps, occupies two rows, and every subsequent
// redraw is off by one - which is what made the transcript break apart.
func TestStatusLineNeverExceedsTheWidth(t *testing.T) {
	for _, width := range []int{20, 40, 80, 120} {
		var buf strings.Builder

		st := newStatus(&buf, true)
		st.setWidth(width)
		st.begin("act")
		st.setPlanMode(true)
		st.setProgress(12, 987654, 0.87)
		st.setInput("> a fairly long thing being typed here")

		line := st.fitLocked(st.lineLocked())

		if got := visibleWidth(line); got > width-1 {
			t.Errorf("width %d: status line is %d columns and would wrap", width, got)
		}
	}
}

// TestStatusLineFitsWhenPlanModeIsOn: plan mode adds the longest suffix, so it
// is the case most likely to overflow.
func TestStatusLineFitsWhenPlanModeIsOn(t *testing.T) {
	var buf strings.Builder

	st := newStatus(&buf, true)
	st.setWidth(24)
	st.begin("act")
	st.setPlanMode(true)
	st.setProgress(3, 12400, 0.68)

	line := st.fitLocked(st.lineLocked())

	if visibleWidth(line) > 23 {
		t.Fatalf("plan-mode status is %d columns at width 24: %q", visibleWidth(line), line)
	}
}

// TestWrapVisibleBreaksAtWords keeps prose from being chopped mid-word.
func TestWrapVisibleBreaksAtWords(t *testing.T) {
	text := "the quick brown fox jumps over the lazy dog and keeps going"

	chunks, rest := wrapVisible(text, 20)

	for _, chunk := range chunks {
		if visibleWidth(chunk) > 20 {
			t.Fatalf("chunk %q is %d columns", chunk, visibleWidth(chunk))
		}
		if strings.HasPrefix(chunk, " ") || strings.HasSuffix(chunk, " ") {
			t.Fatalf("chunk has stray whitespace: %q", chunk)
		}
		// Every word in a chunk should be whole.
		for _, word := range strings.Fields(chunk) {
			if !strings.Contains(text, word) {
				t.Fatalf("chunk split a word: %q", chunk)
			}
		}
	}

	joined := strings.Join(append(chunks, rest), " ")
	if strings.Join(strings.Fields(joined), " ") != strings.Join(strings.Fields(text), " ") {
		t.Fatalf("wrapping changed the text:\n got: %q\nwant: %q", joined, text)
	}
}

// TestWrapVisibleHardSplitsAnUnbreakableWord: a long URL still has to fit.
func TestWrapVisibleHardSplitsAnUnbreakableWord(t *testing.T) {
	text := strings.Repeat("x", 50)

	chunks, rest := wrapVisible(text, 10)

	if len(chunks) == 0 {
		t.Fatal("nothing was produced")
	}
	for _, chunk := range chunks {
		if visibleWidth(chunk) > 10 {
			t.Fatalf("chunk %q overflowed", chunk)
		}
	}
	if got := len(strings.Join(chunks, "")) + len(rest); got != 50 {
		t.Fatalf("characters were lost: %d of 50", got)
	}
}

// TestMarkdownWrapsToTheTerminalWidth is the streaming half of the fix.
func TestMarkdownWrapsToTheTerminalWidth(t *testing.T) {
	m := &markdown{styled: true, width: 30}

	long := strings.TrimSpace(strings.Repeat("word ", 40))
	out := m.feed(long)
	out += m.flush()

	for _, line := range strings.Split(strings.TrimRight(stripAnsi(out), "\n"), "\n") {
		if visibleWidth(line) > 30 {
			t.Fatalf("a line is %d columns at width 30: %q", visibleWidth(line), line)
		}
	}
	if !strings.Contains(stripAnsi(out), "word word") {
		t.Fatalf("content was lost: %q", stripAnsi(out))
	}
}

// TestMarkdownUsesTheWholeWidth pins that wrapping is not gratuitously early.
func TestMarkdownUsesTheWholeWidth(t *testing.T) {
	m := &markdown{styled: false, width: 40}

	out := m.feed(strings.Repeat("ab ", 30))
	out += m.flush()

	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if len(line) > 39 {
			t.Fatalf("line of %d columns at width 40: %q", len(line), line)
		}
	}

	// At least one line should be close to the limit, or we are wrapping far too
	// early and the output is ragged for no reason.
	longest := 0
	for _, line := range strings.Split(out, "\n") {
		if len(line) > longest {
			longest = len(line)
		}
	}
	if longest < 30 {
		t.Fatalf("longest line is only %d columns at width 40; wrapping too early", longest)
	}
}
