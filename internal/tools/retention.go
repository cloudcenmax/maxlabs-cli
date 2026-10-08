package tools

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// OmittedKind describes how confident a reported omission count is.
type OmittedKind string

const (
	// OmittedNone means nothing was dropped.
	OmittedNone OmittedKind = "none"

	// OmittedExact means the count is the precise number of units dropped,
	// because the caller observed every unit before deciding.
	OmittedExact OmittedKind = "exact"

	// OmittedUnknown means something was dropped and the caller did not count
	// it. Reported honestly as unknown rather than as a guess, because a wrong
	// number in a model-visible notice is worse than an honest absence.
	OmittedUnknown OmittedKind = "unknown"
)

// Omitted reports what bounding removed.
type Omitted struct {
	Kind  OmittedKind
	Count int
	Unit  string
}

// Notice renders the omission as a model-visible clause, or "" when nothing was
// dropped.
func (o Omitted) Notice() string {
	switch o.Kind {
	case OmittedExact:
		return fmt.Sprintf("(%d %s omitted.)", o.Count, o.Unit)
	case OmittedUnknown:
		return fmt.Sprintf("(Some %s omitted.)", o.Unit)
	default:
		return ""
	}
}

// elisionMarker is placed between the retained head and tail of a bounded
// string. It is a fixed literal on purpose: the marker is part of the bytes the
// provider caches, so it must not vary with the amount dropped.
const elisionMarker = "\n\n[... content omitted ...]\n\n"

// BoundText keeps a byte window from each end of text.
//
// The budget counts BYTES, not characters or lines, because the inputs are byte
// streams and a byte budget is the only one that behaves identically for ASCII
// and multi-byte text. Cuts are moved back to a rune boundary so the retained
// text is always valid UTF-8 and never carries a replacement character
// introduced by the cut itself.
//
// The returned Omitted count is exact: it is the number of bytes actually
// dropped, including any boundary bytes the rune alignment discarded.
func BoundText(text string, headBytes, tailBytes int) (string, Omitted) {
	if headBytes < 0 {
		headBytes = 0
	}
	if tailBytes < 0 {
		tailBytes = 0
	}

	if len(text) <= headBytes+tailBytes {
		return text, Omitted{Kind: OmittedNone, Unit: "bytes"}
	}

	head := runeSafePrefix(text, headBytes)
	tail := runeSafeSuffix(text, tailBytes)

	out := head + elisionMarker + tail
	dropped := len(text) - len(head) - len(tail)

	return out, Omitted{Kind: OmittedExact, Count: dropped, Unit: "bytes"}
}

// BoundItems keeps the first max items of a slice.
//
// The caller must have observed every item for the count to be exact; that is
// why the omission is reported rather than inferred. Callers that stop early get
// OmittedUnknown, which is the honest answer.
func BoundItems[T any](items []T, max int) ([]T, Omitted) {
	if max < 0 {
		max = 0
	}

	if len(items) <= max {
		return items, Omitted{Kind: OmittedNone, Unit: "items"}
	}

	return items[:max], Omitted{Kind: OmittedExact, Count: len(items) - max, Unit: "items"}
}

// runeSafePrefix returns the longest prefix of s no longer than n bytes that
// ends on a rune boundary.
func runeSafePrefix(s string, n int) string {
	if n >= len(s) {
		return s
	}

	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}

	return s[:n]
}

// runeSafeSuffix returns the longest suffix of s no longer than n bytes that
// starts on a rune boundary.
func runeSafeSuffix(s string, n int) string {
	if n >= len(s) {
		return s
	}

	start := len(s) - n
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}

	return s[start:]
}

// joinNotice appends a retention notice to body when something was dropped.
func joinNotice(body string, omitted Omitted) string {
	notice := omitted.Notice()
	if notice == "" {
		return body
	}

	if strings.TrimSpace(body) == "" {
		return notice
	}

	return body + "\n\n" + notice
}
