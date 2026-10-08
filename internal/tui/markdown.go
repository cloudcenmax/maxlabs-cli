package tui

import (
	"regexp"
	"strings"
)

// Markdown rendering for the terminal.
//
// KV Cache effect: none. This runs after the model has answered and touches only
// what is printed - never a request. The model still writes natural markdown,
// which is what it is best at, and the shell decides how that looks.
//
// The renderer is LINE-BASED and stateful, which is what makes it work while
// streaming. A construct can arrive split across two deltas, so rendering each
// delta independently would show a stray `*` or backtick. Instead complete lines
// are rendered and anything unterminated is held until its line ends, with two
// pieces of state carried between lines: whether a code fence is open, and the
// partial line.
const (
	boldOn    = "\x1b[1m"
	boldOff   = "\x1b[22m"
	italicOn  = "\x1b[3m"
	italicOff = "\x1b[23m"

	// codeStyle is cyan. It is deliberately NOT the brand pink: the pink means
	// "the harness is working", and spending it on code spans would dilute a
	// signal into decoration.
	codeStyle = "\x1b[36m"
)

var (
	reCodeSpan = regexp.MustCompile("`([^`]+)`")
	reBold     = regexp.MustCompile(`\*\*([^*]+)\*\*|__([^_]+)__`)
	reItalic   = regexp.MustCompile(`\*([^*\n]+)\*|(?:\A|\s)_([^_\n]+)_`)
	reFence    = regexp.MustCompile("^\\s*```")
	reHeading  = regexp.MustCompile(`^(#{1,6})\s+(.*)$`)
	reBullet   = regexp.MustCompile(`^(\s*)[-*+]\s+(.*)$`)
	reOrdered  = regexp.MustCompile(`^(\s*)(\d+)\.\s+(.*)$`)
	reQuote    = regexp.MustCompile(`^(\s*)>\s?(.*)$`)
)

// markdown renders model output for a terminal.
type markdown struct {
	// styled enables ANSI. When false the text passes through untouched, which
	// keeps piped and redirected output byte-accurate.
	styled bool

	// inFence tracks whether an unterminated code fence is open. It has to
	// survive between lines because a fence spans them.
	inFence bool

	// line holds the part of a line that has not been terminated yet.
	line strings.Builder

	// width is the terminal's column count, or 0 when unknown. Streamed text is
	// wrapped to it so the terminal never has to wrap - and so a paragraph
	// streams instead of appearing all at once when it finally ends.
	width int
}

// partialFlush is how long an unterminated line may grow before it is printed
// anyway.
//
// Waiting for a newline keeps every construct intact, but a long paragraph would
// then appear all at once and streaming would look broken. Flushing at a word
// boundary is the compromise; the cost is that a marker split exactly at the
// boundary shows its raw characters, which is rare and cosmetic.
const partialFlush = 200

// writeLine renders one complete line, including its newline.
func (m *markdown) writeLine(line string) string {
	if !m.styled {
		return line + "\n"
	}

	// A fence line toggles state and prints nothing itself: the fence is syntax,
	// not content.
	if reFence.MatchString(line) {
		m.inFence = !m.inFence

		// The fence prints nothing in either direction. Emitting a newline on the
		// closing fence double-spaces the text below it, because the last code
		// line and any following blank line each already end one.
		return ""
	}

	if m.inFence {
		// Indent so code is visually distinct without relying on colour alone.
		return "  " + dim + line + reset + "\n"
	}

	if match := reHeading.FindStringSubmatch(line); match != nil {
		return "\n" + boldOn + m.inline(match[2]) + boldOff + "\n"
	}

	if match := reBullet.FindStringSubmatch(line); match != nil {
		return match[1] + "• " + m.inline(match[2]) + "\n"
	}

	if match := reOrdered.FindStringSubmatch(line); match != nil {
		return match[1] + match[2] + ". " + m.inline(match[3]) + "\n"
	}

	if match := reQuote.FindStringSubmatch(line); match != nil {
		return match[1] + dim + "│ " + reset + m.inline(match[2]) + "\n"
	}

	return m.inline(line) + "\n"
}

