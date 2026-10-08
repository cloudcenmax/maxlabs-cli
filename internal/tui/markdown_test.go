package tui

import (
	"strings"
	"testing"
)

// stripAnsi removes escape sequences so assertions can be about text.
func stripAnsi(s string) string {
	out := strings.Builder{}

	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			i++
			continue
		}

		out.WriteByte(s[i])
		i++
	}

	return out.String()
}

// render is the helper the tests use.
func render(styled bool, chunks ...string) string {
	m := &markdown{styled: styled}

	var out strings.Builder
	for _, chunk := range chunks {
		out.WriteString(m.feed(chunk))
	}
	out.WriteString(m.flush())

	return out.String()
}

// TestMarkdownRemovesLiteralMarkers is the whole point: the user should not see
// the syntax, only the thing it was trying to express.
func TestMarkdownRemovesLiteralMarkers(t *testing.T) {
	out := render(true, "This is **bold** and this is `code`.\n")

	plain := stripAnsi(out)
	if strings.Contains(plain, "**") {
		t.Fatalf("bold markers survived: %q", plain)
	}
	if strings.Contains(plain, "`") {
		t.Fatalf("backticks survived: %q", plain)
	}
	if !strings.Contains(plain, "bold") || !strings.Contains(plain, "code") {
		t.Fatalf("the words should remain: %q", plain)
	}

	// And they should carry styling.
	if !strings.Contains(out, boldOn) {
		t.Fatalf("bold produced no styling: %q", out)
	}
	if !strings.Contains(out, codeStyle) {
		t.Fatalf("code produced no styling: %q", out)
	}
}

// TestMarkdownHeadingsAndBullets covers the structure that reads worst raw.
func TestMarkdownHeadingsAndBullets(t *testing.T) {
	out := render(true, "## Findings\n\n- first point\n- second point\n")

	plain := stripAnsi(out)
	if strings.Contains(plain, "##") {
		t.Fatalf("heading markers survived: %q", plain)
	}
	if !strings.Contains(plain, "Findings") {
		t.Fatalf("heading text lost: %q", plain)
	}
	if !strings.Contains(plain, "• first point") {
		t.Fatalf("bullets were not normalised: %q", plain)
	}
	if strings.Contains(plain, "- first") {
		t.Fatalf("a raw dash bullet survived: %q", plain)
	}
}

// TestMarkdownCodeFenceIsPreserved is the case that breaks naive renderers:
// inside a fence, markdown is content, not syntax.
func TestMarkdownCodeFenceIsPreserved(t *testing.T) {
	out := render(true, "Before\n```php\n$a = **$b;\n```\nAfter\n")

	plain := stripAnsi(out)
	if !strings.Contains(plain, "$a = **$b;") {
		t.Fatalf("code content was altered: %q", plain)
	}
	if strings.Contains(plain, "```") {
		t.Fatalf("fence markers survived: %q", plain)
	}
	if !strings.Contains(plain, "Before") || !strings.Contains(plain, "After") {
		t.Fatalf("surrounding text lost: %q", plain)
	}
}

// TestMarkdownSurvivesSplitsAcrossDeltas is the streaming contract.
//
// A construct routinely arrives in pieces. Rendering each delta independently
// would print a stray marker; holding until the line ends avoids it.
func TestMarkdownSurvivesSplitsAcrossDeltas(t *testing.T) {
	// The same logical message, split at an awkward point.
	out := render(true, "This is **bo", "ld** and `co", "de`.\n")

	plain := stripAnsi(out)
	if strings.Contains(plain, "**") || strings.Contains(plain, "`") {
		t.Fatalf("a split construct leaked its markers: %q", plain)
	}
	if !strings.Contains(plain, "This is bold and code.") {
		t.Fatalf("the assembled text is wrong: %q", plain)
	}
}

// TestMarkdownFenceSurvivesSplits: the fence marker itself can be split, which
// would otherwise be read as ordinary text.
func TestMarkdownFenceSurvivesSplits(t *testing.T) {
	out := render(true, "```", "php\n$x = 1;\n", "```", "\n")

	plain := stripAnsi(out)
	if strings.Contains(plain, "```") {
		t.Fatalf("a split fence leaked: %q", plain)
	}
	if !strings.Contains(plain, "$x = 1;") {
		t.Fatalf("code content lost: %q", plain)
	}
}

// TestMarkdownUnstyledPassesThrough keeps piped output byte-accurate: the
// renderer is a terminal concern, and a redirect should not silently rewrite
// the model's answer.
func TestMarkdownUnstyledPassesThrough(t *testing.T) {
	const input = "## Heading\n- item with **bold**\n"

	out := render(false, input)
	if out != input {
		t.Fatalf("unstyled output was modified:\n got: %q\nwant: %q", out, input)
	}
}

// TestMarkdownEmptyStaysQuiet guards the status line from stray blank output.
func TestMarkdownEmptyStaysQuiet(t *testing.T) {
	if got := render(true, ""); got != "" {
		t.Fatalf("empty input produced %q", got)
	}
}

// TestMarkdownCRLFIsNormalised: a stray carriage return lands mid-screen and
// scrambles the pinned status line.
func TestMarkdownCRLFIsNormalised(t *testing.T) {
	out := render(true, "one\r\ntwo\r\n")

	if strings.Contains(out, "\r") {
		t.Fatalf("a carriage return survived: %q", out)
	}
	if out != "one\ntwo\n" {
		t.Fatalf("out = %q", out)
	}
}

// TestMarkdownEmphasisInsideCodeSpanIsUntouched: `a_b_c` must not come out half
// italic, which is what happens when emphasis is applied before code spans are
// isolated.
func TestMarkdownEmphasisInsideCodeSpanIsUntouched(t *testing.T) {
	out := render(true, "run `a_b_c` now\n")

	plain := stripAnsi(out)
	if strings.Contains(out, italicOn) {
		t.Fatalf("emphasis was applied inside a code span: %q", out)
	}
	if !strings.Contains(plain, "a_b_c") {
		t.Fatalf("code content altered: %q", plain)
	}
}

// TestMarkdownFlushReleasesHeldText: the end of a message must not swallow a
// trailing line that never got its newline.
func TestMarkdownFlushReleasesHeldText(t *testing.T) {
	out := render(true, "a closing **sentence**")

	if !strings.Contains(stripAnsi(out), "a closing sentence") {
		t.Fatalf("the trailing line was lost: %q", out)
	}
}

// TestMarkdownClosesAnOpenFence keeps a message that ends mid-fence from
// leaking code styling into everything printed afterwards.
func TestMarkdownClosesAnOpenFence(t *testing.T) {
	m := &markdown{styled: true}

	_ = m.feed("```\ncode\n")
	_ = m.flush()

	if m.inFence {
		t.Fatal("an unterminated fence must not stay open across messages")
	}
}

// TestMarkdownOrderedLists keeps numbers readable.
func TestMarkdownOrderedLists(t *testing.T) {
	out := render(true, "1. first\n2. second\n")

	plain := stripAnsi(out)
	if !strings.Contains(plain, "1. first") || !strings.Contains(plain, "2. second") {
		t.Fatalf("ordered list mangled: %q", plain)
	}
}
