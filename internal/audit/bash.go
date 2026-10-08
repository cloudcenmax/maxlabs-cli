package audit

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

// readOnlyCommands may run unattended.
//
// Membership is a claim that the command does not modify the filesystem, the
// network, or the process environment. It is not a claim that every invocation
// is useful - `cat` on a huge file is unhelpful, not dangerous.
//
// Anything absent from this set is risky. That is the fail-closed direction: a
// command added to a system later is unknown here, and unknown asks.
var readOnlyCommands = map[string]bool{
	// Filesystem inspection
	"ls": true, "cat": true, "head": true, "tail": true, "wc": true,
	"file": true, "stat": true, "tree": true, "du": true, "df": true,
	"realpath": true, "basename": true, "dirname": true, "readlink": true,
	"pwd": true, "find": true, "fd": true,

	// Text processing
	"grep": true, "rg": true, "ag": true, "sort": true, "uniq": true,
	"cut": true, "tr": true, "sed": true, "awk": true, "jq": true,
	"diff": true, "comm": true, "column": true, "nl": true, "rev": true,
	"tac": true, "fold": true, "expand": true, "unexpand": true,

	// Output with no side effect
	"echo": true, "printf": true, "date": true, "true": true, "false": true,

	// Identity and environment inspection
	"whoami": true, "id": true, "uname": true, "hostname": true,
	"which": true, "type": true, "command": true, "env": true, "printenv": true,

	// Version and help, which are how a person checks a tool exists
	"man": true, "help": true,

	// Waiting is harmless
	"sleep": true,
}

// readOnlySubcommands are subcommands of otherwise-mutating commands.
//
// These are checked rather than the command alone, because `git log` and
// `git push` are not the same request. An entry here means the subcommand reads;
// the command it belongs to is risky by default.
var readOnlySubcommands = map[string]map[string]bool{
	"git": {
		"status": true, "diff": true, "log": true, "show": true,
		"branch": true, "remote": true, "tag": true, "describe": true,
		"blame": true, "shortlog": true, "rev-parse": true, "ls-files": true,
		"cat-file": true, "config": false, "stash": false, "worktree": false,
	},
	"go": {
		"build": true, "test": true, "vet": true, "list": true,
		"fmt": false, "env": true, "doc": true, "version": true,
		"mod": false, "get": false, "install": false, "run": false,
	},
	"npm":     {"test": true, "ls": true, "list": true, "view": true, "outdated": true},
	"php":     {"--version": true, "-v": true, "-r": false},
	"node":    {"--version": true, "-v": true, "-p": false, "-e": false},
	"docker":  {"ps": true, "images": true, "version": true, "logs": true, "inspect": true},
	"kubectl": {"get": true, "describe": true, "logs": true, "version": true},
}

// dangerousFlags make an otherwise read-only command mutate.
//
// This is why the command name alone is not enough. `sed` reads a file; `sed -i`
// rewrites it. Approving the first must not approve the second.
var dangerousFlags = map[string][]string{
	"find":   {"-exec", "-execdir", "-delete", "-ok", "-okdir", "-fprint", "-fprint0", "-fls"},
	"sed":    {"-i", "--in-place"},
	"awk":    {"-i", "--in-place"},
	"sort":   {"-o", "--output"},
	"uniq":   {},
	"tail":   {"-f", "--follow"},       // not a mutation, but it never returns
	"rg":     {"--files-with-matches"}, // harmless, kept for symmetry
	"grep":   {"-r", "--recursive"},    // reads a tree; still reads
	"ls":     {},
	"go":     {},
	"git":    {},
	"docker": {},
}

