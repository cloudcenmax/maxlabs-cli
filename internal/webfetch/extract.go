package webfetch

import (
	"html"
	"strings"
	"unicode"
)

// extract pulls readable text and a title out of a response.
//
// Deliberately not a parser. A full HTML parser is a dependency, and the goal
// here is not fidelity: it is to hand the model the words on the page without
// the markup, with scripts and styles - which are code, not content - removed
// before anything else, so a page whose visible text is short cannot smuggle
// instructions through a hidden block.
func extract(body, contentType string) (title, text string) {
	if !strings.Contains(strings.ToLower(contentType), "html") {
		// Not markup, so it is returned as it stands, minus the whitespace a
		// terminal would collapse anyway.
		return "", collapse(body)
	}

	title = extractTag(body, "title")
	text = stripMarkup(body)

	return title, collapse(text)
}

// stripMarkup removes everything that is not visible text.
func stripMarkup(body string) string {
	var out strings.Builder

	lower := strings.ToLower(body)
	i := 0

	for i < len(lower) {
		open := strings.IndexByte(lower[i:], '<')

		if open < 0 {
			out.WriteString(body[i:])

			break
		}

		out.WriteString(body[i : i+open])

		rest := lower[i+open:]

		// A comment can hold a great deal, and none of it is shown to a reader.
		if strings.HasPrefix(rest, "<!--") {
			if end := strings.Index(rest, "-->"); end >= 0 {
				i += open + end + 3

				continue
			}

			break
		}

		// Script and style are removed with their contents. Leaving the body of
		// a <script> in place would put code into the model's context as prose.
		if name, ok := blockedElement(rest); ok {
			if end := indexOfClose(lower[i+open:], name); end >= 0 {
				i += open + end

				continue
			}

			break
		}

		close := strings.IndexByte(lower[i+open:], '>')
		if close < 0 {
			break
		}

		i += open + close + 1
	}

	return out.String()
}

// blockedElements are removed along with everything inside them.
var blockedElements = []string{"script", "style", "noscript", "template", "svg", "iframe"}

// blockedElement reports whether a tag opens an element whose contents are not
// visible text.
func blockedElement(tag string) (string, bool) {
	for _, name := range blockedElements {
		if strings.HasPrefix(tag, "<"+name) {
			return name, true
		}
	}

	return "", false
}

// indexOfClose finds the end of an element, returning the offset just past it.
func indexOfClose(body, name string) int {
	needle := "</" + name

	idx := strings.Index(body, needle)
	if idx < 0 {
		return -1
	}

	end := strings.IndexByte(body[idx:], '>')
	if end < 0 {
		return -1
	}

	return idx + end + 1
}

// extractTag returns the contents of the first matching tag.
func extractTag(body, name string) string {
	lower := strings.ToLower(body)

	open := strings.Index(lower, "<"+name)
	if open < 0 {
		return ""
	}

	start := strings.IndexByte(lower[open:], '>')
	if start < 0 {
		return ""
	}

	start += open + 1

	end := strings.Index(lower[start:], "</"+name)
	if end < 0 {
		return ""
	}

	return strings.TrimSpace(html.UnescapeString(body[start : start+end]))
}

// collapse reduces whitespace and entities to what a reader would see.
func collapse(text string) string {
	text = html.UnescapeString(text)

	var out strings.Builder

	blank := false

	for _, r := range text {
		if unicode.IsSpace(r) {
			if !blank {
				out.WriteByte(' ')

				blank = true
			}

			continue
		}

		blank = false

		out.WriteRune(r)
	}

	return strings.TrimSpace(out.String())
}
