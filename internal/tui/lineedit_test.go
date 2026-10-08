package tui

import (
	"bufio"
	"bytes"
	"strings"
	"sync/atomic"
	"testing"
)

// newTestEditor builds an editor over a scripted keystroke stream.
//
// file is nil, which suppresses drawing: these tests are about decoding and
// line state, not about escape sequences reaching a screen.
func newTestEditor(input string, quiet *atomic.Bool) (*lineEditor, *bytes.Buffer) {
	out := &bytes.Buffer{}
	quietFlag := quiet
	if quietFlag == nil {
		quietFlag = &atomic.Bool{}
	}

	return newLineEditor(bufio.NewReader(strings.NewReader(input)), out, nil, quietFlag, func() string { return "> " }, true), out
}

// TestEditorSubmitsALine is the baseline: typing still produces what was typed.
func TestEditorSubmitsALine(t *testing.T) {
	e, _ := newTestEditor("hello there\n", nil)

	line, action, ok, err := e.readLine()
	if err != nil || !ok {
		t.Fatalf("readLine: ok=%v err=%v", ok, err)
	}
	if action != keyEnter {
		t.Fatalf("action = %v, want keyEnter", action)
	}
	if line != "hello there" {
		t.Fatalf("line = %q", line)
	}
}

// TestEditorBackspaceEdits covers the key that only exists once the terminal
// stops echoing for you.
func TestEditorBackspaceEdits(t *testing.T) {
	// "helo" then backspace then "lo" -> "hello"
	e, _ := newTestEditor("helo\x7flo\n", nil)

	line, _, _, err := e.readLine()
	if err != nil {
		t.Fatalf("readLine: %v", err)
	}
	if line != "hello" {
		t.Fatalf("line = %q, want hello", line)
	}
}

// TestEditorCursorMovement covers left/right editing, which a line editor is
// expected to have once it is reading raw keys anyway.
func TestEditorCursorMovement(t *testing.T) {
	// Type "helo", step left over the "o", insert "l" -> "hello"
	e, _ := newTestEditor("helo\x1b[Dl\n", nil)

	line, _, _, err := e.readLine()
	if err != nil {
		t.Fatalf("readLine: %v", err)
	}
	if line != "hello" {
		t.Fatalf("line = %q, want hello", line)
	}
}

// TestEditorShiftTabIsReportedAsAnAction is the requested feature: switching
// mode without leaving the keyboard, from the middle of a prompt.
func TestEditorShiftTabIsReportedAsAnAction(t *testing.T) {
	e, _ := newTestEditor("\x1b[Z", nil)

	line, action, ok, err := e.readLine()
	if err != nil || !ok {
		t.Fatalf("readLine: ok=%v err=%v", ok, err)
	}
	if action != keyShiftTab {
		t.Fatalf("action = %v, want keyShiftTab", action)
	}
	if line != "" {
		t.Fatalf("a mode switch must not submit text, got %q", line)
	}
}

// TestEditorTabAlsoToggles is the courtesy alias, for terminals that eat
// Shift+Tab.
func TestEditorTabAlsoToggles(t *testing.T) {
	e, _ := newTestEditor("\t", nil)

	_, action, _, err := e.readLine()
	if err != nil {
		t.Fatalf("readLine: %v", err)
	}
	if action != keyTab {
		t.Fatalf("action = %v, want keyTab", action)
	}
}

// TestEditorHistoryRecallsPreviousPrompts covers up/down.
func TestEditorHistoryRecallsPreviousPrompts(t *testing.T) {
	e, _ := newTestEditor("first\nsecond\n\x1b[A\x1b[A\n", nil)

	var lines []string
	for i := 0; i < 3; i++ {
		line, action, ok, err := e.readLine()
		if err != nil || !ok {
			t.Fatalf("readLine %d: ok=%v err=%v", i, ok, err)
		}
		if action != keyEnter {
			t.Fatalf("readLine %d: action = %v", i, action)
		}
		lines = append(lines, line)
	}

	want := []string{"first", "second", "first"}
	for i := range want {
		if lines[i] != want[i] {
			t.Fatalf("history[%d] = %q, want %q (all: %v)", i, lines[i], want[i], lines)
		}
	}
}

