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
var workerShellTokens = regexp.MustCompile(`(?:"[^"\\]*"|'[^'\\]*'|[^\s'"|&;<>\\])+|&&|\|\||&>|>>|>&|[|;>\n]`)
var workerPrivatePart = regexp.MustCompile(`(?i)^(?:\.aws|\.ssh|\.npmrc|\.netrc|\.pypirc|\.git-credentials|auth\.json|credentials\.json|service-account\.json)$`)

type workerAssessmentRange struct {
	start, end  int
	replacement string
}

var descriptor = regexp.MustCompile(`^\d+$`)

func workerShellWord(raw string) string {
	var quote rune
	var out strings.Builder
	for _, c := range raw {
		if quote != 0 {
			if c == quote {
				quote = 0
			} else {
				out.WriteRune(c)
			}
		} else if c == '\'' || c == '"' {
			quote = c
		} else {
			out.WriteRune(c)
		}
	}
	return out.String()
}
func localWorkerOutput(root, target string) bool {
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
	if strings.ContainsAny(command, "$`\\\r<") {
		return "", false
	}
	parts := workerShellTokens.FindAllStringIndex(command, -1)
	consumed := 0
	for _, part := range parts {
		if strings.TrimSpace(command[consumed:part[0]]) != "" {
			return "", false
		}
		consumed = part[1]
	}
	if strings.TrimSpace(command[consumed:]) != "" {
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
	sort.Slice(ranges, func(i, j int) bool { return ranges[i].start < ranges[j].start })
	assessed := command
	for i := len(ranges) - 1; i >= 0; i-- {
		r := ranges[i]
		assessed = assessed[:r.start] + r.replacement + assessed[r.end:]
	}
	assessed = strings.TrimSpace(assessed)
	return assessed, assessed != ""
}
