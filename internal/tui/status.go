package tui

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// brandPink is #ff66b2, --color-primary-500 in resources/css/app.css.
//
// Emitted as a truecolor escape where the terminal advertises support, and as
// the nearest xterm-256 index otherwise. Both are checked rather than assumed:
// emitting truecolor at a terminal that cannot render it produces literal
// escape garbage in the middle of someone's session.
const (
	pinkTruecolor = "\x1b[38;2;255;102;178m"
	pink256       = "\x1b[38;5;205m"
	reset         = "\x1b[0m"
	dim           = "\x1b[2m"

	// defaultColumns is the width assumed when the terminal will not report one.
	defaultColumns = 80

	// clearLine and upOne are the whole of the positioning this shell needs.
	upOne     = "\x1b[1A"
	clearLine = "\r\x1b[K"
)

// spinnerFrames is a braille cycle: dense enough to look continuous at the
// refresh rate below without being noisy.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// refreshInterval is how often the status line is redrawn and buffered output is
// flushed.
//
// Deltas are buffered rather than written per token: redrawing a status line on
// every token of a fast stream is what makes a terminal flicker. Twelve times a
// second reads as continuous to a person.
const refreshInterval = 80 * time.Millisecond

// status owns the bottom line of the terminal.
//
// The invariant is: after a render, the status occupies the last line and the
// cursor sits on the empty line below it. Writing output means moving up one
// line, clearing the status, printing, and rendering again - which keeps the
// status pinned at the bottom while the transcript grows above it.
//
// When Ansi is false every escape is suppressed, so a piped or redirected run
// produces clean text with no control characters in it.
type status struct {
	mu   sync.Mutex
	out  io.Writer
	ansi bool
	// pinnedRows is how many rows the shell is holding at the bottom of the
	// screen: the thinking region plus the status line.
	//
	// It is a count rather than a flag because a thinking region sits above the
	// status line while the model reasons, and the cursor has to be moved back
	// over the whole block. Moving back one row, as a single-row pin did, would
	// land in the middle of it.
	pinnedRows int

	// thinking is the region shown while the model reasons. It is a single row:
	// the reasoning itself is not displayed, only how much of it there has been.
	thinking []string

	// thinkingRunes counts characters of reasoning seen, which is the basis for
	// the token estimate.
	thinkingRunes int

	// thinkingFrom records when the model began reasoning, for the collapsed
	// summary.
	thinkingFrom time.Time

	mode      string
	planMode  bool
	stepLimit int
	spins     int

	// width is the terminal's column count, or 0 when unknown. It bounds the
	// status line: a pinned row that wraps stops being pinned.
	width int

	// thinkingLevel is the reasoning effort label, distinct from the thinking
	// region above: one is a setting, the other is content.
	thinkingLevel string

	// typed is the line being entered while a turn runs. It is shown on the
	// status line because that row is already owned by the status; opening a
	// second row for it would mean two writers on the same screen area.
	typed string
	steps int
	input int
	hit   float64
	busy  bool

	pending strings.Builder

	// md renders model output for a terminal. It is stateful across calls
	// because a markdown construct can span deltas.
	md markdown

	stop chan struct{}
	done chan struct{}
}

func newStatus(out io.Writer, ansi bool) *status {
	return &status{out: out, ansi: ansi, hit: -1, md: markdown{styled: ansi}}
}

// setWidth records the terminal width and passes it to the renderer, which wraps
// streamed text to it.
func (s *status) setWidth(columns int) {
	if columns <= 0 {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.width = columns
	s.md.width = columns
}

// Width reports the current column count, or 0 when unknown.
func (s *status) Width() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.width
}

// start begins the refresh loop.
func (s *status) start() {
	s.mu.Lock()
	if s.stop != nil {
		s.mu.Unlock()
		return
	}
	s.stop = make(chan struct{})
	s.done = make(chan struct{})
	stop, done := s.stop, s.done
	s.mu.Unlock()

	go func() {
		defer close(done)

		ticker := time.NewTicker(refreshInterval)
		defer ticker.Stop()

		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				s.refresh()
			}
		}
	}()
}

// stopLoop halts the refresh loop and flushes anything buffered.
func (s *status) stopLoop() {
	s.mu.Lock()
	stop, done := s.stop, s.done
	s.stop, s.done = nil, nil
	s.mu.Unlock()

	if stop == nil {
		return
	}

	close(stop)
	<-done

	s.mu.Lock()
	s.flushLocked()
	s.clearLocked()
	s.mu.Unlock()
}

// refresh redraws and flushes.
func (s *status) refresh() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.spins++
	s.flushLocked()

	if !s.busy {
		return
	}
	s.renderLocked()
}

