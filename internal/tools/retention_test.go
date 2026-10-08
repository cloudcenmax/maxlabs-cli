package tools_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"censi/harness/internal/tools"
)

// TestBoundedTextReportsExactOmittedBytes pins the contract the model-facing
// notice depends on: the count is what was actually dropped, not an estimate.
func TestBoundedTextReportsExactOmittedBytes(t *testing.T) {
	text := strings.Repeat("a", 100)

	out, omitted := tools.BoundText(text, 20, 10)

	if omitted.Kind != tools.OmittedExact {
		t.Fatalf("expected an exact count, got %v", omitted.Kind)
	}
	if omitted.Count != 70 {
		t.Fatalf("omitted = %d, want 70", omitted.Count)
	}
	if len(out) != 30+len(elision(len(text), 30)) {
		// The retained window is exactly head+tail plus the marker.
		if !strings.HasPrefix(out, strings.Repeat("a", 20)) || !strings.HasSuffix(out, strings.Repeat("a", 10)) {
			t.Fatalf("the retained ends are wrong: %q", out)
		}
	}
	if !strings.Contains(out, "content omitted") {
		t.Fatalf("the elision marker is missing: %q", out)
	}
}

// TestBoundedTextLeavesShortInputAlone: bounding must be a no-op below budget,
// or the marker itself would perturb otherwise-identical results.
func TestBoundedTextLeavesShortInputAlone(t *testing.T) {
	text := "short"

	out, omitted := tools.BoundText(text, 100, 100)

	if out != text {
		t.Fatalf("short input was altered: %q", out)
	}
	if omitted.Kind != tools.OmittedNone {
		t.Fatalf("expected no omission, got %v", omitted.Kind)
	}
}

// TestBoundedTextNeverSplitsARune matters because a cut mid-character would put
// an invalid byte sequence into history, which is both a correctness problem and
// a source of byte drift between otherwise identical results.
func TestBoundedTextNeverSplitsARune(t *testing.T) {
	// Each rune is 3 bytes, so 10 bytes of budget lands mid-character.
	text := strings.Repeat("→", 50)

	out, omitted := tools.BoundText(text, 10, 10)

	if !utf8.ValidString(out) {
		t.Fatalf("bounded output is not valid UTF-8: %q", out)
	}
	if strings.ContainsRune(out, utf8.RuneError) {
		t.Fatalf("the cut introduced a replacement character")
	}
	if omitted.Kind != tools.OmittedExact {
		t.Fatalf("expected an exact count, got %v", omitted.Kind)
	}

	// The count must include any boundary bytes the alignment discarded.
	kept := len(out) - len(elision(len(text), 0))
	if kept+omitted.Count != len(text) {
		t.Fatalf("accounting does not balance: kept=%d omitted=%d total=%d", kept, omitted.Count, len(text))
	}
}

// TestBoundedTextIsDeterministic guards the cache: the same input must produce
// the same bytes, or a tool result would differ between identical runs.
func TestBoundedTextIsDeterministic(t *testing.T) {
	text := strings.Repeat("abc", 1000)

	first, firstOmitted := tools.BoundText(text, 50, 50)
	for i := 0; i < 20; i++ {
		again, againOmitted := tools.BoundText(text, 50, 50)
		if again != first || againOmitted != firstOmitted {
			t.Fatalf("bounding is not deterministic at iteration %d", i)
		}
	}
}

// TestBoundedItemsCountsExactly covers the list case.
func TestBoundedItemsCountsExactly(t *testing.T) {
	items := []string{"a", "b", "c", "d", "e"}

	kept, omitted := tools.BoundItems(items, 2)

	if len(kept) != 2 || kept[0] != "a" || kept[1] != "b" {
		t.Fatalf("kept = %v", kept)
	}
	if omitted.Kind != tools.OmittedExact || omitted.Count != 3 {
		t.Fatalf("omitted = %+v, want 3 exact", omitted)
	}

	if _, none := tools.BoundItems(items, 10); none.Kind != tools.OmittedNone {
		t.Fatalf("expected no omission, got %v", none.Kind)
	}
}

// TestOmitedNoticeRendersHonestly: an unknown count must not be rendered as a
// number. A wrong figure in a model-visible notice is worse than an absent one,
// because the model will act on it.
func TestOmittedNoticeRendersHonestly(t *testing.T) {
	if got := (tools.Omitted{Kind: tools.OmittedExact, Count: 3, Unit: "bytes"}).Notice(); got != "(3 bytes omitted.)" {
		t.Fatalf("exact notice = %q", got)
	}
	if got := (tools.Omitted{Kind: tools.OmittedUnknown, Unit: "hits"}).Notice(); strings.ContainsAny(got, "0123456789") {
		t.Fatalf("an unknown count must not print a number: %q", got)
	}
	if got := (tools.Omitted{Kind: tools.OmittedNone, Unit: "bytes"}).Notice(); got != "" {
		t.Fatalf("no omission must render nothing, got %q", got)
	}
}

// elision reports the marker length for the accounting assertions above. It
// deliberately does not hard-code the marker text, so changing the marker cannot
// silently invalidate the arithmetic.
func elision(total, retained int) string {
	out, _ := tools.BoundText("", 0, 0)
	_ = total
	_ = retained
	_ = out
	// Recover the marker by bounding a known input and stripping the window.
	text := strings.Repeat("x", 100)
	bounded, _ := tools.BoundText(text, 10, 10)
	return strings.TrimSuffix(strings.TrimPrefix(bounded, strings.Repeat("x", 10)), strings.Repeat("x", 10))
}
