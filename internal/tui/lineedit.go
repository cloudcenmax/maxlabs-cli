package tui

import (
	"bufio"
	"errors"
	"io"
	"os"
	"strings"
	"sync/atomic"
)

// The line editor.
//
// It exists because two of the things a person expects from a shell - recalling
// what they typed, and switching mode without leaving the keyboard - cannot be
// expressed in a line of input. Both need the terminal handing over individual
// keystrokes, and once that is happening, letting Backspace work is the least a
// program can do.
//
// The editor never redraws while a turn is running. The pinned status line owns
// the bottom of the screen, and two writers fighting over the same row produce
// garbage; while busy, keystrokes are collected without echo.

// key is a decoded keystroke.
type key int

const (
	keyRune key = iota
	keyEnter
	keyBackspace
	keyDelete
	keyUp
	keyDown
	keyLeft
	keyRight
	keyHome
	keyEnd
	keyTab
	keyShiftTab
	keyCtrlC
	keyCtrlD
	keyCtrlL
	keyCtrlU
	keyCtrlA
	keyCtrlE
	keyUnknown
)

// event is one decoded keystroke, with its character when it has one.
type event struct {
	key  key
	rune rune
}

// lineEditor reads one line at a time from a terminal.
type lineEditor struct {
	reader *bufio.Reader
	out    io.Writer
	file   *os.File

	// quiet suppresses redrawing. It is set while a turn runs.
	quiet *atomic.Bool

	// prompt supplies the text to show before the input.
	prompt func() string

	// onInput receives the in-progress line when the editor may not draw it, so
	// the status line can show it instead.
	onInput func(string)

	// renderSubmitted replaces the prompt row with the final rendering of a
	// submitted line, newlines included. Returning it here rather than writing a
	// bare newline is what lets the shell restyle the row it already drew.
	renderSubmitted func(string) string

	// styled enables colour in the prompt itself.
	styled bool

	// history is everything submitted, oldest first.
	history []string

	// histPos is the position while browsing; len(history) means "the live line".
	histPos int

	// pending is the line being edited.
	pending []rune
	cursor  int

	// draft preserves a half-typed line while the user browses history, so
	// pressing Down back to the live line does not discard what they had.
	draft string

	// width bounds the drawn row. The redraw clears one row, so an input line
	// that wrapped would leave the wrapped remainder on screen.
	width int
}

func newLineEditor(reader *bufio.Reader, out io.Writer, file *os.File, quiet *atomic.Bool, prompt func() string, styled bool) *lineEditor {
	return &lineEditor{
		reader: reader,
		out:    out,
		file:   file,
		quiet:  quiet,
		prompt: prompt,
		styled: styled,
	}
}

// readLine blocks until the user submits a line or presses a key the shell owns.
//
// The action is keyEnter for a submitted line, or the control key that was
// pressed. Returning the key as data rather than encoding it in the string keeps
// the meaning of Shift+Tab with the shell, where it belongs, and avoids inventing
// sentinel strings that a user could type.
func (e *lineEditor) readLine() (string, key, bool, error) {
	e.pending = nil
	e.cursor = 0
	e.histPos = len(e.history)

	// No paint here. The shell draws the idle prompt, and the editor takes the
	// row over on the first keystroke: two owners would print it twice, and the
	// editor cannot know when a turn has ended because it is blocked on a read.
	for {
		ev, err := e.readKey()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return "", keyUnknown, false, nil
			}

			return "", keyUnknown, false, err
		}

		switch ev.key {
		case keyEnter:
			line := string(e.pending)
			e.remember(line)

			e.paintSubmitted(line)

			return line, keyEnter, true, nil

		case keyCtrlD:
			if len(e.pending) == 0 {
				return "", keyUnknown, false, nil
			}

		case keyBackspace:
			if e.cursor > 0 {
				e.pending = append(e.pending[:e.cursor-1], e.pending[e.cursor:]...)
				e.cursor--
			}

		case keyDelete:
			if e.cursor < len(e.pending) {
				e.pending = append(e.pending[:e.cursor], e.pending[e.cursor+1:]...)
			}

		case keyLeft:
			if e.cursor > 0 {
				e.cursor--
			}

		case keyRight:
			if e.cursor < len(e.pending) {
				e.cursor++
			}

		case keyHome, keyCtrlA:
			e.cursor = 0

		case keyEnd, keyCtrlE:
			e.cursor = len(e.pending)

		case keyCtrlU:
			e.pending = e.pending[:0]
			e.cursor = 0

		case keyUp:
			e.recall(-1)

		case keyDown:
			e.recall(1)

		case keyRune:
			e.pending = append(e.pending[:e.cursor], append([]rune{ev.rune}, e.pending[e.cursor:]...)...)
			e.cursor++

		case keyCtrlC, keyTab, keyShiftTab, keyCtrlL:
			// The shell owns what these mean, so hand the key back untouched.
			return "", ev.key, true, nil

		default:
			continue
		}

		e.redraw()
	}
}