// flushLocked writes any buffered streamed text above the status line.
func (s *status) flushLocked() {
	if s.pending.Len() == 0 {
		return
	}

	text := s.pending.String()
	s.pending.Reset()

	s.eraseBlockLocked()

	fmt.Fprint(s.out, text)
	if !strings.HasSuffix(text, "\n") {
		fmt.Fprint(s.out, "\n")
	}

	if s.busy {
		s.renderLocked()
	}
}

// renderLocked draws the thinking region and the status line as one block.
func (s *status) renderLocked() {
	if !s.ansi {
		return
	}

	if s.pinnedRows > 0 {
		fmt.Fprint(s.out, "\x1b["+itoa(s.pinnedRows)+"A")
	}

	rows := 0

	for _, line := range s.thinking {
		// The line styles itself. Wrapping it here as well emitted the dim and
		// italic escapes twice - invisible, but a sign the two layers disagreed
		// about who owned the appearance.
		fmt.Fprint(s.out, clearLine, s.fitLocked(line), "\n")
		rows++
	}

	fmt.Fprint(s.out, clearLine, s.fitLocked(s.lineLocked()), "\n")
	rows++

	s.pinnedRows = rows
}

// eraseBlockLocked clears the pinned block and leaves the cursor at its first
// row, ready for the transcript to continue there.
//
// It erases to the end of the screen rather than clearing row by row: the block
// is the last thing on screen by construction, so there is nothing below it to
// damage, and one sequence is less to get wrong.
func (s *status) eraseBlockLocked() {
	if s.pinnedRows == 0 {
		return
	}

	fmt.Fprint(s.out, "\x1b["+itoa(s.pinnedRows)+"A\x1b[J")
	s.pinnedRows = 0
}

// clearLocked removes the pinned block entirely.
func (s *status) clearLocked() {
	if !s.ansi {
		return
	}

	s.eraseBlockLocked()
}

// beginThinking starts the reasoning region.
func (s *status) beginThinking() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.thinkingFrom.IsZero() {
		s.thinkingFrom = time.Now()
	}
}

// pushThinking records reasoning arriving from the model.
//
// The reasoning text is deliberately NOT displayed. A model's chain of thought
// is long, changes constantly, and scrolls the answer off the screen - what a
// person actually wants to know while waiting is how much longer, and a rising
// token count answers that without competing for attention with the answer.
func (s *status) pushThinking(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.thinkingRunes += len([]rune(text))
	s.thinking = []string{s.thinkingLineLocked()}

	if s.busy {
		s.renderLocked()
	}
}

// thinkingLineLocked is the live reasoning indicator.
//
// The figure is prefixed with "~" and is honestly approximate. Measured against
// live streams, reasoning ran between 2.6 and 5.4 characters per token, so no
// local constant is right and a precise-looking number would be a lie. It exists
// to show progress; reportThinking gives the real figure once the endpoint
// reports it.
func (s *status) thinkingLineLocked() string {
	estimate := s.thinkingRunes / 4

	return dim + italicOn + "✻ thinking" + reset + dim + " ~" +
		humanTokens(estimate) + " tokens" + reset
}

// endThinking collapses the region and reports how long it took.
//
// The reasoning is erased rather than left on screen: it is scaffolding for the
// answer, and it competes for attention with the thing the user actually asked
// for. One dim line records that it happened.
func (s *status) endThinking() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.thinkingFrom.IsZero() && len(s.thinking) == 0 {
		return
	}

	s.eraseBlockLocked()
	s.thinking = nil
	s.thinkingRunes = 0

	// No summary is printed here. The accurate token count only exists in the
	// usage block at the end of the stream, so the record of what the thinking
	// cost is written then - see reportThinking. Printing an estimate now and a
	// different number later would look like a bug.
}

// fitLocked truncates the status line so it cannot wrap.
//
// One column is kept spare. A row exactly as wide as the terminal leaves the
// cursor in the wrap-pending state, and some terminals then scroll on the next
// character - which moves the whole transcript and breaks the pin.
func (s *status) fitLocked(line string) string {
	if s.width <= 0 {
		return line
	}

	return truncateVisible(line, s.width-1)
}