// inline applies emphasis and code styling within a single line.
//
// Code spans are handled first and in isolation: a backtick-delimited run must
// not have emphasis applied inside it, or `a_b_c` would come out half italic.
func (m *markdown) inline(line string) string {
	if !m.styled {
		return line
	}

	var out strings.Builder

	parts := reCodeSpan.Split(line, -1)
	spans := reCodeSpan.FindAllStringSubmatch(line, -1)

	for i, part := range parts {
		out.WriteString(m.emphasis(part))
		if i < len(spans) {
			out.WriteString(codeStyle + spans[i][1] + reset)
		}
	}

	return out.String()
}

// emphasis applies bold then italic. Order matters: replacing bold first removes
// the `**` pairs that the italic rule would otherwise see as two single markers.
func (m *markdown) emphasis(s string) string {
	s = reBold.ReplaceAllStringFunc(s, func(match string) string {
		inner := strings.TrimSuffix(strings.TrimPrefix(match, "**"), "**")
		inner = strings.TrimSuffix(strings.TrimPrefix(inner, "__"), "__")

		return boldOn + inner + boldOff
	})

	s = reItalic.ReplaceAllStringFunc(s, func(match string) string {
		// Preserve any leading whitespace the pattern had to consume to anchor.
		lead := ""
		for len(match) > 0 && (match[0] == ' ' || match[0] == '\t') {
			lead += string(match[0])
			match = match[1:]
		}

		inner := strings.TrimSuffix(strings.TrimPrefix(match, "*"), "*")
		inner = strings.TrimSuffix(strings.TrimPrefix(inner, "_"), "_")

		return lead + italicOn + inner + italicOff
	})

	return s
}

// feed consumes newly arrived text and returns what should be printed.
//
// Only complete lines are rendered; the trailing partial is held so a construct
// split across deltas is never printed half-formed.
func (m *markdown) feed(text string) string {
	m.line.WriteString(text)

	var out strings.Builder

	// Line endings are normalised: the wire carries whichever the model emits,
	// and mixing them into a pinned status line is what produces stray carriage
	// returns mid-screen.
	buffered := strings.ReplaceAll(m.line.String(), "\r\n", "\n")
	buffered = strings.ReplaceAll(buffered, "\r", "\n")

	for {
		index := strings.IndexByte(buffered, '\n')
		if index < 0 {
			break
		}

		out.WriteString(m.writeLine(buffered[:index]))
		buffered = buffered[index+1:]
	}

	m.line.Reset()

	// Wrap the partial to the terminal width so a long paragraph streams.
	//
	// The width matters: breaking on an arbitrary character count puts hard
	// newlines in the middle of prose, which is what makes a long answer look
	// ragged. Breaking at a space within the terminal's own width is what the
	// terminal would have done anyway, so the two agree.
	if m.width > 1 {
		chunks, rest := wrapVisible(buffered, m.width-1)
		for _, chunk := range chunks {
			out.WriteString(m.renderWrapped(chunk))
		}

		buffered = rest
	} else if len(buffered) > partialFlush {
		// No known width: fall back to a length threshold at a word boundary.
		if cut := strings.LastIndexByte(buffered, ' '); cut > 0 {
			out.WriteString(m.renderWrapped(buffered[:cut]))
			buffered = buffered[cut+1:]
		}
	}

	m.line.WriteString(buffered)

	return out.String()
}

// renderWrapped renders a chunk of an unterminated line that is being wrapped.
func (m *markdown) renderWrapped(line string) string {
	if !m.styled || m.inFence {
		return line + "\n"
	}

	return m.inline(line) + "\n"
}

// flush renders whatever is still held, for the end of a message.
func (m *markdown) flush() string {
	text := m.line.String()
	m.line.Reset()

	// An unterminated fence must be closed here REGARDLESS of whether any text
	// is still held. A fence can be fully consumed by feed() - the opening line
	// and every code line ended in a newline - leaving nothing buffered, and
	// returning early in that case would leave the fence open so the next
	// message inherited the code style.
	m.inFence = false

	if text == "" {
		return ""
	}

	if !m.styled {
		return text
	}

	return m.inline(text)
}
