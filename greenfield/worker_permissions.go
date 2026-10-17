package greenfield

import (
	"os"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"sort"
	"strings"
)

// A deliberately small lexer: unrecognised shell syntax keeps the ordinary
// approval path. Only checked output destinations are removed for assessment.
var workerPrivatePart = regexp.MustCompile(`(?i)^(?:\.aws|\.ssh|\.npmrc|\.netrc|\.pypirc|\.git-credentials|auth\.json|credentials\.json|service-account\.json)$`)

type workerAssessmentRange struct {
	start, end  int
	replacement string
}

var descriptor = regexp.MustCompile(`^\d+$`)

// workerShellParts accepts literal shell words, including quoted PHP and
// escaped characters. Expansion remains outside this approval shortcut.
func workerShellParts(command string) ([][2]int, bool) {
	var parts [][2]int
	for i := 0; i < len(command); {
		if command[i] == ' ' || command[i] == '\t' {
			i++
			continue
		}
		start := i
		if strings.ContainsRune("|&;>\n", rune(command[i])) {
			i++
			if i < len(command) && (command[start:i+1] == "&&" || command[start:i+1] == "||" || command[start:i+1] == "&>" || command[start:i+1] == ">>" || command[start:i+1] == ">&") {
				i++
			}
			if command[start:i] == "&" {
				return nil, false
			}
		} else {
			var quote byte
			for i < len(command) {
				c := command[i]
				if quote == '\'' {
					if c == quote {
						quote = 0
					}
					i++
					continue
				}
				if c == '\\' {
					if i+1 >= len(command) || command[i+1] == '\n' || command[i+1] == '\r' {
						return nil, false
					}
					i += 2
					continue
				}
				if c == '$' || c == '`' || c == '\r' {
					return nil, false
				}
				if quote != 0 {
					if c == quote {
						quote = 0
					}
					i++
					continue
				}
				if c == '\'' || c == '"' {
					quote = c
					i++
					continue
				}
				if c == '<' || c == '(' || c == ')' || c == '{' || c == '}' || c == '~' || c == '#' {
					return nil, false
				}
				if strings.ContainsRune(" \t\n|&;>", rune(c)) {
					break
				}
				i++
			}
			if quote != 0 {
				return nil, false
			}
		}
		parts = append(parts, [2]int{start, i})
	}
	return parts, true
}

func workerShellWord(raw string) string {
	var quote byte
	var out strings.Builder
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if c == '\\' && quote != '\'' && i+1 < len(raw) {
			next := raw[i+1]
			if quote == 0 || strings.ContainsRune("$`\"\\", rune(next)) {
				out.WriteByte(next)
				i++
				continue
			}
		}
		if quote != 0 {
			if c == quote {
				quote = 0
			} else {
				out.WriteByte(c)
			}
		} else if c == '\'' || c == '"' {
			quote = c
		} else {
			out.WriteByte(c)
		}
	}
	return out.String()
}

// Only cat with a quoted delimiter may supply literal here-document data.
// Its body is a file payload, not shell instructions. All commands before and
// after it still pass the usual assessment; execution keeps the original text.
func workerLiteralHeredocs(command string) (string, bool) {
	masked := []byte(command)
	var quote byte
	for i := 0; i < len(command); i++ {
		c := command[i]
		if quote == '\'' {
			if c == quote {
				quote = 0
			}
			continue
		}
		if c == '\\' {
			i++
			continue
		}
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			continue
		}
		if c == '\'' || c == '"' {
			quote = c
			continue
		}
		if c != '<' {
			continue
		}
		if !strings.HasPrefix(command[i:], "<<") {
			return "", false
		}
		lineStart := strings.LastIndex(command[:i], "\n") + 1
		parts, ok := workerShellParts(command[lineStart:i])
		if !ok {
			return "", false
		}
		name := ""
		for _, part := range parts {
			raw := command[lineStart+part[0] : lineStart+part[1]]
			if raw == ";" || raw == "&&" || raw == "||" || raw == "|" {
				name = ""
			} else if name == "" {
				name = workerShellWord(raw)
			}
		}
		if name != "cat" {
			return "", false
		}
		j := i + 2
		for j < len(command) && (command[j] == ' ' || command[j] == '\t') {
			j++
		}
		if j >= len(command) || (command[j] != '\'' && command[j] != '"') {
			return "", false
		}
		delimiterQuote := command[j]
		j++
		start := j
		for j < len(command) && ((command[j] >= 'a' && command[j] <= 'z') || (command[j] >= 'A' && command[j] <= 'Z') || (command[j] >= '0' && command[j] <= '9') || command[j] == '_') {
			j++
		}
		if j == start || j >= len(command) || command[j] != delimiterQuote {
			return "", false
		}
		delimiter := command[start:j]
		j++
		lineEnd := strings.IndexByte(command[j:], '\n')
		if lineEnd < 0 {
			return "", false
		}
		lineEnd += j
		if strings.Contains(command[j:lineEnd], "<") {
			return "", false
		}
		bodyEnd := lineEnd + 1
		found := false
		for bodyEnd <= len(command) {
			end := strings.IndexByte(command[bodyEnd:], '\n')
			if end < 0 {
				end = len(command)
			} else {
				end += bodyEnd
			}
			if command[bodyEnd:end] == delimiter {
				bodyEnd = end
				found = true
				break
			}
			if end == len(command) {
				break
			}
			bodyEnd = end + 1
		}
		if !found {
			return "", false
		}
		for k := i; k < j; k++ {
			masked[k] = ' '
		}
		for k := lineEnd + 1; k < bodyEnd; k++ {
			masked[k] = ' '
		}
		i = bodyEnd - 1
	}
	return string(masked), true
}