// freshApprovalCommands are too broad or destructive for a remembered
// session-wide Bash approval. They can still run after the person reviews the
// exact command, but that decision is never reused for a later invocation.
var freshApprovalCommands = map[string]string{
	"rm":         "deletes files",
	"rmdir":      "deletes directories",
	"unlink":     "deletes a file",
	"shred":      "destroys file contents",
	"truncate":   "destroys file contents",
	"mv":         "moves or overwrites files",
	"tee":        "writes to files",
	"xargs":      "constructs and runs another command",
	"parallel":   "constructs and runs other commands",
	"osascript":  "runs automation code with access to desktop applications",
	"powershell": "runs a script whose effects are not bounded by this command line",
	"pwsh":       "runs a script whose effects are not bounded by this command line",
	"bash":       "runs a script whose effects are not bounded by this command line",
	"dash":       "runs a script whose effects are not bounded by this command line",
	"fish":       "runs a script whose effects are not bounded by this command line",
	"sh":         "runs a script whose effects are not bounded by this command line",
	"zsh":        "runs a script whose effects are not bounded by this command line",
}

// RequiresFreshApproval reports whether a call must bypass any remembered
// "always allow Bash" decision. This is deliberately narrower than Assess:
// ordinary project commands such as `php artisan test` may benefit from a
// session approval, while commands that hide work or can remove data must be
// reviewed every time.
func RequiresFreshApproval(tool string, args json.RawMessage) (bool, string) {
	if tool != "bash" {
		return false, ""
	}

	var parsed struct {
		Command string `json:"command"`
	}

	if err := json.Unmarshal(args, &parsed); err != nil {
		return true, "the Bash arguments could not be read"
	}

	command := strings.TrimSpace(parsed.Command)
	if command == "" {
		return true, "the Bash command is empty"
	}

	if strings.Contains(command, "$(") || strings.Contains(command, "`") {
		return true, "the command runs hidden command substitution"
	}

	if hasWriteRedirect(command) {
		return true, "the command redirects output to a file"
	}

	if reason, found := onTheFloor(command); found {
		return true, reason
	}

	for _, part := range splitCommands(command) {
		fields := dropAssignments(stripWrappers(strings.Fields(part)))
		if len(fields) == 0 {
			continue
		}

		name := filepath.Base(fields[0])
		if reason, found := freshApprovalCommands[name]; found {
			return true, name + " " + reason
		}

		if name == "find" && containsAny(fields[1:], "-delete", "-exec", "-execdir", "-ok", "-okdir") {
			return true, "find can delete files or execute another command"
		}

		if name == "git" && containsAny(fields[1:], "clean", "reset", "push", "restore", "checkout") {
			return true, "the Git operation can discard work or publish changes"
		}

		if (name == "python" || name == "python3") && containsAny(fields[1:], "-c") {
			return true, name + " executes inline code"
		}

		if name == "node" && containsAny(fields[1:], "-e", "--eval") {
			return true, "node executes inline code"
		}

		if (name == "perl" || name == "ruby") && containsAny(fields[1:], "-e") {
			return true, name + " executes inline code"
		}

		if name == "php" && containsAny(fields[1:], "-r") {
			return true, "php executes inline code"
		}
	}

	return false, ""
}

// ApprovalScope limits a remembered Bash approval to the command family the
// person actually reviewed. Approving `php artisan test` therefore covers
// later Artisan work, but cannot silently authorize `rm`, `osascript`, or an
// unrelated executable added later in the turn.
func ApprovalScope(tool string, args json.RawMessage) string {
	if tool != "bash" {
		return tool
	}

	var parsed struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(args, &parsed); err != nil {
		return "bash:unreadable"
	}

	var families []string
	for _, part := range splitCommands(strings.TrimSpace(parsed.Command)) {
		fields := dropAssignments(stripWrappers(strings.Fields(part)))
		if len(fields) == 0 {
			continue
		}

		name := filepath.Base(fields[0])
		family := name

		switch name {
		case "php":
			if len(fields) > 1 && filepath.Base(fields[1]) == "artisan" {
				family = "php artisan"
				if len(fields) > 2 && !strings.HasPrefix(fields[2], "-") {
					family += " " + fields[2]
				}
			} else if script := firstNonFlag(fields[1:]); script != "" {
				family += " " + script
			}
		case "node", "perl", "python", "python3", "ruby":
			if script := firstNonFlag(fields[1:]); script != "" {
				family += " " + script
			}
		case "cargo", "composer", "docker", "git", "go", "kubectl", "make", "npm", "pnpm", "yarn":
			if subcommand := firstNonFlag(fields[1:]); subcommand != "" {
				family += " " + subcommand
				if (name == "npm" || name == "pnpm" || name == "yarn") && subcommand == "run" {
					if script := firstNonFlagAfter(fields[1:], "run"); script != "" {
						family += " " + script
					}
				}
			}
		}

		families = append(families, family)
	}

	if len(families) == 0 {
		return "bash:empty"
	}

	return "bash:" + strings.Join(families, "+")
}

