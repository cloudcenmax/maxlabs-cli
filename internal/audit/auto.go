package audit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// AutoDecision is what autonomous mode can conclude about a call on its own.
//
// Decision is the same three-way verdict as Assess. Review asks the caller to
// obtain one isolated second opinion before involving a person: it is used for
// calls whose request is plausible but whose effects cannot be established from
// the text alone. A Review is never an approval; it is a question about whether
// an approval is needed at all.
type AutoDecision struct {
	Decision
	Review bool

	// ReviewDecides marks a call whose answer is settled by the isolated
	// review alone. It is used for deletion inside the workspace: the reviewer
	// checks whether the person's request actually covers it, and an
	// inconclusive review refuses the call instead of interrupting the person.
	ReviewDecides bool
}

// WithTask attaches the person's request to the decision, for the explanation.
func (d AutoDecision) WithTask(intent string) AutoDecision {
	d.Decision = d.Decision.WithTask(intent)

	return d
}

func safeAuto(reason string) AutoDecision {
	return AutoDecision{Decision: safe(reason)}
}

func riskyAuto(reason string, review bool) AutoDecision {
	return AutoDecision{Decision: risky(reason), Review: review}
}

func refusedAuto(reason string) AutoDecision {
	return AutoDecision{Decision: refused(reason)}
}

// AssessAuto is the autonomous-mode policy: decide as much as possible without a
// person, and reserve a question for effects that are genuinely outside the
// ordinary shape of work in a project folder.
//
// It is deliberately deterministic. Auto mode used to send nearly every command
// to an isolated model review, which made routine work wait on a second model
// request and still produced a prompt whenever the review was unsure. The
// deterministic rules below cover the ordinary case (reads, builds, tests,
// formatting, workspace edits, local Git work). A model review is only requested
// for calls this layer cannot establish - destructive commands, opaque shell, or
// commands outside the workspace - where an explicit request from the person is
// the thing that should decide.
//
// The floor is unchanged and checks before every allow rule: credentials,
// privileges, system services, publishing, and network egress never become
// ordinary because a rule below is broad.
func AssessAuto(tool string, args json.RawMessage, transcript Transcript, workspace string) AutoDecision {
	intent := userRequest(transcript)

	// Checked before anything else, so no allow rule can reach past it.
	if found := FindCredentials(tool, args); len(found) > 0 {
		return refusedAuto(
			"this call would send " + describe(found) + " to " + tool + ", which can reach the network",
		).WithTask(intent)
	}

	switch tool {
	case "read", "glob", "grep":
		return safeAuto("reads only").WithTask(intent)

	case "write", "edit":
		return assessAutoWrite(tool, args, workspace).WithTask(intent)

	// Fetching a page is a read. Its contents are untrusted, which is a reason
	// to keep them out of trusted instructions, not a reason to interrupt.
	case "webfetch":
		return safeAuto("fetches a page without changing the workspace").WithTask(intent)

	// A subagent's own calls pass through this same guard, so delegating does
	// not widen what can happen.
	case "task":
		return safeAuto("a subagent's calls are judged individually").WithTask(intent)

	case "bash":
		return assessAutoBash(args, workspace).WithTask(intent)

	default:
		return riskyAuto(fmt.Sprintf("%q is not a tool the auto policy knows", tool), false).WithTask(intent)
	}
}

// assessAutoWrite permits workspace edits and treats paths elsewhere as a
// question. Scope, not the word "write", is the risk: an edit inside the folder
// the person chose is the ordinary work of the app.
func assessAutoWrite(tool string, args json.RawMessage, workspace string) AutoDecision {
	var parsed struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(args, &parsed); err != nil {
		return riskyAuto("the arguments could not be read, so the target is unknown", false)
	}

	if strings.TrimSpace(parsed.Path) == "" {
		return riskyAuto("no path was given", false)
	}

	if !pathWithinWorkspace(parsed.Path, workspace) {
		return riskyAuto(fmt.Sprintf("%s would write to %s, outside the assigned workspace", tool, parsed.Path), false)
	}

	return safeAuto(fmt.Sprintf("%s writes inside the workspace", tool))
}