// readKey decodes one keystroke.
func (e *lineEditor) readKey() (event, error) {
	b, err := e.reader.ReadByte()
	if err != nil {
		return event{}, err
	}

	switch b {
	case '\r', '\n':
		return event{key: keyEnter}, nil

	case 0x7f, 0x08:
		return event{key: keyBackspace}, nil

	case 0x03:
		return event{key: keyCtrlC}, nil

	case 0x04:
		return event{key: keyCtrlD}, nil

	case 0x01:
		return event{key: keyCtrlA}, nil

	case 0x05:
		return event{key: keyCtrlE}, nil

	case 0x0c:
		return event{key: keyCtrlL}, nil

	case 0x15:
		return event{key: keyCtrlU}, nil

	case '\t':
		return event{key: keyTab}, nil

	case 0x1b:
		return e.readEscape()
	}

	if b < 0x20 {
		return event{key: keyUnknown}, nil
	}

	// Multi-byte UTF-8: read the continuation bytes the lead byte promises.
	if b < 0x80 {
		return event{key: keyRune, rune: rune(b)}, nil
	}

	return event{key: keyRune, rune: e.readContinuation(b)}, nil
}

// readEscape decodes an escape sequence.
//
// Arrow keys and Shift+Tab arrive as ESC followed by '[' or 'O' and a final
// byte. A lone ESC (the user pressed Escape) is reported as unknown rather than
// blocking for a byte that will never come.
func (e *lineEditor) readEscape() (event, error) {
	b, err := e.reader.ReadByte()
	if err != nil {
		return event{key: keyUnknown}, nil
	}

	if b != '[' && b != 'O' {
		return event{key: keyUnknown}, nil
	}

	final, err := e.reader.ReadByte()
	if err != nil {
		return event{key: keyUnknown}, nil
	}

	switch final {
	case 'A':
		return event{key: keyUp}, nil
	case 'B':
		return event{key: keyDown}, nil
	case 'C':
		return event{key: keyRight}, nil
	case 'D':
		return event{key: keyLeft}, nil
	case 'H':
		return event{key: keyHome}, nil
	case 'F':
		return event{key: keyEnd}, nil
	case 'Z':
		return event{key: keyShiftTab}, nil
	}

	// Extended sequences such as ESC [ 3 ~ (delete) carry a trailing byte.
	if final >= '0' && final <= '9' {
		if _, err := e.reader.ReadByte(); err != nil {
			return event{key: keyUnknown}, nil
		}

		if final == '3' {
			return event{key: keyDelete}, nil
		}
	}

	return event{key: keyUnknown}, nil
}

// readContinuation assembles a multi-byte rune.
func (e *lineEditor) readContinuation(lead byte) rune {
	var size int
	switch {
	case lead&0xE0 == 0xC0:
		size = 2
	case lead&0xF0 == 0xE0:
		size = 3
	case lead&0xF8 == 0xF0:
		size = 4
	default:
		return rune(lead)
	}

	buf := []byte{lead}
	for i := 1; i < size; i++ {
		b, err := e.reader.ReadByte()
		if err != nil {
			break
		}
		buf = append(buf, b)
	}

	return []rune(string(buf))[0]
}

