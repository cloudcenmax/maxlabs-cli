package greenfield

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var logReadLine = regexp.MustCompile(`^([A-Za-z_][A-Za-z_0-9]*)\s*=\s*open\((['"])([^'"\\\n]+)(['"])\)\.read\(\)$`)
var logFindLine = regexp.MustCompile(`^([A-Za-z_][A-Za-z_0-9]*)\s*=\s*([A-Za-z_][A-Za-z_0-9]*)\.find\((['"])([^'"\\\n]*)(['"])\)$`)

// Recognize only bounded log-reading snippets. Executable Python, imports,
// writes, arbitrary expressions, and non-log paths keep ordinary review.
func workerReadOnlyLogBody(root, body string) bool {
	if len(body) > 4096 {
		return false
	}
	lines := []string{}
	for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, strings.TrimSpace(line))
		}
	}
	if len(lines) != 2 && len(lines) != 3 {
		return false
	}
	m := logReadLine.FindStringSubmatch(lines[0])
	if m == nil || m[2] != m[4] || m[1] == "print" || m[1] == "max" || m[1] == "open" || !workerLogPath(root, m[3]) {
		return false
	}
	data := regexp.QuoteMeta(m[1])
	pattern := `^print\(` + data + `\[-([0-9]{1,4}):\]\)$`
	if len(lines) == 3 {
		f := logFindLine.FindStringSubmatch(lines[1])
		if f == nil || f[2] != m[1] || f[3] != f[5] || f[1] == m[1] || f[1] == "print" || f[1] == "max" || f[1] == "open" {
			return false
		}
		index := regexp.QuoteMeta(f[1])
		pattern = `^print\(` + data + `\[max\(0,\s*` + index + `\s*-\s*([0-9]{1,4})\):\s*` + index + `\s*\+\s*([0-9]{1,4})\]\)$`
	}
	values := regexp.MustCompile(pattern).FindStringSubmatch(lines[len(lines)-1])
	if values == nil {
		return false
	}
	size := 0
	for _, v := range values[1:] {
		n, _ := strconv.Atoi(v)
		size += n
	}
	return size > 0 && size <= 4096
}

func workerLogPath(root, target string) bool {
	if !localWorkerOutput(root, target) {
		return false
	}
	candidate := target
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(root, candidate)
	}
	for _, base := range []string{root, os.TempDir(), "/tmp"} {
		rel, err := filepath.Rel(base, candidate)
		if err != nil || !SafeTaskPath(filepath.ToSlash(rel)) {
			continue
		}
		canonical, err := filepath.EvalSymlinks(base)
		if err != nil {
			continue
		}
		path := canonical
		for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
			path = filepath.Join(path, part)
			info, err := os.Lstat(path)
			if os.IsNotExist(err) {
				break
			}
			if err != nil || info.Mode()&os.ModeSymlink != 0 {
				return false
			}
		}
	}
	return true
}