// assessAutoBash judges a shell command for autonomous mode.
func assessAutoBash(args json.RawMessage, workspace string) AutoDecision {
	var parsed struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(args, &parsed); err != nil {
		return riskyAuto("the arguments could not be read, so the command is unknown", true)
	}

	command := strings.TrimSpace(parsed.Command)
	if command == "" {
		return riskyAuto("no command was given", true)
	}

	// Self-concealing or output-redirecting commands are not established by a
	// pattern match, so they get a second opinion before anyone is asked.
	if strings.Contains(command, "$(") || strings.Contains(command, "`") {
		return riskyAuto("runs a command inside another command, which cannot be read reliably", true)
	}

	if hasWriteRedirect(command) {
		return riskyAuto("redirects output to a file", true)
	}

	// The floor does not move with the mode.
	if reason, found := onTheFloor(command); found {
		return riskyAuto(reason, true)
	}

	// Deleting files is the one destructive family auto mode decides for
	// itself: the isolated review is asked whether the request covers the
	// specific paths, and an inconclusive answer refuses the call rather than
	// bothering the person. Anything outside the workspace, an unsafe chain, or
	// the workspace root itself falls back to the ordinary explicit decision.
	if reviewable, reason := deletionReviewable(command, workspace); reviewable {
		return AutoDecision{
			Decision:      risky(reason),
			Review:        true,
			ReviewDecides: true,
		}
	}

	// Commands whose exact text must be reviewed every time even when a family
	// was approved earlier. These are still eligible for one isolated review
	// when the person's request clearly authorizes them.
	if fresh, reason := RequiresFreshApproval("bash", args); fresh && reason != "" {
		return riskyAuto(reason, true)
	}

	// Harmless redirection (descriptor copies and /dev/null) is noise for the
	// purpose of reading the command. File redirects were refused above.
	parts := splitCommands(stripOutputRedirections(command))
	if len(parts) == 0 {
		return riskyAuto("the command could not be read", true)
	}

	var reasons []string

	for _, part := range parts {
		allowed, positivelyRisky, reason := assessAutoPart(part, workspace)
		if allowed {
			reasons = append(reasons, "workspace-scoped "+reason)

			continue
		}

		if positivelyRisky {
			return riskyAuto(reason, true)
		}

		// A recognised read-only command is safe on its own. The check runs
		// after the mutating rules above, so `git branch -D` cannot be rescued by
		// the fact that `git branch` usually reads.
		if decision := assessSimpleCommand(part); decision.Allowed() {
			reasons = append(reasons, decision.Reasons...)

			continue
		}

		if reason == "" {
			reason = "not a command auto mode can establish"
		}

		return riskyAuto(reason, true)
	}

	return safeAuto("auto mode: " + strings.Join(unique(reasons), ", "))
}

var (
	descriptorRedirect = regexp.MustCompile(`\s*\d*>&\d+`)
	nullRedirect       = regexp.MustCompile(`\s*\d*>\s*/dev/null`)
)

// stripOutputRedirections removes the redirections hasWriteRedirect already
// judged harmless, so they do not become phantom command parts.
func stripOutputRedirections(command string) string {
	command = descriptorRedirect.ReplaceAllString(command, "")
	command = nullRedirect.ReplaceAllString(command, "")

	return command
}

// deletionCommands are the commands that remove files or their contents.
var deletionCommands = map[string]bool{
	"rm": true, "rmdir": true, "unlink": true, "shred": true, "truncate": true,
}

// deletionReviewable reports whether a command is a deletion the isolated
// review is allowed to settle on its own.
//
// Every non-deletion part must be ordinary workspace work, every target must
// resolve inside the workspace, and the workspace root itself is never a target.
// A command mixing deletion with an unrelated risky step keeps the explicit
// question, because the reviewer would have to bless both at once.
func deletionReviewable(command, workspace string) (bool, string) {
	parts := splitCommands(command)
	found := false

	for _, part := range parts {
		fields := dropAssignments(stripWrappers(strings.Fields(part)))
		if len(fields) == 0 {
			continue
		}

		name := filepath.Base(fields[0])
		if deletionCommands[name] {
			found = true

			targets, ok := deletionTargets(fields[1:])
			if !ok {
				return false, ""
			}

			for _, target := range targets {
				if !pathWithinWorkspace(target, workspace) {
					return false, ""
				}

				// Deleting the workspace itself is never an ordinary clean-up.
				if target == "." || target == "./" {
					return false, ""
				}

				if workspace != "" {
					if targetPath, err := filepath.Abs(filepath.Join(workspace, target)); err == nil {
						if rootPath, err := filepath.Abs(workspace); err == nil && targetPath == rootPath {
							return false, ""
						}
					}
				}
			}

			continue
		}

		// The rest of the chain must be ordinary, already-permitted work.
		if assessSimpleCommand(part).Allowed() {
			continue
		}

		if allowed, risky, _ := assessAutoPart(part, workspace); allowed && !risky {
			continue
		}

		return false, ""
	}

	if !found {
		return false, ""
	}

	return true, "deletes paths inside the workspace; the isolated review must confirm the request covers them"
}