func localWorkerOutput(root, target string) bool {
	if strings.ContainsAny(target, "*?[]{}~") {
		return false
	}
	if target == "/dev/null" || target == "/dev/stdout" || target == "/dev/stderr" || (goruntime.GOOS == "windows" && strings.EqualFold(target, "NUL")) {
		return true
	}
	for _, part := range strings.FieldsFunc(target, func(c rune) bool { return c == '/' || c == '\\' }) {
		if part == ".." || workerPrivatePart.MatchString(part) {
			return false
		}
	}
	candidate := target
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(root, target)
	}
	directories := []string{root, os.TempDir()}
	if goruntime.GOOS != "windows" {
		directories = append(directories, "/tmp")
	}
	for _, directory := range directories {
		if directory != root {
			ext := strings.ToLower(filepath.Ext(candidate))
			if !filepath.IsAbs(target) || (ext != ".log" && ext != ".txt" && ext != ".tap" && ext != ".xml" && ext != ".json") {
				continue
			}
		}
		rel, e := filepath.Rel(directory, candidate)
		if e != nil || !SafeTaskPath(filepath.ToSlash(rel)) {
			continue
		}
		canonical, e := filepath.EvalSymlinks(directory)
		if e != nil {
			continue
		}
		path, e := SafeWorkerPath(canonical, filepath.Join(canonical, rel))
		if e != nil {
			continue
		}
		info, e := os.Stat(path)
		if e == nil && !info.Mode().IsRegular() {
			continue
		}
		if e != nil && !os.IsNotExist(e) {
			continue
		}
		return true
	}
	return false
}
func workerAutoCommand(root, command string) (string, bool) {
	canonical, e := filepath.EvalSymlinks(root)
	if e != nil {
		return "", false
	}
	root = canonical
	var ok bool
	command, ok = workerLiteralHeredocs(command)
	if !ok {
		return "", false
	}
	parts, ok := workerShellParts(command)
	if !ok {
		return "", false
	}

	ranges := []workerAssessmentRange{}
	if len(parts) >= 3 && workerShellWord(command[parts[0][0]:parts[0][1]]) == "cd" && command[parts[2][0]:parts[2][1]] == "&&" {
		canonical, e := filepath.EvalSymlinks(root)
		destination := workerShellWord(command[parts[1][0]:parts[1][1]])
		if !filepath.IsAbs(destination) {
			destination = filepath.Join(root, destination)
		}
		path, e2 := filepath.EvalSymlinks(destination)
		if e == nil && e2 == nil && path == canonical {
			ranges = append(ranges, workerAssessmentRange{parts[0][0], parts[2][1], " "})
		}
	}
	for i := 0; i < len(parts); i++ {
		part := parts[i]
		op := command[part[0]:part[1]]
		if op != ">" && op != ">>" && op != ">&" && op != "&>" {
			continue
		}
		i++
		if i >= len(parts) {
			return "", false
		}
		next := parts[i]
		target := workerShellWord(command[next[0]:next[1]])
		if op == ">&" {
			if target != "1" && target != "2" {
				return "", false
			}
		} else if !localWorkerOutput(root, target) {
			return "", false
		}
		start := part[0]
		if i >= 2 {
			previous := parts[i-2]
			if previous[1] == start && descriptor.MatchString(command[previous[0]:previous[1]]) {
				start = previous[0]
			}
		}
		ranges = append(ranges, workerAssessmentRange{start, next[1], " "})
	}
	// Allow only the literal environment-template copy; never overwrite a
	// previously configured worker environment without an explicit review.
	for i, part := range parts {
		if workerShellWord(command[part[0]:part[1]]) != "cp" {
			continue
		}
		if i > 0 {
			previous := command[parts[i-1][0]:parts[i-1][1]]
			if previous != "&&" && previous != "||" && previous != ";" && previous != "\n" {
				continue
			}
		}
		j := i + 1
		preserve := false
		if j < len(parts) && command[parts[j][0]:parts[j][1]] == "-n" {
			preserve = true
			j++
		}
		if j < len(parts) && command[parts[j][0]:parts[j][1]] == "--" {
			j++
		}
		if j+1 >= len(parts) || workerShellWord(command[parts[j][0]:parts[j][1]]) != ".env.example" || workerShellWord(command[parts[j+1][0]:parts[j+1][1]]) != ".env" {
			continue
		}
		if j+2 < len(parts) {
			next := command[parts[j+2][0]:parts[j+2][1]]
			if next != "&&" && next != "||" && next != ";" && next != "\n" {
				continue
			}
		}
		template, e := SafeWorkerPath(root, ".env.example")
		if e != nil {
			continue
		}
		info, e := os.Stat(template)
		if e != nil || !info.Mode().IsRegular() {
			continue
		}
		info, e = os.Lstat(filepath.Join(root, ".env"))
		if e == nil && (!preserve || !info.Mode().IsRegular()) {
			continue
		}
		if e != nil && !os.IsNotExist(e) {
			continue
		}
		ranges = append(ranges, workerAssessmentRange{part[0], parts[j+1][1], "true"})
	}
	// ls/stat reveal metadata, not the values stored in a private environment.
	metadata := false
	for i, part := range parts {
		raw := command[part[0]:part[1]]
		if raw == "&&" || raw == "||" || raw == ";" || raw == "|" || raw == "\n" {
			metadata = false
			continue
		}
		previous := ""
		if i > 0 {
			previous = command[parts[i-1][0]:parts[i-1][1]]
		}
		commandStart := i == 0 || previous == "&&" || previous == "||" || previous == ";" || previous == "|" || previous == "\n"
		if commandStart {
			name := workerShellWord(raw)
			switch name {
			case "ls", "stat", "test", "[", "/bin/ls", "/usr/bin/ls", "/bin/stat", "/usr/bin/stat", "/bin/test", "/usr/bin/test", "/bin/[", "/usr/bin/[":
				metadata = true
			default:
				metadata = false
			}
		}
		value := workerShellWord(raw)
		covered := false
		for _, r := range ranges {
			if part[0] >= r.start && part[1] <= r.end {
				covered = true
				break
			}
		}
		if covered {
			continue
		}
		if !commandStart && !strings.Contains(value, "=") && filepath.Base(value) == ".env.example" {
			if path, e := SafeWorkerPath(root, value); e == nil && filepath.Base(path) == ".env.example" {
				ranges = append(ranges, workerAssessmentRange{part[0], part[1], "worker-env-template"})
				continue
			}
		}
		if commandStart || !metadata || strings.HasPrefix(value, "-") {
			continue
		}
		if _, e := SafePath(root, value); e == nil {
			ranges = append(ranges, workerAssessmentRange{part[0], part[1], "worker-local-metadata"})
		}
	}
	// Destinations are resolved from the assigned cwd. Any other explicit cwd
	// change needs review, including a relative directory that may be a symlink.
	for i, part := range parts {
		if i > 0 && !strings.Contains("|;\n", command[parts[i-1][0]:parts[i-1][1]]) && command[parts[i-1][0]:parts[i-1][1]] != "&&" && command[parts[i-1][0]:parts[i-1][1]] != "||" {
			continue
		}
		name := workerShellWord(command[part[0]:part[1]])
		if name != "cd" && name != "pushd" && name != "popd" {
			continue
		}
		covered := false
		for _, r := range ranges {
			if part[0] >= r.start && part[1] <= r.end {
				covered = true
				break
			}
		}
		if !covered {
			return "", false
		}
	}

	// The audit must see actual command names and flags, so r\m and 'rm'
	// cannot conceal a deletion behind shell quoting or escaping.
	for _, part := range parts {
		raw := command[part[0]:part[1]]
		value := workerShellWord(raw)
		if raw == value || value == "" || strings.ContainsAny(value, " \t\n|&;<>$`\\\"'(){}*?[]") {
			continue
		}
		covered := false
		for _, r := range ranges {
			if part[0] >= r.start && part[1] <= r.end {
				covered = true
				break
			}
		}
		if !covered {
			ranges = append(ranges, workerAssessmentRange{part[0], part[1], value})
		}
	}

	sort.Slice(ranges, func(i, j int) bool { return ranges[i].start < ranges[j].start })
	assessed := command
	for i := len(ranges) - 1; i >= 0; i-- {
		r := ranges[i]
		assessed = assessed[:r.start] + r.replacement + assessed[r.end:]
	}
	assessed = strings.TrimSpace(assessed)
	return assessed, assessed != ""
}