// lineLocked builds the status text.
//
// The mode sits at the END, where it is read last and remembered, rather than
// buried among counters. Plan mode is the one state a user can be in without
// meaning to be, so it is stated rather than abbreviated and it says how to
// leave - a mode with no visible exit is a trap.
func (s *status) lineLocked() string {
	spin := pink() + spinnerFrames[s.spins%len(spinnerFrames)] + reset

	parts := []string{spin}

	// Near the front, so a narrow terminal truncates the counters rather than
	// the setting that explains why the model is slow.
	if s.thinkingLevel != "" {
		parts = append(parts, "think:"+s.thinkingLevel)
	}
	if s.steps > 0 {
		// The ceiling is shown alongside the count. A limit that only becomes
		// visible at the moment it is hit cannot be planned around.
		if s.stepLimit > 0 {
			parts = append(parts, fmt.Sprintf("step %d/%d", s.steps, s.stepLimit))
		} else {
			parts = append(parts, fmt.Sprintf("step %d", s.steps))
		}
	}
	if s.hit >= 0 {
		parts = append(parts, fmt.Sprintf("cache %.0f%%", s.hit*100))
	}
	if s.input > 0 {
		parts = append(parts, humanTokens(s.input)+" in")
	}

	sep := dim + " · " + reset
	line := strings.Join(parts, sep)

	if s.typed != "" {
		line += sep + boldOn + s.typed + reset
	}

	if s.planMode {
		// Loud, and it says how to get out.
		if s.ansi {
			return line + sep + boldOn + pinkTruecolor + "PLAN MODE" + reset +
				dim + " · shift+tab to exit" + reset
		}

		return line + sep + "PLAN MODE (shift+tab to exit)"
	}

	if s.ansi {
		return line + sep + s.mode + dim + " · shift+tab · ctrl+c" + reset
	}

	return line + sep + s.mode
}

// write buffers streamed model text.
func (s *status) write(text string) {
	if text == "" {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Model output goes through the markdown renderer; shell messages do not.
	// Only complete lines are rendered, so a construct arriving across two
	// deltas is never printed half-formed.
	s.pending.WriteString(s.md.feed(text))

	// A newline is a natural boundary: flush it straight away rather than making
	// a paragraph wait up to a refresh interval.
	if strings.Contains(text, "\n") {
		s.flushLocked()
	}
}

// writeRaw buffers shell output without markdown rendering.
func (s *status) writeRaw(text string) {
	if text == "" {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.pending.WriteString(text)
}

// println writes a line of shell output, keeping it above the status.
//
// Buffered model output is drained first. Without that, anything still held -
// a wrapped line, the renderer's unterminated tail - is written after this
// message instead of before it, and a single reply looks like two halves with a
// summary stuck between them.
func (s *status) println(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.printlnLocked(text)
}

// printlnLocked is println for a caller that already holds the lock.
func (s *status) printlnLocked(text string) {
	s.flushLocked()

	s.eraseBlockLocked()

	fmt.Fprint(s.out, text, "\n")

	if s.busy {
		s.renderLocked()
	}
}

// printf writes formatted shell output.
func (s *status) printf(format string, args ...any) {
	s.println(fmt.Sprintf(format, args...))
}

// begin marks the shell busy and sets the mode label.
func (s *status) begin(mode string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.busy = true
	s.mode = mode
	s.steps = 0
}

// The submitted-prompt band.
//
// Both the foreground and the background are set explicitly, which is the whole
// trick: contrast stops being a property of whatever theme the user runs and
// becomes a property of this code. Left to inherit, a band is only as readable
// as the terminal's own foreground happens to be against it.
//
// The background is the brand pink, --color-primary-500 in
// resources/css/app.css, with black on top. That pairing measures 7.8:1, which
// clears WCAG AAA for body text - and it is the TEXT contrast that matters most,
// because that is what is actually read.
//
// The pink is a saturated mid-tone rather than a dark or light band, which is
// what lets one colour serve both themes: it measures 7.8:1 against a black
// terminal and 2.7:1 against a white one. So it pops hard on dark and reads as a
// clear coloured block on light. Text on it is always 7.8:1 either way, because
// both fg and bg are stated rather than inherited.
const (
	bandTruecolor = "\x1b[48;2;255;102;178m\x1b[38;2;0;0;0m"
	band256       = "\x1b[48;5;205m\x1b[38;5;16m"
)

// promptBandOn reports whether submitted prompts are drawn as a band.
//
// On by default: telling what you asked apart from what the model answered is
// the point, not a preference. It can be turned off with HARNESS_PROMPT_BAND=0
// for anyone who prefers a quiet marker.
func promptBandOn() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("HARNESS_PROMPT_BAND"))) {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

// echoPrompt renders a submitted line, so what the user asked is distinguishable
// from what the model answered.
func (s *status) echoPrompt(text string) string {
	if !s.ansi {
		return "> " + text
	}

	if promptBandOn() {
		band := band256
		if truecolor() {
			band = bandTruecolor
		}

		// A space either side keeps the text off the colour's edge.
		return band + " ❯ " + text + " " + reset
	}

	return boldOn + pink() + "❯" + reset + " " + boldOn + text + boldOff
}

// setInput records what the user is typing mid-turn and repaints.
//
// Raw mode means the terminal does not echo, so without this a prompt typed
// while the agent works is invisible until it is submitted.
func (s *status) setInput(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.typed = text

	if s.busy {
		s.renderLocked()
	}
}

// reportThinking prints what the reasoning actually cost, from the endpoint's
// own usage block.
func (s *status) reportThinking(elapsed time.Duration, tokens int) {
	if tokens <= 0 {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Clear the live region first. Printing through printlnLocked redraws the
	// block, so a region left populated would be drawn again BELOW the summary -
	// the indicator outliving the thing it was indicating.
	s.thinking = nil
	s.eraseBlockLocked()

	s.printlnLocked(fmt.Sprintf("%s%s✻ thought for %.1fs · %s tokens%s",
		dim, italicOn, elapsed.Seconds(), humanTokens(tokens), reset))
}

// pause stops the spinner and clears the status line, for when the harness is
// waiting on a person rather than doing work.
//
// A spinner is a claim that something is progressing. Showing one while blocked
// on an approval is a lie, and because it redraws several times a second it also
// keeps wiping the question off the screen - which is exactly what makes an
// approval prompt unreadable.
func (s *status) pause() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.busy = false
	s.flushLocked()
	s.clearLocked()
}

// resume restarts the spinner after a pause.
func (s *status) resume() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.busy = true
	s.renderLocked()
}

