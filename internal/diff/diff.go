// Package diff computes line diffs for display.
//
// KV Cache effect: none. A diff is computed from bytes already on disk and is
// never sent to a model.
//
// This is a presentational diff, not a patch format. It exists so a person can
// see what changed without opening the file, and it deliberately trades minimal
// output for small, readable code.
package diff

import "strings"

// Kind classifies one line of a diff.
type Kind int

const (
	// Same is a context line.
	Same Kind = iota

	// Add is a line present only in the new text.
	Add

	// Del is a line present only in the old text.
	Del

	// Gap marks elided unchanged lines. It is a distinct kind rather than an
	// empty Same line, because a file can genuinely contain a blank line and a
	// reader must be able to tell the two apart.
	Gap
)

// Line is one line of a diff.
type Line struct {
	Kind Kind
	Text string
}

// maxTableCells bounds the quadratic table.
//
// An exact diff needs a table of len(old) x len(new). A large file would allocate
// hundreds of megabytes to answer a question nobody asked - whether a thousand
// line file changed. Past this bound the diff falls back to trimming the common
// head and tail, which is still exactly right for the common case of a small
// edit inside a large file.
const maxTableCells = 4_000_000

// Lines computes a line diff between two texts.
func Lines(old, new string) []Line {
	oldLines := splitLines(old)
	newLines := splitLines(new)

	if len(oldLines)*len(newLines) > maxTableCells {
		return trimmed(oldLines, newLines)
	}

	return lcs(oldLines, newLines)
}

// splitLines splits on newlines, dropping a single trailing empty line so a file
// ending in a newline does not appear to gain a blank line.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}

	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	return lines
}

// lcs computes an exact diff using a longest-common-subsequence table.
func lcs(oldLines, newLines []string) []Line {
	n, m := len(oldLines), len(newLines)

	// table[i][j] is the LCS length of oldLines[i:] and newLines[j:].
	table := make([][]int, n+1)
	for i := range table {
		table[i] = make([]int, m+1)
	}

	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if oldLines[i] == newLines[j] {
				table[i][j] = table[i+1][j+1] + 1

				continue
			}

			if table[i+1][j] >= table[i][j+1] {
				table[i][j] = table[i+1][j]
			} else {
				table[i][j] = table[i][j+1]
			}
		}
	}

	var out []Line

	for i, j := 0, 0; i < n || j < m; {
		switch {
		case i < n && j < m && oldLines[i] == newLines[j]:
			out = append(out, Line{Kind: Same, Text: oldLines[i]})
			i++
			j++

		// Removals are emitted before additions. When the table offers a choice
		// the two orders are equally minimal, and the convention - what changed
		// away, then what replaced it - is what every reader already expects.
		case i < n && (j == m || table[i+1][j] >= table[i][j+1]):
			out = append(out, Line{Kind: Del, Text: oldLines[i]})
			i++

		default:
			out = append(out, Line{Kind: Add, Text: newLines[j]})
			j++
		}
	}

	return out
}

// trimmed falls back to removing the common head and tail.
//
// It is exact when there is a single contiguous change, and honest when there is
// not: everything between the common head and tail is reported as removed and
// then added, which is a superset of the true change rather than a wrong answer.
func trimmed(oldLines, newLines []string) []Line {
	head := 0
	for head < len(oldLines) && head < len(newLines) && oldLines[head] == newLines[head] {
		head++
	}

	tail := 0
	for tail < len(oldLines)-head && tail < len(newLines)-head &&
		oldLines[len(oldLines)-1-tail] == newLines[len(newLines)-1-tail] {
		tail++
	}

	var out []Line

	for _, line := range oldLines[:head] {
		out = append(out, Line{Kind: Same, Text: line})
	}
	for _, line := range oldLines[head : len(oldLines)-tail] {
		out = append(out, Line{Kind: Del, Text: line})
	}
	for _, line := range newLines[head : len(newLines)-tail] {
		out = append(out, Line{Kind: Add, Text: line})
	}
	for _, line := range oldLines[len(oldLines)-tail:] {
		out = append(out, Line{Kind: Same, Text: line})
	}

	return out
}

// Stats counts the changes in a diff.
func Stats(lines []Line) (added, removed int) {
	for _, line := range lines {
		switch line.Kind {
		case Add:
			added++
		case Del:
			removed++
		}
	}

	return added, removed
}

// Condense replaces long runs of unchanged lines with a gap marker.
//
// A diff is read for what changed, so three lines of context on either side is
// what a person needs; the rest is noise that pushes the interesting part off
// the screen.
func Condense(lines []Line, context int) []Line {
	if context < 0 {
		context = 0
	}

	// Mark which context lines survive: the ones near a change.
	keep := make([]bool, len(lines))
	for i, line := range lines {
		if line.Kind == Same {
			continue
		}

		for j := i - context; j <= i+context; j++ {
			if j >= 0 && j < len(lines) {
				keep[j] = true
			}
		}
	}

	var out []Line
	gap := false

	for i, line := range lines {
		if keep[i] {
			out = append(out, line)
			gap = false

			continue
		}

		if !gap {
			// A single elision marker per run, not one per dropped line.
			out = append(out, Line{Kind: Gap})
			gap = true
		}
	}

	return out
}
