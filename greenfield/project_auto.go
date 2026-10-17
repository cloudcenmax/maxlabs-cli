package greenfield

import (
	"encoding/json"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"

	"censi/harness/internal/audit"
)

// Shared by the host guard and team workers. Assessment never changes execution.
func ProjectAutoCommand(root, command string) bool {
	assessed, ok := workerAutoCommand(root, command)
	if !ok || outsideCommand.MatchString(assessed) || projectEscalation(assessed) {
		return false
	}
	args, _ := json.Marshal(map[string]string{"command": assessed})
	fresh, _ := audit.RequiresFreshApproval("bash", args)
	return !fresh
}

func projectEscalation(command string) bool {
	parts, ok := workerShellParts(command)
	if !ok {
		return true
	}
	name := ""
	words := []string{}
	check := func() bool {
		if name == "" {
			return false
		}
		n := strings.ToLower(filepath.Base(name))
		if regexp.MustCompile(`(?:^|[-_.])(?:deploy|publish|release)(?:$|[-_.])`).MatchString(n) {
			return true
		}
		if strings.Contains("|aws|gcloud|az|terraform|pulumi|kubectl|docker|vercel|netlify|fly|", "|"+n+"|") {
			if len(words) < 2 || !strings.Contains("|get|describe|logs|version|ps|images|inspect|status|", "|"+words[1]+"|") {
				return true
			}
		}
		for i, w := range words {
			if strings.HasPrefix(w, "--prefix") || strings.HasPrefix(w, "--directory") || strings.HasPrefix(w, "--working-dir") || strings.HasPrefix(w, "--project-dir") {
				return true
			}
			if w == "--host" || strings.HasPrefix(w, "--host=") {
				v := strings.TrimPrefix(w, "--host=")
				if w == "--host" && i+1 < len(words) {
					v = words[i+1]
				}
				if v != "127.0.0.1" && v != "localhost" && v != "::1" {
					return true
				}
			}
			if strings.Contains("|npm|pnpm|yarn|bun|composer|pip|pip3|gem|", "|"+n+"|") {
				if w == "-g" || w == "--global" || strings.HasPrefix(w, "--global=") || w == "--system" {
					return true
				}
				if i == 2 && len(words) > 1 && (words[1] == "run" || words[1] == "run-script") && regexp.MustCompile(`(?i)(?:deploy|publish|release)`).MatchString(w) {
					return true
				}
			}
			if n == "php" && (w == "migrate:fresh" || w == "migrate:reset" || w == "migrate:refresh" || w == "db:wipe" || w == "db:drop" || w == "--force" || w == "--env=production") {
				return true
			}
			if n == "git" && (w == "-D" || w == "--delete") {
				return true
			}
		}
		return false
	}
	for _, p := range parts {
		raw := command[p[0]:p[1]]
		if raw == "|" || raw == ";" || raw == "\n" || raw == "&&" || raw == "||" {
			if check() {
				return true
			}
			name = ""
			words = nil
			continue
		}
		v := workerShellWord(raw)
		if name == "" && regexp.MustCompile(`^[A-Za-z_][A-Za-z_0-9]*=`).MatchString(v) {
			if regexp.MustCompile(`^(PATH|HOME|LD_PRELOAD|LD_LIBRARY_PATH|DYLD_INSERT_LIBRARIES|BASH_ENV|ENV)=`).MatchString(v) {
				return true
			}
			continue
		}
		if name == "" && (v == "env" || v == "command") {
			continue
		}
		if name == "" {
			name = v
		}
		words = append(words, v)
	}
	return check()
}

func localPreviewRequest(words []string) bool {
	if len(words) < 2 || words[0] != "curl" {
		return false
	}
	urls := 0
	for i := 1; i < len(words); i++ {
		value := words[i]
		switch value {
		case "-s", "-S", "-sS", "-fsS", "-f", "-I", "--head", "--silent", "--show-error", "--fail", "--disable", "-q":
			continue
		case "-o", "--output":
			i++
			if i >= len(words) || words[i] != "/dev/null" {
				return false
			}
			continue
		case "-w", "--write-out":
			i++
			if i >= len(words) || len(words[i]) > 256 || strings.Contains(words[i], "%output") {
				return false
			}
			continue
		case "--max-time", "--connect-timeout":
			i++
			if i >= len(words) || !regexp.MustCompile(`^\d+(?:\.\d+)?$`).MatchString(words[i]) {
				return false
			}
			continue
		}
		u, err := url.Parse(value)
		if err != nil || u.Scheme != "http" || u.User != nil || u.Fragment != "" || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost" && u.Hostname() != "::1") {
			return false
		}
		urls++
	}
	return urls == 1
}