// TestEditorHistoryDownReturnsToTheDraft: browsing away from a half-typed line
// and back must not discard it.
func TestEditorHistoryDownReturnsToTheDraft(t *testing.T) {
	// Submit one line, then type a draft and browse up and back down.
	e, _ := newTestEditor("earlier\nhalf typed\x1b[A\x1b[B\n", nil)

	first, _, _, _ := e.readLine()
	if first != "earlier" {
		t.Fatalf("first = %q", first)
	}

	second, _, _, err := e.readLine()
	if err != nil {
		t.Fatalf("readLine: %v", err)
	}
	if second != "half typed" {
		t.Fatalf("the draft was lost: %q", second)
	}
}

// TestEditorHistorySkipsEmptyAndRepeatedLines keeps the recall list useful.
func TestEditorHistorySkipsEmptyAndRepeatedLines(t *testing.T) {
	e, _ := newTestEditor("same\nsame\n\nother\n", nil)

	for i := 0; i < 4; i++ {
		if _, _, ok, _ := e.readLine(); !ok {
			t.Fatalf("readLine %d ended early", i)
		}
	}

	if len(e.history) != 2 {
		t.Fatalf("history = %v, want two entries", e.history)
	}
}

// TestEditorCtrlDOnAnEmptyLineIsEOF, and on a full line it is not.
func TestEditorCtrlDOnAnEmptyLineIsEOF(t *testing.T) {
	e, _ := newTestEditor("\x04", nil)

	if _, _, ok, err := e.readLine(); ok || err != nil {
		t.Fatalf("ctrl+d on an empty line should end input: ok=%v err=%v", ok, err)
	}
}

// TestEditorCtrlU Clears the line, another standard editor key.
func TestEditorCtrlUClearsTheLine(t *testing.T) {
	e, _ := newTestEditor("wrong\x15right\n", nil)

	line, _, _, err := e.readLine()
	if err != nil {
		t.Fatalf("readLine: %v", err)
	}
	if line != "right" {
		t.Fatalf("line = %q, want right", line)
	}
}

// TestEditorDoesNotDrawWhileBusy: the pinned status line owns the bottom of the
// screen during a turn, and two writers on one row produce garbage.
func TestEditorDoesNotDrawWhileBusy(t *testing.T) {
	quiet := &atomic.Bool{}

	e, _ := newTestEditor("x", quiet)

	quiet.Store(true)
	if e.shouldDraw() {
		t.Fatal("the editor would draw while a turn is running")
	}

	quiet.Store(false)
	// Still false: there is no terminal file in this test. The distinction is
	// the point - no raw-mode file means no editing UI at all.
	if e.shouldDraw() {
		t.Fatal("the editor would draw without a terminal file")
	}
}

// TestEditorHandlesUTF8: a path with a non-ASCII character must survive, which
// byte-wise editing would mangle.
func TestEditorHandlesUTF8(t *testing.T) {
	e, _ := newTestEditor("café/日本\n", nil)

	line, _, _, err := e.readLine()
	if err != nil {
		t.Fatalf("readLine: %v", err)
	}
	if line != "café/日本" {
		t.Fatalf("line = %q", line)
	}
}

// TestEditorUnknownSequenceIsIgnored keeps a stray escape from submitting input.
func TestEditorUnknownSequenceIsIgnored(t *testing.T) {
	e, _ := newTestEditor("ok\x1b[9~\n", nil)

	line, _, _, err := e.readLine()
	if err != nil {
		t.Fatalf("readLine: %v", err)
	}
	if line != "ok" {
		t.Fatalf("line = %q", line)
	}
}

// TestEditorHistoryDownStepsForward covers the other direction on its own, so a
// failure points at one behaviour rather than two.
func TestEditorHistoryDownStepsForward(t *testing.T) {
	// Two submissions, then Up Up Down -> the newer entry.
	e, _ := newTestEditor("first\nsecond\n\x1b[A\x1b[A\x1b[B\n", nil)

	var lines []string
	for i := 0; i < 3; i++ {
		line, _, _, err := e.readLine()
		if err != nil {
			t.Fatalf("readLine %d: %v", i, err)
		}
		lines = append(lines, line)
	}

	if lines[2] != "second" {
		t.Fatalf("Down did not step forward: %v", lines)
	}
}