func firstNonFlag(values []string) string {
	for _, value := range values {
		if !strings.HasPrefix(value, "-") {
			return filepath.Base(value)
		}
	}

	return ""
}

func firstNonFlagAfter(values []string, after string) string {
	found := false
	for _, value := range values {
		if !found {
			found = value == after
			continue
		}
		if !strings.HasPrefix(value, "-") {
			return filepath.Base(value)
		}
	}

	return ""
}

func containsAny(values []string, candidates ...string) bool {
	for _, value := range values {
		for _, candidate := range candidates {
			if value == candidate || strings.HasPrefix(value, candidate+"=") {
				return true
			}
		}
	}

	return false
}

// assessBash judges a shell command.
func assessBash(args json.RawMessage) Decision {
	var parsed struct {
		Command string `json:"command"`
	}

	if err := json.Unmarshal(args, &parsed); err != nil {
		return risky("the arguments could not be read, so the command is unknown")
	}

	command := strings.TrimSpace(parsed.Command)

	if command == "" {
		return risky("no command was given")
	}

	return assessCommand(command)
}

// assessCommand judges one shell command, which may be several joined together.
func assessCommand(command string) Decision {
	// A command that hides part of itself is judged on what is hidden. Command
	// substitution and backticks run a nested command whose text may not appear
	// in the parts split below.
	if strings.Contains(command, "$(") || strings.Contains(command, "`") {
		return risky("runs a command inside another command, which cannot be read reliably")
	}

	// A redirect writes somewhere. Even `> /dev/null` is a file operation, and
	// telling the harmless redirects from the harmful ones is not worth the risk
	// of being wrong.
	if hasWriteRedirect(command) {
		return risky("redirects output to a file")
	}

	// The floor is checked first and on the whole command, so no allow rule
	// below can reach past it. This is what makes the mode safe to offer: the
	// rules that auto-approve can be extended without extending these.
	if reason, found := onTheFloor(command); found {
		return risky(reason)
	}

	parts := splitCommands(command)

	if len(parts) == 0 {
		return risky("the command could not be read")
	}

	// Every part must be safe. A chain is only as safe as its worst link, and a
	// person reading `ls && rm -rf /` should not have it approved by the `ls`.
	var reasons []string

	for _, part := range parts {
		verdict := assessSimpleCommand(part)

		if !verdict.Allowed() {
			return risky(verdict.Explain())
		}

		reasons = append(reasons, verdict.Reasons...)
	}

	joined := strings.Join(unique(reasons), ", ")

	if len(parts) > 1 {
		return safe("read-only: " + joined)
	}

	return safe(joined)
}

