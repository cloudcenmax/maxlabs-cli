package diff_test

import (
	"strings"
	"testing"

	"censi/harness/internal/diff"
)

// render flattens a diff into a readable form for assertions.
func render(lines []diff.Line) string {
	var out strings.Builder

	for _, line := range lines {
		switch line.Kind {
		case diff.Add:
			out.WriteString("+" + line.Text + "\n")
		case diff.Del:
			out.WriteString("-" + line.Text + "\n")
		case diff.Gap:
			out.WriteString("...\n")
		default:
			out.WriteString(" " + line.Text + "\n")
		}
	}

	return out.String()
}

// TestNewFileIsAllAdditions: a file that did not exist has no removals.
func TestNewFileIsAllAdditions(t *testing.T) {
	lines := diff.Lines("", "one\ntwo\n")

	added, removed := diff.Stats(lines)
	if added != 2 || removed != 0 {
		t.Fatalf("added=%d removed=%d, want 2 and 0:\n%s", added, removed, render(lines))
	}
}

// TestSingleLineEditShowsBothSides is the common case, and the one a person
// most needs to see.
func TestSingleLineEditShowsBothSides(t *testing.T) {
	lines := diff.Lines("a\nb\nc\n", "a\nB\nc\n")

	if got := render(lines); got != " a\n-b\n+B\n c\n" {
		t.Fatalf("diff =\n%s", got)
	}
}

// TestIdenticalTextsProduceNoChanges keeps a no-op write from looking like one.
func TestIdenticalTextsProduceNoChanges(t *testing.T) {
	lines := diff.Lines("same\n", "same\n")

	added, removed := diff.Stats(lines)
	if added != 0 || removed != 0 {
		t.Fatalf("added=%d removed=%d", added, removed)
	}
}

// TestInsertionIsPureAddition.
func TestInsertionIsPureAddition(t *testing.T) {
	lines := diff.Lines("a\nc\n", "a\nb\nc\n")

	if got := render(lines); got != " a\n+b\n c\n" {
		t.Fatalf("diff =\n%s", got)
	}
}

// TestDeletionIsPureRemoval.
func TestDeletionIsPureRemoval(t *testing.T) {
	lines := diff.Lines("a\nb\nc\n", "a\nc\n")

	if got := render(lines); got != " a\n-b\n c\n" {
		t.Fatalf("diff =\n%s", got)
	}
}

// TestTrailingNewlineDoesNotInventALine: a file ending in a newline must not
// appear to gain or lose a blank line.
func TestTrailingNewlineDoesNotInventALine(t *testing.T) {
	for _, c := range []struct{ old, new string }{
		{"a\n", "a"},
		{"a", "a\n"},
		{"a\n", "a\n"},
	} {
		lines := diff.Lines(c.old, c.new)

		added, removed := diff.Stats(lines)
		if added != 0 || removed != 0 {
			t.Fatalf("%q -> %q reported +%d -%d:\n%s", c.old, c.new, added, removed, render(lines))
		}
	}
}

// TestMultipleChangesAreAllFound: the point of a real diff over trimming.
func TestMultipleChangesAreAllFound(t *testing.T) {
	old := "a\nb\nc\nd\ne\nf\ng\nh\n"
	new := "a\nB\nc\nd\ne\nf\nG\nh\n"

	lines := diff.Lines(old, new)

	if got := render(lines); got != " a\n-b\n+B\n c\n d\n e\n f\n-g\n+G\n h\n" {
		t.Fatalf("diff =\n%s", got)
	}
}

// TestCRLFIsNormalised, or every line would look changed.
func TestCRLFIsNormalised(t *testing.T) {
	lines := diff.Lines("a\r\nb\r\n", "a\nb\n")

	added, removed := diff.Stats(lines)
	if added != 0 || removed != 0 {
		t.Fatalf("line endings alone produced +%d -%d:\n%s", added, removed, render(lines))
	}
}

// TestCondenseElidesFarContext: a small change in a large file must not print
// the whole file.
func TestCondenseElidesFarContext(t *testing.T) {
	var oldB, newB strings.Builder
	for i := 0; i < 200; i++ {
		oldB.WriteString("line\n")
		newB.WriteString("line\n")
	}
	oldB.WriteString("changed\n")
	newB.WriteString("different\n")

	lines := diff.Condense(diff.Lines(oldB.String(), newB.String()), 2)

	same := 0
	for _, line := range lines {
		if line.Kind == diff.Same {
			same++
		}
	}

	if same > 6 {
		t.Fatalf("kept %d context lines, expected a handful:\n%s", same, render(lines))
	}

	gaps := 0
	for _, line := range lines {
		if line.Kind == diff.Gap {
			gaps++
		}
	}
	if gaps == 0 {
		t.Fatal("no elision marker was inserted")
	}
}

// TestGapIsDistinctFromABlankLine: a file can contain a real blank line, and a
// reader must be able to tell it from elided output.
func TestGapIsDistinctFromABlankLine(t *testing.T) {
	lines := diff.Lines("a\n\nb\n", "a\n\nb\n")

	for _, line := range lines {
		if line.Kind == diff.Gap {
			t.Fatal("a genuine blank line was reported as an elision")
		}
	}
}

// TestLargeInputsFallBackWithoutLosingTheChange: past the table bound the diff
// degrades, but it must still report what changed.
func TestLargeInputsFallBackWithoutLosingTheChange(t *testing.T) {
	old := strings.Repeat("filler\n", 3000) + "old\n"
	new := strings.Repeat("filler\n", 3000) + "new\n"

	lines := diff.Lines(old, new)

	added, removed := diff.Stats(lines)
	if added == 0 || removed == 0 {
		t.Fatalf("the change was lost in the fallback: +%d -%d", added, removed)
	}

	if !strings.Contains(render(lines), "-old\n") || !strings.Contains(render(lines), "+new\n") {
		t.Fatalf("diff =\n%s", render(lines)[:200])
	}
}
