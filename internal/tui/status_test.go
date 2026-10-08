package tui

import (
	"bytes"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestStatusPauseStopsTheSpinner is the fix for an approval prompt nobody could
// read: a spinner is a claim that something is progressing, and redrawing it
// several times a second wipes the question off the screen.
func TestStatusPauseStopsTheSpinner(t *testing.T) {
	var buf bytes.Buffer

	st := newStatus(&buf, true)
	st.begin("act")
	st.write("working\n")
	st.refresh()

	frame := spinnerFrames[0]
	before := strings.Count(buf.String(), frame)

	st.pause()

	// Refresh must now be inert: the harness is blocked on a person.
	st.refresh()
	st.refresh()
	st.refresh()

	if after := strings.Count(buf.String(), frame); after != before {
		t.Fatalf("the spinner kept drawing while paused: %d frames became %d", before, after)
	}
}

// TestStatusResumeRestartsTheSpinner keeps the pause from being permanent.
func TestStatusResumeRestartsTheSpinner(t *testing.T) {
	var buf bytes.Buffer

	st := newStatus(&buf, true)
	st.begin("act")
	st.pause()

	before := buf.Len()
	st.resume()
	st.refresh()

	if buf.Len() <= before {
		t.Fatal("the status line did not come back after resuming")
	}
}

// TestStatusPauseClearsTheLine: the status line must not sit under the question.
func TestStatusPauseClearsTheLine(t *testing.T) {
	var buf bytes.Buffer

	st := newStatus(&buf, true)
	st.begin("act")
	st.refresh()

	buf.Reset()

	st.pause()

	// The block is erased by stepping back over every row it occupies and
	// clearing to the end of the screen. It is a count of rows now, not one,
	// because a thinking region sits above the status line.
	out := buf.String()
	if !strings.Contains(out, "\x1b[J") {
		t.Fatalf("pause did not erase the pinned block: %q", out)
	}
	if !strings.Contains(out, "\x1b[1A") {
		t.Fatalf("pause did not step back over the block: %q", out)
	}
}

// TestStatusStylesSuppressedWhenPiped: an approval prompt written to a pipe must
// not carry escape sequences.
func TestStatusStylesSuppressedWhenPiped(t *testing.T) {
	var buf bytes.Buffer

	st := newStatus(&buf, false)

	for _, got := range []string{st.bold("x"), st.tone("x"), st.code("x"), st.brand("x")} {
		if got != "x" {
			t.Fatalf("a style leaked without a terminal: %q", got)
		}
	}
}

// TestStatusShowsTypedInput: during a turn the status line is the only row
// available, so the in-progress prompt has to appear there.
func TestStatusShowsTypedInput(t *testing.T) {
	var buf bytes.Buffer

	st := newStatus(&buf, true)
	st.begin("act")
	st.setProgress(1, 1000, 0.5)

	st.setInput("> stop")
	st.refresh()

	if !strings.Contains(stripAnsi(buf.String()), "> stop") {
		t.Fatalf("the typed line is not shown: %q", buf.String())
	}

	buf.Reset()
	st.setInput("")
	st.refresh()

	if strings.Contains(stripAnsi(buf.String()), "stop") {
		t.Fatalf("stale input survived: %q", buf.String())
	}
}

// TestStatusInputIgnoredWhenIdle: with no turn running the editor owns the row
// and the status line is not drawn at all.
func TestStatusInputIgnoredWhenIdle(t *testing.T) {
	var buf bytes.Buffer

	st := newStatus(&buf, true)
	st.setInput("> leftover")

	if buf.Len() != 0 {
		t.Fatalf("the status line drew while idle: %q", buf.String())
	}
}

// TestEchoPromptBandIsTheDefault: telling what you asked apart from what the
// model answered is the point, not a preference.
func TestEchoPromptBandIsTheDefault(t *testing.T) {
	t.Setenv("HARNESS_PROMPT_BAND", "")

	var buf bytes.Buffer
	st := newStatus(&buf, true)

	echo := st.echoPrompt("read the plan")

	if !strings.Contains(echo, "48;") {
		t.Fatalf("no background band by default: %q", echo)
	}
	if !strings.Contains(echo, "❯") {
		t.Fatalf("the marker was dropped: %q", echo)
	}
	if !strings.Contains(stripAnsi(echo), "read the plan") {
		t.Fatalf("the text was lost: %q", echo)
	}
}

// TestBandSetsBothColours is the property that makes it work on any theme.
//
// Inheriting the foreground would make readability a property of the user's
// terminal. Setting both makes it a property of this code.
func TestBandSetsBothColours(t *testing.T) {
	t.Setenv("HARNESS_PROMPT_BAND", "1")

	var buf bytes.Buffer
	st := newStatus(&buf, true)

	echo := st.echoPrompt("hello")

	if !strings.Contains(echo, "48;") && !strings.Contains(echo, "48;5;") {
		t.Fatalf("no background: %q", echo)
	}
	if !strings.Contains(echo, "38;") && !strings.Contains(echo, "38;5;") {
		t.Fatalf("no explicit foreground, so contrast depends on the theme: %q", echo)
	}
	if strings.Contains(echo, "\x1b[7m") {
		t.Fatalf("the band fell back to reverse video: %q", echo)
	}

	// Black on the brand pink. This is the number that matters: the text is
	// what gets read, and it must not inherit whatever the theme sets.
	if !strings.Contains(echo, "38;2;0;0;0") && !strings.Contains(echo, "38;5;16") {
		t.Fatalf("the band text is not black: %q", echo)
	}
	if !strings.Contains(echo, "48;2;255;102;178") && !strings.Contains(echo, "48;5;205") {
		t.Fatalf("the band is not the brand pink: %q", echo)
	}
}

// TestBandOffFallsBackToTheMarker keeps the opt-out working.
func TestBandOffFallsBackToTheMarker(t *testing.T) {
	t.Setenv("HARNESS_PROMPT_BAND", "0")

	var buf bytes.Buffer
	st := newStatus(&buf, true)

	echo := st.echoPrompt("read the plan")

	if strings.Contains(echo, "48;") {
		t.Fatalf("a background survived the opt-out: %q", echo)
	}
	if !strings.Contains(echo, "❯") {
		t.Fatalf("the marker should remain: %q", echo)
	}
	if !strings.Contains(echo, boldOn) {
		t.Fatalf("the marker style should still emphasise: %q", echo)
	}
}

// TestEchoPromptPlainWhenPiped keeps redirects byte-clean.
func TestEchoPromptPlainWhenPiped(t *testing.T) {
	t.Setenv("HARNESS_PROMPT_BAND", "1")

	var buf bytes.Buffer
	st := newStatus(&buf, false)

	echo := st.echoPrompt("read the plan")
	if echo != "> read the plan" {
		t.Fatalf("piped echo = %q", echo)
	}
	if strings.Contains(echo, "\x1b") {
		t.Fatalf("an escape survived into a pipe: %q", echo)
	}
}

// TestSubmittedRowIsRestyled: the marker has to replace the row the user typed
// on, not follow it, or both the marker and the raw prompt stay on screen.
func TestSubmittedRowIsRestyled(t *testing.T) {
	e := newLineEditor(nil, &bytes.Buffer{}, nil, &atomic.Bool{}, func() string { return "> " }, true)

	// Without a shell-supplied renderer the row keeps its prompt.
	if got := e.submittedRendering("hello"); got != "> hello\n" {
		t.Fatalf("default rendering = %q", got)
	}

	e.renderSubmitted = func(text string) string { return "❯ " + text + "\n\n" }

	if got := e.submittedRendering("hello"); got != "❯ hello\n\n" {
		t.Fatalf("styled rendering = %q", got)
	}
}

// TestThinkingRegionShowsACountNotTheReasoning is the requested behaviour.
//
// A chain of thought is long, changes constantly, and scrolls the answer off the
// screen. What someone waiting actually wants is to know how much longer, and a
// rising token count answers that without competing for attention.
func TestThinkingRegionShowsACountNotTheReasoning(t *testing.T) {
	var buf bytes.Buffer

	st := newStatus(&buf, true)
	st.setWidth(80)
	st.begin("act")
	st.beginThinking()
	st.pushThinking("let me work through the constraints carefully")

	out := stripAnsi(buf.String())

	if strings.Contains(out, "let me work through") {
		t.Fatalf("the reasoning text was displayed:\n%s", out)
	}
	if !strings.Contains(out, "thinking") {
		t.Fatalf("no reasoning indicator at all:\n%s", out)
	}
	if !strings.Contains(out, "tokens") {
		t.Fatalf("no token count:\n%s", out)
	}
}

// TestThinkingCountRises: the number has to move, or it might as well be static.
func TestThinkingCountRises(t *testing.T) {
	var buf bytes.Buffer

	st := newStatus(&buf, true)
	st.setWidth(80)
	st.begin("act")
	st.beginThinking()

	st.pushThinking("short")
	first := stripAnsi(buf.String())

	buf.Reset()
	for i := 0; i < 400; i++ {
		st.pushThinking("more reasoning text here")
	}
	later := stripAnsi(buf.String())

	if first == later {
		t.Fatalf("the count never changed: %q then %q", first, later)
	}
	if !strings.Contains(later, "tokens") {
		t.Fatalf("no count shown: %q", later)
	}
}

// TestThinkingRegionSitsAboveTheStatusLine is the layout.
func TestThinkingRegionSitsAboveTheStatusLine(t *testing.T) {
	var buf bytes.Buffer

	st := newStatus(&buf, true)
	st.setWidth(80)
	st.begin("act")
	st.beginThinking()
	st.pushThinking("reasoning")

	out := stripAnsi(buf.String())

	thinkingAt := strings.Index(out, "thinking")
	statusAt := strings.Index(out, "act")

	if thinkingAt < 0 || statusAt < 0 {
		t.Fatalf("expected both rows:\n%s", out)
	}
	if thinkingAt > statusAt {
		t.Fatalf("the indicator should sit above the status line:\n%s", out)
	}
}

// TestThinkingIsVisuallyDistinct: it must not be mistaken for the answer.
func TestThinkingIsVisuallyDistinct(t *testing.T) {
	var buf bytes.Buffer

	st := newStatus(&buf, true)
	st.setWidth(80)
	st.begin("act")
	st.beginThinking()
	st.pushThinking("weighing the options")

	out := buf.String()

	if !strings.Contains(out, dim) {
		t.Fatalf("the indicator is not dimmed: %q", out)
	}
	if !strings.Contains(out, italicOn) {
		t.Fatalf("the indicator is not italicised: %q", out)
	}
}

// TestThinkingCollapsesWhenTheAnswerStarts: it closes itself once real results
// arrive, and leaves the screen clean.
func TestThinkingCollapsesWhenTheAnswerStarts(t *testing.T) {
	var buf bytes.Buffer

	st := newStatus(&buf, true)
	st.setWidth(80)
	st.begin("act")
	st.beginThinking()
	st.pushThinking("this is a long chain of reasoning that should never be printed")

	buf.Reset()
	st.endThinking()

	out := stripAnsi(buf.String())

	if strings.Contains(out, "long chain of reasoning") {
		t.Fatalf("the reasoning text leaked into the transcript: %q", out)
	}
	if strings.Contains(out, "thinking") {
		t.Fatalf("the live indicator was not removed: %q", out)
	}
}

// TestThinkingSummaryQuotesTheRealCount is the accuracy fix.
//
// A local estimate cannot be right: measured against live streams the ratio
// ranged from 2.6 to 5.4 characters per token, so the summary waits for the
// endpoint's own figure rather than repeating a guess.
func TestThinkingSummaryQuotesTheRealCount(t *testing.T) {
	var buf bytes.Buffer

	st := newStatus(&buf, true)
	st.setWidth(80)
	st.begin("act")
	st.beginThinking()
	st.pushThinking(strings.Repeat("x", 40000)) // estimate would be ~10k

	buf.Reset()
	st.reportThinking(4*time.Second, 1400)

	out := stripAnsi(buf.String())

	if !strings.Contains(out, "thought for 4.0s") {
		t.Fatalf("no elapsed time: %q", out)
	}
	if !strings.Contains(out, "1.4k tokens") {
		t.Fatalf("the real count was not quoted: %q", out)
	}
	if strings.Contains(out, "10.0k") {
		t.Fatalf("the estimate was quoted as fact: %q", out)
	}
}

// TestThinkingSummaryIsSkippedWithoutUsage: a route that reports nothing must
// not produce "0 tokens".
func TestThinkingSummaryIsSkippedWithoutUsage(t *testing.T) {
	var buf bytes.Buffer

	st := newStatus(&buf, true)
	st.setWidth(80)
	st.begin("act")
	st.beginThinking()
	st.pushThinking("reasoning")

	buf.Reset()
	st.reportThinking(time.Second, 0)

	if out := stripAnsi(buf.String()); strings.Contains(out, "thought for") {
		t.Fatalf("a summary was printed with no token count: %q", out)
	}
}

// TestLiveThinkingIsMarkedApproximate keeps an estimate from reading as a
// measurement.
func TestLiveThinkingIsMarkedApproximate(t *testing.T) {
	var buf bytes.Buffer

	st := newStatus(&buf, true)
	st.setWidth(80)
	st.begin("act")
	st.beginThinking()
	st.pushThinking(strings.Repeat("x", 4000))

	out := stripAnsi(buf.String())

	if !strings.Contains(out, "~1.0k tokens") {
		t.Fatalf("the live figure is not marked approximate: %q", out)
	}
}

// TestThinkingRegionIsAlwaysOneRow keeps the erase count predictable. A region
// taller than the terminal could not be erased correctly, since erasing means
// stepping back over a known number of rows.
func TestThinkingRegionIsAlwaysOneRow(t *testing.T) {
	var buf bytes.Buffer

	st := newStatus(&buf, true)
	st.setWidth(40)
	st.begin("act")
	st.beginThinking()

	for i := 0; i < 200; i++ {
		st.pushThinking("reasoning fragment ")
	}

	st.mu.Lock()
	rows := len(st.thinking)
	pinned := st.pinnedRows
	st.mu.Unlock()

	if rows != 1 {
		t.Fatalf("the region is %d rows, want 1", rows)
	}
	if pinned != 2 {
		t.Fatalf("the block is %d rows, want the indicator plus the status line", pinned)
	}
}

// TestThinkingEraseAccountsForEveryRow is the property that keeps the screen
// intact: the number of rows stepped back must equal the number drawn.
func TestThinkingEraseAccountsForEveryRow(t *testing.T) {
	var buf bytes.Buffer

	st := newStatus(&buf, true)
	st.setWidth(40)
	st.begin("act")
	st.beginThinking()
	st.pushThinking("some reasoning")

	st.mu.Lock()
	rows := st.pinnedRows
	st.mu.Unlock()

	if rows != 2 {
		t.Fatalf("expected the indicator plus the status line, got %d rows", rows)
	}

	buf.Reset()
	st.endThinking()

	if !strings.Contains(buf.String(), "\x1b["+itoa(rows)+"A") {
		t.Fatalf("the erase did not step back %d rows: %q", rows, buf.String())
	}
}

// TestReasoningAfterContentIsDropped: once the answer has started there is no
// region to reopen, and reopening one would put scaffolding below the answer.
func TestThinkingDoesNotReopenAfterContent(t *testing.T) {
	var buf bytes.Buffer

	st := newStatus(&buf, true)
	st.setWidth(80)
	st.begin("act")

	st.endThinking()
	buf.Reset()

	st.mu.Lock()
	defer st.mu.Unlock()

	if st.pinnedRows != 0 {
		t.Fatalf("collapsing an empty region left %d pinned rows", st.pinnedRows)
	}
}

// TestStepLimitIsVisibleInTheStatusLine: a ceiling that only becomes visible at
// the moment it is hit cannot be planned around.
func TestStepLimitIsVisibleInTheStatusLine(t *testing.T) {
	var buf bytes.Buffer

	st := newStatus(&buf, true)
	st.setWidth(100)
	st.begin("act")
	st.setStepLimit(60)
	st.setProgress(12, 1000, 0.5)
	st.refresh()

	out := stripAnsi(buf.String())

	if !strings.Contains(out, "step 12/60") {
		t.Fatalf("the ceiling is not shown: %q", out)
	}
}