// assessSimpleCommand judges a single command with no operators.
func assessSimpleCommand(command string) Decision {
	fields := stripWrappers(strings.Fields(command))

	if len(fields) == 0 {
		return risky("empty command")
	}

	// A leading environment assignment is not part of the command name.
	fields = dropAssignments(fields)

	if len(fields) == 0 {
		return risky("only environment assignments, no command")
	}

	name := fields[0]

	// A path-qualified command is judged by its base name only when the path is
	// a system location. `./deploy.sh` is not `ls` because it ends in `l`.
	base := name
	if strings.Contains(name, "/") {
		if strings.HasPrefix(name, "./") || strings.HasPrefix(name, "../") ||
			strings.HasPrefix(name, "/tmp/") || strings.HasPrefix(name, "/var/") {
			return risky(fmt.Sprintf("runs %s from a writable location", name))
		}

		if idx := strings.LastIndex(name, "/"); idx >= 0 {
			base = name[idx+1:]
		}
	}

	if subcommands, ok := readOnlySubcommands[base]; ok {
		return assessSubcommand(base, fields[1:], subcommands)
	}

	if !readOnlyCommands[base] {
		return risky(fmt.Sprintf("%q is not a recognised read-only command", base))
	}

	for _, flag := range dangerousFlags[base] {
		for _, field := range fields[1:] {
			if field == flag || strings.HasPrefix(field, flag+"=") {
				return risky(fmt.Sprintf("%s %s changes files", base, flag))
			}
		}
	}

	return safe(base)
}

// assessSubcommand judges a command that is read-only in some subcommands only.
func assessSubcommand(name string, args []string, allowed map[string]bool) Decision {
	for _, arg := range args {
		// Global flags precede the subcommand.
		if strings.HasPrefix(arg, "-") {
			continue
		}

		permitted, known := allowed[arg]

		if !known {
			return risky(fmt.Sprintf("%s %s is not a recognised read-only subcommand", name, arg))
		}

		if !permitted {
			return risky(fmt.Sprintf("%s %s changes things", name, arg))
		}

		return safe(fmt.Sprintf("%s %s reads", name, arg))
	}

	// A bare `git` with no subcommand is harmless but says nothing useful.
	return risky(fmt.Sprintf("%s with no subcommand does nothing useful", name))
}

// splitCommands breaks a command line into its simple commands.
//
// Quotes are respected, so a semicolon inside a string does not split anything.
// The split is deliberately simple: a shell has more syntax than this, and what
// is not understood here becomes an unrecognised command and asks.
func splitCommands(command string) []string {
	var (
		parts   []string
		current strings.Builder
		quote   rune
	)

	flush := func() {
		if text := strings.TrimSpace(current.String()); text != "" {
			parts = append(parts, text)
		}

		current.Reset()
	}

	runes := []rune(command)

	for i := 0; i < len(runes); i++ {
		char := runes[i]

		if quote != 0 {
			current.WriteRune(char)

			if char == quote {
				quote = 0
			}

			continue
		}

		switch char {
		case '\'', '"':
			quote = char
			current.WriteRune(char)

		case ';', '\n':
			flush()

		case '&', '|':
			// Both operators, including `&&` and `||`, and including the single
			// forms which background and pipe.
			flush()

			if i+1 < len(runes) && (runes[i+1] == char) {
				i++
			}

		case '(':
			// A subshell boundary. Splitting at it would lose the grouping, so
			// the whole command is treated as unreadable instead.
			flush()

		case ')':
			flush()

		default:
			current.WriteRune(char)
		}
	}

	flush()

	return parts
}

// hasWriteRedirect reports whether a command sends output to a file.
//
// `>` and `>>` write. `<` reads, and is left alone. The check ignores those
// inside quotes, because `echo "a > b"` writes nothing.
func hasWriteRedirect(command string) bool {
	var quote rune

	runes := []rune(command)

	for i := 0; i < len(runes); i++ {
		char := runes[i]

		if quote != 0 {
			if char == quote {
				quote = 0
			}

			continue
		}

		if char == '\'' || char == '"' {
			quote = char

			continue
		}

		if char == '>' {
			// `>&2` and `> /dev/null` are still redirects; 2>&1 is a descriptor
			// copy, which writes nowhere new.
			return true
		}
	}

	return false
}

// unique removes duplicates while keeping the order they appeared in.
func unique(values []string) []string {
	seen := make(map[string]bool, len(values))

	out := values[:0]

	for _, value := range values {
		if seen[value] {
			continue
		}

		seen[value] = true

		out = append(out, value)
	}

	return out
}