// flushStream releases anything the markdown renderer is still holding.
//
// The renderer holds an unterminated final line so a construct split across
// deltas is never printed half-formed. That line has to be released BEFORE
// anything else is printed, or it appears after it - which is what puts the tail
// of an answer below the step summary and makes one reply look like two.
func (s *status) flushStream() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.pending.WriteString(s.md.flush())
	s.flushLocked()
}

// end marks the shell idle and clears the status line.
func (s *status) end() {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Anything the renderer is still holding belongs on screen before the
	// status line is torn down.
	s.pending.WriteString(s.md.flush())
	s.flushLocked()
	s.busy = false
	s.clearLocked()
}

// setThinkingLevel records the reasoning effort label.
func (s *status) setThinkingLevel(label string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.thinkingLevel = label
}

// setProgress updates the counters shown in the status line.
func (s *status) setProgress(steps, inputTokens int, hit float64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.steps = steps
	s.input = inputTokens
	s.hit = hit
}

// setStepLimit records the per-turn ceiling.
func (s *status) setStepLimit(limit int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.stepLimit = limit
}

// setMode updates only the mode label.
func (s *status) setMode(mode string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.mode = mode
}

// setPlanMode records whether plan mode is active, which the status line states
// loudly.
func (s *status) setPlanMode(active bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.planMode = active
}

// pink returns the brand colour escape this terminal can actually render.
func pink() string {
	if truecolor() {
		return pinkTruecolor
	}

	return pink256
}

// truecolor reports whether the terminal advertises 24-bit colour.
func truecolor() bool {
	term := os.Getenv("COLORTERM")

	return strings.Contains(term, "truecolor") || strings.Contains(term, "24bit")
}

// Style helpers. Each returns its input unchanged when the output is not a
// terminal, so a piped approval prompt carries no escape sequences.

func (s *status) bold(text string) string {
	if !s.ansi {
		return text
	}

	return boldOn + text + boldOff
}

func (s *status) tone(text string) string {
	if !s.ansi {
		return text
	}

	return dim + text + reset
}

func (s *status) code(text string) string {
	if !s.ansi {
		return text
	}

	return codeStyle + text + reset
}

func (s *status) brand(text string) string {
	if !s.ansi {
		return text
	}

	return pink() + text + reset
}

// Diff colours. Green and red are the convention for added and removed, and a
// diff is one place where the convention is worth more than a palette choice -
// every developer already reads these two colours without thinking.
const (
	addedStyle   = "\x1b[32m"
	removedStyle = "\x1b[31m"

	// maxDiffLines bounds what one file change prints. A rewritten file has no
	// unchanged run to elide, so without this the whole file would be shown.
	maxDiffLines = 20

	// diffContextLines is how many unchanged lines survive around each change.
	// A diff is read for what changed; more context than this pushes the
	// interesting part off the screen.
	diffContextLines = 2
)

func (s *status) added(text string) string {
	if !s.ansi {
		return text
	}

	return addedStyle + text + reset
}

func (s *status) removed(text string) string {
	if !s.ansi {
		return text
	}

	return removedStyle + text + reset
}

// humanTokens shortens a token count for a narrow status line.
func humanTokens(n int) string {
	switch {
	case n >= 999_950:
		// Rounds to 1000.0k and above, which reads as a mistake.
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}
