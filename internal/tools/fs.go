package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"censi/harness/internal/diff"
	"censi/harness/internal/session"
)

// Default budgets. Every tool result is bounded, because an unbounded result
// becomes unbounded history, and history is re-sent on every step. A single
// careless `cat` of a large file would otherwise be re-billed on every turn
// until compaction shadows it.
const (
	defaultReadBytes  = 32 * 1024
	defaultListItems  = 200
	defaultGrepHits   = 200
	truncatedReadTail = 4 * 1024
)

// ReadTool reads a file from the workspace.
type ReadTool struct{ ws *Workspace }

// NewReadTool returns a read tool bound to a workspace.
func NewReadTool(ws *Workspace) *ReadTool { return &ReadTool{ws: ws} }

// Definition implements Tool.
func (t *ReadTool) Definition() Definition {
	return Definition{
		Name: "read",
		Description: "Read a file from the workspace. Returns the file with line numbers. " +
			"Use offset and limit to page through a large file.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"path": {"type": "string", "description": "File path relative to the workspace root."},
				"offset": {"type": "integer", "description": "First line to return, 1-based."},
				"limit": {"type": "integer", "description": "Maximum number of lines to return."}
			},
			"required": ["path"]
		}`),
		ReadOnly: true,
	}
}

type readArgs struct {
	Path   string `json:"path"`
	Offset int    `json:"offset"`
	Limit  int    `json:"limit"`
}

// Execute implements Tool.
func (t *ReadTool) Execute(_ context.Context, raw json.RawMessage) (Result, error) {
	var args readArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return Error("read: the arguments were not valid JSON: " + err.Error()), nil
	}

	path, err := t.ws.Resolve(args.Path)
	if err != nil {
		return Error("read: " + err.Error()), nil
	}

	info, err := os.Stat(path)
	if err != nil {
		return Error(fmt.Sprintf("read: %s does not exist.", t.ws.Display(path))), nil
	}
	if info.IsDir() {
		return Error(fmt.Sprintf("read: %s is a directory; use glob to list it.", t.ws.Display(path))), nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return Error("read: " + err.Error()), nil
	}

	lines := strings.Split(string(data), "\n")

	// Offset is 1-based and clamped rather than rejected: a model that asks for
	// line 1 of a 3-line file wants the file, not a lecture.
	start := 0
	if args.Offset > 1 {
		start = args.Offset - 1
	}
	if start > len(lines) {
		start = len(lines)
	}

	end := len(lines)
	if args.Limit > 0 && start+args.Limit < end {
		end = start + args.Limit
	}

	var b strings.Builder
	for i := start; i < end; i++ {
		fmt.Fprintf(&b, "%d\t%s\n", i+1, lines[i])
	}

	text, omitted := BoundText(b.String(), defaultReadBytes, truncatedReadTail)

	return Result{
		Content: []session.Block{session.Text(joinNotice(text, omitted))},
		Omitted: omitted,
	}, nil
}

// WriteTool writes a file in the workspace.
type WriteTool struct{ ws *Workspace }

// NewWriteTool returns a write tool bound to a workspace.
func NewWriteTool(ws *Workspace) *WriteTool { return &WriteTool{ws: ws} }

// Definition implements Tool.
func (t *WriteTool) Definition() Definition {
	return Definition{
		Name: "write",
		Description: "Write a file, creating it or replacing its whole contents. " +
			"Parent directories are created as needed.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"path": {"type": "string", "description": "File path relative to the workspace root."},
				"content": {"type": "string", "description": "The complete file contents."}
			},
			"required": ["path", "content"]
		}`),
	}
}

type writeArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// Execute implements Tool.
func (t *WriteTool) Execute(_ context.Context, raw json.RawMessage) (Result, error) {
	var args writeArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return Error("write: the arguments were not valid JSON: " + err.Error()), nil
	}

	path, err := t.ws.Resolve(args.Path)
	if err != nil {
		return Error("write: " + err.Error()), nil
	}

	// Read the previous contents first: after the write they are gone, and a
	// diff needs both sides.
	previous, _ := os.ReadFile(path)

	if err := t.ws.writeFile(path, []byte(args.Content)); err != nil {
		return Error("write: " + err.Error()), nil
	}

	display := t.ws.Display(path)

	result := Text(fmt.Sprintf("Wrote %s (%d bytes).", display, len(args.Content)))
	result.Diff = diff.Lines(string(previous), args.Content)
	result.DiffTarget = display

	return result, nil
}

