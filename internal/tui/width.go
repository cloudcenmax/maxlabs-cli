package tui

import "strings"

// Visible width.
//
// Escape sequences occupy no columns, so a line's width cannot be measured in
// bytes. Everything that decides where to wrap or when to truncate goes through
// here, or a coloured status line would be cut short while a plain one fitted.

// visibleWidth counts the columns a string occupies.
//
// It counts runes rather than cells: a wide CJK character occupies two columns,
// and treating it as one only ever under-estimates, which errs toward leaving
// room rather than overflowing.
func visibleWidth(s string) int {
	count := 0
	escaping := false

	for _, r := range s {
		switch {
		case escaping:
			if r == 'm' || r == 'K' || r == 'A' {
				escaping = false
			}

		case r == 0x1b:
			escaping = true

		default:
			count++
		}
	}

	return count
}

// isEscapeFinal reports whether a rune ends an escape sequence.
func isEscapeFinal(r rune) bool {
	return r == 'm' || r == 'K' || r == 'A' || r == 'B' || r == 'C' || r == 'D'
}

// truncateVisible cuts a string to a column budget, keeping escape sequences
// intact and closing any styling that was opened.
func truncateVisible(s string, limit int) string {
	if limit <= 0 || visibleWidth(s) <= limit {
		return s
	}

	var (
		out       strings.Builder
		count     int
		escaping  bool
		sawEscape bool
	)

	for _, r := range s {
		if escaping {
			out.WriteRune(r)
			if isEscapeFinal(r) {
				escaping = false
			}

			continue
		}

		if r == 0x1b {
			escaping = true
			// Any escape at all means styling may be open when the cut lands.
			sawEscape = true
			out.WriteRune(r)

			continue
		}

		if count >= limit {
			break
		}

		out.WriteRune(r)
		count++
	}

	// A truncated line may have left a colour or bold on. Close it, or the
	// styling bleeds into everything written afterwards.
	if sawEscape {
		out.WriteString(reset)
	}

	return out.String()
}

// wrapVisible splits text into column-bounded chunks at word boundaries.
//
// It returns the chunks that fit and the remainder. A single word longer than
// the budget is broken rather than allowed to overflow, because overflow is what
// produces the ragged output this exists to prevent.
func wrapVisible(text string, width int) ([]string, string) {
	if width <= 0 {
		return nil, text
	}

	var chunks []string

	for visibleWidth(text) > width {
		runes := []rune(text)
		count := 0
		cut := -1

		for i, r := range runes {
			if count >= width {
				break
			}
			if r == ' ' {
				cut = i
			}
			count++
		}

		if cut <= 0 {
			// No usable break: hard-split at the budget.
			chunks = append(chunks, string(runes[:min(width, len(runes))]))
			text = string(runes[min(width, len(runes)):])

			continue
		}

		chunks = append(chunks, string(runes[:cut]))
		text = strings.TrimLeft(string(runes[cut:]), " ")
	}

	return chunks, text
}

func min(a, b int) int {
	if a < b {
		return a
	}

	return b
}

// tailRunes returns the last n runes of s.
func tailRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}

	runes := []rune(s)
	if len(runes) <= n {
		return s
	}

	return string(runes[len(runes)-n:])
}