// deletionTargets reads the paths an rm-family command names. A deletion with
// no readable target is not reviewable on its own.
func deletionTargets(args []string) ([]string, bool) {
	var targets []string
	endFlags := false

	for _, arg := range args {
		if !endFlags && arg == "--" {
			endFlags = true

			continue
		}

		if !endFlags && strings.HasPrefix(arg, "-") {
			continue
		}

		if arg == "" {
			continue
		}

		targets = append(targets, arg)
	}

	return targets, len(targets) > 0
}

// assessAutoPart permits the ordinary mutating commands of project work, but
// only when their effects stay inside the workspace. The second return says the
// part was positively identified as risky (so it must not fall through to the
// read-only rules); the third is the explanation. A part that is neither
// recognised nor positively risky falls through to the read-only classifier.
func assessAutoPart(part, workspace string) (bool, bool, string) {
	fields := dropAssignments(stripWrappers(strings.Fields(part)))
	if len(fields) == 0 {
		return false, true, "empty command"
	}

	rawName := fields[0]
	args := fields[1:]

	// A binary run from a writable location is not the tool it is named after.
	if strings.Contains(rawName, "/") {
		if strings.HasPrefix(rawName, "./") || strings.HasPrefix(rawName, "../") ||
			strings.HasPrefix(rawName, "/tmp/") || strings.HasPrefix(rawName, "/var/") {
			return false, true, fmt.Sprintf("runs %s from a writable location", rawName)
		}

		rawName = filepath.Base(rawName)
	}

	// A command that changes scope to outside the workspace is not workspace
	// scoped, whatever it runs afterwards.
	if rawName == "cd" {
		if len(args) == 0 {
			return false, true, "changes directory with no target"
		}

		if pathWithinWorkspace(args[0], workspace) {
			return true, false, "cd"
		}

		return false, true, fmt.Sprintf("changes directory to %s, outside the workspace", args[0])
	}

	// Anything naming an absolute path outside the workspace is a question,
	// even for an otherwise ordinary command: this is the hallucinated-path
	// case, where the model edits a real file somewhere else.
	if outside := absoluteOutsideWorkspace(args, workspace); outside != "" {
		return false, true, fmt.Sprintf("touches %s, outside the workspace", outside)
	}

	if reason := riskyScriptName(args); reason != "" {
		return false, true, reason
	}

	switch rawName {
	case "mkdir", "touch", "cp":
		return true, false, rawName

	case "go":
		if sub := firstNonFlag(args); oneOf(sub, "build", "test", "vet", "fmt", "tidy", "clean", "generate") {
			return true, false, "go " + sub
		}

	case "cargo":
		if sub := firstNonFlag(args); oneOf(sub, "build", "test", "check", "clippy", "fmt", "doc", "nextest") {
			return true, false, "cargo " + sub
		}

	case "npm", "pnpm", "yarn", "bun":
		if sub := firstNonFlag(args); oneOf(sub, "test", "run", "lint", "build", "typecheck", "format", "check", "exec", "vitest", "jest") {
			return true, false, rawName + " " + sub
		}

	case "npx", "bunx":
		if sub := firstNonFlag(args); oneOf(sub, "tsc", "eslint", "prettier", "vitest", "jest", "vite", "next", "playwright") {
			return true, false, rawName + " " + sub
		}

	case "php":
		if len(args) > 0 && filepath.Base(args[0]) == "artisan" {
			if sub := firstNonFlag(args[1:]); oneOf(sub, "test", "lint", "pint", "analyse") {
				return true, false, "php artisan " + sub
			}
		}

	case "composer":
		if sub := firstNonFlag(args); oneOf(sub, "test", "validate") {
			return true, false, "composer " + sub
		}

	case "git":
		switch sub := firstNonFlag(args); sub {
		case "add", "commit", "fetch", "merge", "rebase":
			return true, false, "git " + sub
		case "branch", "tag":
			// Listing or creating these is ordinary; deleting or moving one is
			// how work disappears.
			if containsAny(args, "-d", "-D", "--delete", "-m", "-M", "--move", "-c", "-C", "--copy", "-f", "--force") {
				return false, true, "git " + sub + " can delete or rewrite local work"
			}
			return true, false, "git " + sub
		case "stash":
			if containsAny(args, "drop", "clear") {
				return false, true, "git stash drop or clear deletes saved work"
			}
			return true, false, "git stash"
		case "switch", "checkout":
			// Creating or switching a branch is ordinary; restoring paths is not.
			if containsAny(args, "-b", "-c", "--create") || !containsAny(args, "--", ".") {
				return true, false, "git " + sub
			}
		}

	case "tsc", "eslint", "prettier", "vitest", "jest", "pytest", "ruff", "black", "mypy", "phpunit", "gofmt", "gofumpt", "rustfmt", "gradle", "mvn":
		return true, false, rawName

	case "python", "python3":
		if len(args) >= 2 && args[0] == "-m" && oneOf(args[1], "pytest", "unittest") {
			return true, false, "python -m " + args[1]
		}

	case "make":
		return false, true, "make can run arbitrary recipes, so it needs review"

	default:
		return false, false, fmt.Sprintf("%q is not a command auto mode runs without review", rawName)
	}

	return false, false, ""
}