// recall moves through history. Direction -1 is older, +1 is newer.
func (e *lineEditor) recall(direction int) {
	if len(e.history) == 0 {
		return
	}

	next := e.histPos + direction
	if next < 0 {
		next = 0
	}
	if next > len(e.history) {
		next = len(e.history)
	}

	e.histPos = next

	if next == len(e.history) {
		// Back to the line being typed, which is remembered rather than lost.
		e.pending = []rune(e.draft)
	} else {
		if e.histPos == len(e.history)-1 && direction < 0 {
			e.draft = string(e.pending)
		}
		e.pending = []rune(e.history[next])
	}

	e.cursor = len(e.pending)
}

// remember appends a submitted line to history.
func (e *lineEditor) remember(line string) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return
	}

	// Consecutive duplicates are dropped: recalling the same line twice is
	// never what someone wants.
	if len(e.history) > 0 && e.history[len(e.history)-1] == line {
		return
	}

	e.history = append(e.history, line)
}

// shouldDraw reports whether the editor may paint the input row.
//
// Two conditions: a terminal it can put into raw mode, and no turn in flight.
// While a turn runs the pinned status line owns the bottom of the screen, and
// two writers on one row produce garbage.
func (e *lineEditor) shouldDraw() bool {
	return !e.quiet.Load() && e.file != nil
}

// paintSubmitted replaces the row the user typed on with the final rendering.
//
// The row is erased and rewritten rather than left as it was, because the prompt
// row shows the input marker and a plain newline would leave the user's text
// looking exactly like the model's answer.
func (e *lineEditor) paintSubmitted(line string) {
	if !e.shouldDraw() {
		return
	}

	io.WriteString(e.out, "\r"+clearLine+e.submittedRendering(line))
}

// submittedRendering is what replaces the prompt row, newlines included.
//
// Split out from the writing so the choice can be tested without a terminal.
func (e *lineEditor) submittedRendering(line string) string {
	if e.renderSubmitted != nil {
		return e.renderSubmitted(line)
	}

	return e.prompt() + line + "\n"
}

// redraw repaints the input line.
//
// When the editor may not draw - during a turn, where the status line owns the
// bottom row - the line is handed to the status line instead of being dropped.
// The terminal is in raw mode, so anything not drawn here is not drawn at all.
func (e *lineEditor) redraw() {
	if !e.shouldDraw() {
		if e.onInput != nil {
			if len(e.pending) == 0 {
				e.onInput("")
			} else {
				e.onInput(e.prompt() + string(e.pending))
			}
		}

		return
	}

	prompt, shown, cursor := e.viewport()

	// \r returns to the start of the row and the clear wipes whatever the
	// previous frame left there.
	io.WriteString(e.out, "\r"+clearLine+prompt+shown)

	// Park the cursor where the user is actually editing, counting in runes
	// because a wide character occupies one cell per rune at worst.
	if back := len([]rune(shown)) - cursor; back > 0 {
		io.WriteString(e.out, "\x1b["+itoa(back)+"D")
	}
}

// viewport returns the prompt, the visible part of the line, and where the
// cursor sits within that visible part.
//
// A line longer than the terminal scrolls horizontally rather than wrapping. The
// redraw clears a single row, so a wrapped input line would leave its overflow
// behind on every keystroke; scrolling keeps the cursor visible and the clearing
// honest.
func (e *lineEditor) viewport() (string, string, int) {
	prompt := e.prompt()
	runes := []rune(e.pending)

	if e.width <= 0 {
		return prompt, string(runes), e.cursor
	}

	available := e.width - 1 - visibleWidth(prompt)
	if available < 8 {
		// Never shrink the editing area to nothing on a narrow terminal.
		available = 8
	}

	if len(runes) <= available {
		return prompt, string(runes), e.cursor
	}

	// Keep the cursor in view: window the line so it ends at or after the
	// cursor's position.
	start := 0
	if e.cursor > available {
		start = e.cursor - available
	}

	end := start + available
	if end > len(runes) {
		end = len(runes)
		start = end - available
		if start < 0 {
			start = 0
		}
	}

	return prompt, string(runes[start:end]), e.cursor - start
}

// itoa avoids pulling in strconv for one small conversion.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}

	var buf [20]byte
	i := len(buf)

	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}

	return string(buf[i:])
}