// TestEditorReportsInputWhenItCannotDraw is the fix for typing blind.
//
// Raw mode disables the terminal's own echo, and the editor suppresses its
// drawing during a turn. Without a third path the characters go nowhere and the
// user cannot see what they are typing.
func TestEditorReportsInputWhenItCannotDraw(t *testing.T) {
	quiet := &atomic.Bool{}
	quiet.Store(true)

	out := &bytes.Buffer{}
	e := newLineEditor(bufio.NewReader(strings.NewReader("stop")), out, nil, quiet, func() string { return "> " }, true)

	var seen []string
	e.onInput = func(text string) { seen = append(seen, text) }

	// Drive the keys directly: readLine would block waiting for Enter.
	for i := 0; i < 4; i++ {
		ev, err := e.readKey()
		if err != nil {
			t.Fatalf("readKey %d: %v", i, err)
		}
		e.pending = append(e.pending, ev.rune)
		e.redraw()
	}

	if len(seen) == 0 {
		t.Fatal("the editor reported nothing while suppressed: typing is invisible")
	}

	// The last report carries the whole line, which is what the status shows.
	if last := seen[len(seen)-1]; last != "> stop" {
		t.Fatalf("last report = %q, want %q", last, "> stop")
	}
	if out.Len() != 0 {
		t.Fatalf("the editor drew despite being suppressed: %q", out.String())
	}
}

// TestEditorClearsReportedInput: when the line is emptied, the status line must
// stop showing a stale prompt.
func TestEditorClearsReportedInput(t *testing.T) {
	quiet := &atomic.Bool{}
	quiet.Store(true)

	e := newLineEditor(bufio.NewReader(strings.NewReader("x")), &bytes.Buffer{}, nil, quiet, func() string { return "> " }, true)

	var last string
	e.onInput = func(text string) { last = text }

	e.pending = []rune("x")
	e.redraw()
	if last == "" {
		t.Fatal("input was not reported")
	}

	e.pending = nil
	e.redraw()
	if last != "" {
		t.Fatalf("the status line would keep showing %q", last)
	}
}

// TestEditorViewportNeverExceedsTheWidth: the redraw clears one row, so a
// wrapped input line would leave its overflow on screen.
func TestEditorViewportNeverExceedsTheWidth(t *testing.T) {
	for _, width := range []int{20, 40, 80} {
		e, _ := newTestEditor("", nil)
		e.width = width
		e.prompt = func() string { return "> " }
		e.pending = []rune(strings.Repeat("a long typed line ", 20))
		e.cursor = len(e.pending)

		prompt, shown, cursor := e.viewport()

		if total := visibleWidth(prompt) + visibleWidth(shown); total > width-1 {
			t.Errorf("width %d: the row would be %d columns", width, total)
		}
		if cursor < 0 || cursor > len([]rune(shown)) {
			t.Errorf("width %d: cursor %d is outside the visible line", width, cursor)
		}
		if cursor != len([]rune(shown)) {
			t.Errorf("width %d: the cursor should sit at the end while typing", width)
		}
	}
}

// TestEditorViewportKeepsTheCursorVisibleWhenEditingMidLine: scrolling to the
// cursor is the point; a window that hides it is worse than wrapping.
func TestEditorViewportKeepsTheCursorVisibleWhenEditingMidLine(t *testing.T) {
	e, _ := newTestEditor("", nil)
	e.width = 30
	e.prompt = func() string { return "> " }
	e.pending = []rune(strings.Repeat("x", 100))
	e.cursor = 0

	_, shown, cursor := e.viewport()

	if cursor >= len([]rune(shown))+1 {
		t.Fatalf("the cursor is outside the window: %d of %d", cursor, len([]rune(shown)))
	}
	if cursor < 0 {
		t.Fatalf("negative cursor %d", cursor)
	}
}

// TestEditorViewportLeavesShortLinesAlone keeps the common case unchanged.
func TestEditorViewportLeavesShortLinesAlone(t *testing.T) {
	e, _ := newTestEditor("", nil)
	e.width = 80
	e.prompt = func() string { return "> " }
	e.pending = []rune("short")
	e.cursor = 3

	_, shown, cursor := e.viewport()

	if shown != "short" || cursor != 3 {
		t.Fatalf("viewport = %q, cursor %d", shown, cursor)
	}
}