// riskyScriptName catches deploy/publish/release-like work even inside a known
// package manager, where the difference between `npm run test` and
// `npm run deploy` is one word.
func riskyScriptName(args []string) string {
	for _, arg := range args {
		lower := strings.ToLower(arg)
		switch {
		case lower == "deploy" || lower == "publish" || lower == "release":
			return fmt.Sprintf("%q is a publishing action", arg)
		case strings.HasPrefix(lower, "deploy:") || strings.HasPrefix(lower, "release:"):
			return fmt.Sprintf("%q is a publishing action", arg)
		}
	}

	return ""
}

// absoluteOutsideWorkspace names the first argument that is an absolute path
// outside the workspace. URLs and package names are left alone.
func absoluteOutsideWorkspace(args []string, workspace string) string {
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") || strings.Contains(arg, "://") {
			continue
		}

		isPath := filepath.IsAbs(arg) || strings.HasPrefix(arg, "~/") ||
			(strings.Contains(arg, "/") && !strings.Contains(arg, "="))
		if !isPath {
			continue
		}

		if !pathWithinWorkspace(arg, workspace) {
			return arg
		}
	}

	return ""
}

func oneOf(value string, candidates ...string) bool {
	for _, candidate := range candidates {
		if value == candidate {
			return true
		}
	}

	return false
}

// resolveExistingPrefix resolves symlinks in the longest existing prefix of a
// path. A path that does not exist yet can still sit under a symlinked directory
// (on macOS /var is a link to /private/var), and comparing an unresolved target
// against a resolved root would call an ordinary workspace edit "outside".
func resolveExistingPrefix(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}

	dir := filepath.Dir(path)
	if dir == path {
		return path
	}

	return filepath.Join(resolveExistingPrefix(dir), filepath.Base(path))
}

// pathWithinWorkspace reports whether a path resolves inside the workspace.
//
// Relative paths are resolved against the workspace because that is the tool's
// working directory. Symlinks are followed when the path exists, so a link
// pointing out of the project is not mistaken for a project file. A path that
// cannot be resolved is treated as outside: the question is cheaper than the
// mistake.
func pathWithinWorkspace(path, workspace string) bool {
	path = strings.TrimSpace(path)
	if path == "" {
		return false
	}

	expanded := path

	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			expanded = filepath.Join(home, strings.TrimPrefix(path, "~/"))
		}
	}

	if !filepath.IsAbs(expanded) {
		if workspace == "" {
			// A relative path is relative to the workspace the tool already
			// opened; without one there is nothing to compare against.
			return true
		}

		expanded = filepath.Join(workspace, expanded)
	}

	if workspace == "" {
		return false
	}

	root, err := filepath.Abs(workspace)
	if err != nil {
		return false
	}

	target, err := filepath.Abs(expanded)
	if err != nil {
		return false
	}

	root = resolveExistingPrefix(root)
	target = resolveExistingPrefix(target)

	relative, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}

	return relative == "." ||
		(relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}