// EditTool replaces an exact string in a file.
type EditTool struct{ ws *Workspace }

// NewEditTool returns an edit tool bound to a workspace.
func NewEditTool(ws *Workspace) *EditTool { return &EditTool{ws: ws} }

// Definition implements Tool.
func (t *EditTool) Definition() Definition {
	return Definition{
		Name: "edit",
		Description: "Replace an exact string in a file. The old string must appear exactly once, " +
			"so an ambiguous edit fails rather than guessing. Read the file first.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"path": {"type": "string", "description": "File path relative to the workspace root."},
				"old": {"type": "string", "description": "The exact text to replace."},
				"new": {"type": "string", "description": "The replacement text."}
			},
			"required": ["path", "old", "new"]
		}`),
	}
}

type editArgs struct {
	Path string `json:"path"`
	Old  string `json:"old"`
	New  string `json:"new"`
}

// Execute implements Tool.
func (t *EditTool) Execute(_ context.Context, raw json.RawMessage) (Result, error) {
	var args editArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return Error("edit: the arguments were not valid JSON: " + err.Error()), nil
	}

	if args.Old == "" {
		return Error("edit: the old string must not be empty; use write to create a file."), nil
	}

	path, err := t.ws.Resolve(args.Path)
	if err != nil {
		return Error("edit: " + err.Error()), nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return Error(fmt.Sprintf("edit: cannot read %s.", t.ws.Display(path))), nil
	}

	text := string(data)
	count := strings.Count(text, args.Old)

	switch {
	case count == 0:
		return Error(fmt.Sprintf("edit: the old string does not appear in %s.", t.ws.Display(path))), nil
	case count > 1:
		// Guessing which occurrence was meant is how a model silently corrupts a
		// file. Refusing is the safe failure.
		return Error(fmt.Sprintf(
			"edit: the old string appears %d times in %s; it must appear exactly once. "+
				"Include more surrounding context.", count, t.ws.Display(path))), nil
	}

	updated := strings.Replace(text, args.Old, args.New, 1)

	if err := t.ws.writeFile(path, []byte(updated)); err != nil {
		return Error("edit: " + err.Error()), nil
	}

	result := Text(fmt.Sprintf("Edited %s.", t.ws.Display(path)))
	result.Diff = diff.Lines(text, updated)
	result.DiffTarget = t.ws.Display(path)

	return result, nil
}

// GlobTool lists files matching a pattern.
type GlobTool struct{ ws *Workspace }

// NewGlobTool returns a glob tool bound to a workspace.
func NewGlobTool(ws *Workspace) *GlobTool { return &GlobTool{ws: ws} }

// Definition implements Tool.
func (t *GlobTool) Definition() Definition {
	return Definition{
		Name: "glob",
		Description: "Find files by path pattern. Supports ** to match across directories, " +
			"for example **/*.go. Results are sorted and relative to the workspace root.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"pattern": {"type": "string", "description": "Glob pattern, for example **/*.go."}
			},
			"required": ["pattern"]
		}`),
		ReadOnly: true,
	}
}

type globArgs struct {
	Pattern string `json:"pattern"`
}

// Execute implements Tool.
func (t *GlobTool) Execute(_ context.Context, raw json.RawMessage) (Result, error) {
	var args globArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return Error("glob: the arguments were not valid JSON: " + err.Error()), nil
	}

	if strings.TrimSpace(args.Pattern) == "" {
		return Error("glob: a pattern is required."), nil
	}

	var matches []string
	err := filepath.WalkDir(t.ws.Root(), func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil // an unreadable subtree is skipped, not fatal
		}

		rel := t.ws.Display(path)
		if rel == "." {
			return nil
		}

		if entry.IsDir() {
			if entry.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}

		if matchGlob(args.Pattern, rel) {
			matches = append(matches, rel)
		}

		return nil
	})
	if err != nil {
		return Error("glob: " + err.Error()), nil
	}

	sort.Strings(matches)
	matches, omitted := BoundItems(matches, defaultListItems)

	if len(matches) == 0 {
		return Text(fmt.Sprintf("No files match %q.", args.Pattern)), nil
	}

	body := strings.Join(matches, "\n")

	return Result{
		Content: []session.Block{session.Text(joinNotice(body, omitted))},
		Omitted: omitted,
	}, nil
}

