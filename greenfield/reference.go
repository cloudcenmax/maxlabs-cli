package greenfield

import "strings"

// ReferenceNamed matches a literal path in a human-authored turn, never a tool report.
func ReferenceNamed(path, text string) bool {
	if path == "" || path != strings.TrimSpace(path) || path == "/" || path == "." || path == ".." || path == "~" || strings.ContainsAny(path, "\n\r\x00") {
		return false
	}
	if !strings.ContainsAny(path, "/\\") {
		return strings.Contains(text, `"`+path+`"`) || strings.Contains(text, "'"+path+"'") || strings.Contains(text, "`"+path+"`")
	}
	for offset := 0; offset < len(text); {
		index := strings.Index(text[offset:], path)
		if index < 0 {
			break
		}
		index += offset
		end := index + len(path)
		before := index == 0 || strings.ContainsRune(" \t\n\r\"'`(", rune(text[index-1]))
		after := end == len(text) || strings.ContainsRune(" \t\n\r\"'`,;)", rune(text[end]))
		if before && after {
			return true
		}
		offset = index + 1
	}
	return false
}