// GrepTool searches file contents.
type GrepTool struct{ ws *Workspace }

// NewGrepTool returns a grep tool bound to a workspace.
func NewGrepTool(ws *Workspace) *GrepTool { return &GrepTool{ws: ws} }

// Definition implements Tool.
func (t *GrepTool) Definition() Definition {
	return Definition{
		Name: "grep",
		Description: "Search file contents with a regular expression. Returns path:line:text " +
			"for each match, up to a bounded number of hits.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"pattern": {"type": "string", "description": "Regular expression to search for."},
				"path": {"type": "string", "description": "Optional subdirectory or file to search."},
				"ignore_case": {"type": "boolean", "description": "Match case-insensitively."}
			},
			"required": ["pattern"]
		}`),
		ReadOnly: true,
	}
}

type grepArgs struct {
	Pattern    string `json:"pattern"`
	Path       string `json:"path"`
	IgnoreCase bool   `json:"ignore_case"`
}

// Execute implements Tool.
func (t *GrepTool) Execute(_ context.Context, raw json.RawMessage) (Result, error) {
	var args grepArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return Error("grep: the arguments were not valid JSON: " + err.Error()), nil
	}

	pattern := args.Pattern
	if args.IgnoreCase {
		pattern = "(?i)" + pattern
	}

	re, err := regexp.Compile(pattern)
	if err != nil {
		return Error("grep: invalid regular expression: " + err.Error()), nil
	}

	root, err := t.ws.Resolve(args.Path)
	if err != nil {
		return Error("grep: " + err.Error()), nil
	}

	var hits []string
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			if entry.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if len(hits) >= defaultGrepHits {
			return filepath.SkipAll
		}

		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		if isBinary(data) {
			return nil
		}

		display := t.ws.Display(path)
		for i, line := range strings.Split(string(data), "\n") {
			if re.MatchString(line) {
				hits = append(hits, fmt.Sprintf("%s:%d:%s", display, i+1, line))
				if len(hits) >= defaultGrepHits {
					break
				}
			}
		}

		return nil
	})
	if err != nil {
		return Error("grep: " + err.Error()), nil
	}

	if len(hits) == 0 {
		return Text(fmt.Sprintf("No matches for %q.", args.Pattern)), nil
	}

	body := strings.Join(hits, "\n")

	// The walk stops at the cap, so the total is genuinely unknown. Reporting it
	// as exact would be a lie the model would act on.
	omitted := Omitted{Kind: OmittedNone, Unit: "hits"}
	if len(hits) >= defaultGrepHits {
		omitted = Omitted{Kind: OmittedUnknown, Unit: "hits"}
	}

	return Result{
		Content: []session.Block{session.Text(joinNotice(body, omitted))},
		Omitted: omitted,
	}, nil
}

// isBinary reports whether data looks binary, using the same heuristic as most
// search tools: a NUL byte in the first block.
func isBinary(data []byte) bool {
	limit := len(data)
	if limit > 8000 {
		limit = 8000
	}

	for i := 0; i < limit; i++ {
		if data[i] == 0 {
			return true
		}
	}

	return false
}

// matchGlob matches a slash-separated pattern against a slash-separated path,
// supporting *, ? and ** (which matches zero or more path segments).
//
// Neither filepath.Match nor path.Match supports **, and ** is what models
// actually type, so it is implemented here rather than left as a surprise.
func matchGlob(pattern, name string) bool {
	return matchSegments(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func matchSegments(pattern, name []string) bool {
	for len(pattern) > 0 {
		switch pattern[0] {
		case "**":
			// ** matches zero or more segments: try every split point.
			if len(pattern) == 1 {
				return true
			}
			for i := 0; i <= len(name); i++ {
				if matchSegments(pattern[1:], name[i:]) {
					return true
				}
			}
			return false
		default:
			if len(name) == 0 {
				return false
			}
			ok, err := filepath.Match(pattern[0], name[0])
			if err != nil || !ok {
				return false
			}
			pattern = pattern[1:]
			name = name[1:]
		}
	}

	return len(name) == 0
}
