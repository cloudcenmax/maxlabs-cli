package tools_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"censi/harness/internal/tools"
)

func workspace(t *testing.T) (*tools.Workspace, string) {
	t.Helper()

	root := t.TempDir()
	ws, err := tools.NewWorkspace(root)
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}

	return ws, root
}

func run(t *testing.T, tool interface {
	Execute(context.Context, json.RawMessage) (tools.Result, error)
}, args string) tools.Result {
	t.Helper()

	result, err := tool.Execute(context.Background(), json.RawMessage(args))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	return result
}

func text(result tools.Result) string {
	parts := make([]string, 0, len(result.Content))
	for _, block := range result.Content {
		parts = append(parts, block.Text)
	}
	return strings.Join(parts, "")
}

// TestWorkspaceRefusesToEscape is the guard that keeps a model inside the
// project. It is a correctness guard rather than a security boundary, but a
// relative ".." is the common accident and it must not resolve.
func TestWorkspaceRefusesToEscape(t *testing.T) {
	ws, _ := workspace(t)

	for _, path := range []string{"../outside.txt", "../../etc/passwd", "sub/../../outside.txt"} {
		if _, err := ws.Resolve(path); err == nil {
			t.Fatalf("%q should have been refused", path)
		}
	}

	// Ordinary paths, including ones that merely contain dots, must resolve.
	for _, path := range []string{"a.txt", "sub/dir/a.txt", "./a.txt", "..hidden/a.txt"} {
		if _, err := ws.Resolve(path); err != nil {
			t.Fatalf("%q should resolve: %v", path, err)
		}
	}
}

// TestReadNumbersLinesAndPages covers the two behaviours a model depends on:
// line numbers so it can reference what it read, and paging so it can read a
// large file in bounded pieces.
func TestReadNumbersLinesAndPages(t *testing.T) {
	ws, root := workspace(t)
	if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte("one\ntwo\nthree\nfour\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	tool := tools.NewReadTool(ws)

	whole := text(run(t, tool, `{"path":"f.txt"}`))
	if !strings.Contains(whole, "1\tone") || !strings.Contains(whole, "4\tfour") {
		t.Fatalf("expected line-numbered content, got %q", whole)
	}

	page := text(run(t, tool, `{"path":"f.txt","offset":2,"limit":2}`))
	if !strings.Contains(page, "2\ttwo") || !strings.Contains(page, "3\tthree") {
		t.Fatalf("expected lines 2-3, got %q", page)
	}
	if strings.Contains(page, "1\tone") || strings.Contains(page, "4\tfour") {
		t.Fatalf("paging returned out-of-range lines: %q", page)
	}

	missing := run(t, tool, `{"path":"nope.txt"}`)
	if !missing.IsError {
		t.Fatal("reading a missing file must be an error result")
	}
}

// TestEditRequiresAnUnambiguousMatch: guessing which occurrence was meant is how
// a model silently corrupts a file, so an ambiguous edit must fail.
func TestEditRequiresAnUnambiguousMatch(t *testing.T) {
	ws, root := workspace(t)
	path := filepath.Join(root, "f.txt")
	if err := os.WriteFile(path, []byte("dup dup\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	tool := tools.NewEditTool(ws)

	ambiguous := run(t, tool, `{"path":"f.txt","old":"dup","new":"x"}`)
	if !ambiguous.IsError {
		t.Fatal("an ambiguous edit must fail rather than pick an occurrence")
	}

	content, _ := os.ReadFile(path)
	if string(content) != "dup dup\n" {
		t.Fatalf("a failed edit must not modify the file, got %q", content)
	}

	// A unique match succeeds.
	ok := run(t, tool, `{"path":"f.txt","old":"dup dup","new":"solo"}`)
	if ok.IsError {
		t.Fatalf("a unique edit should succeed: %s", text(ok))
	}
	content, _ = os.ReadFile(path)
	if string(content) != "solo\n" {
		t.Fatalf("file content = %q", content)
	}

	// A missing match is an error result, not a silent no-op.
	if missing := run(t, tool, `{"path":"f.txt","old":"absent","new":"x"}`); !missing.IsError {
		t.Fatal("a missing match must be reported")
	}
}

// TestWriteCreatesParentDirectories keeps a model from needing mkdir before it
// can write a file.
func TestWriteCreatesParentDirectories(t *testing.T) {
	ws, root := workspace(t)

	result := run(t, tools.NewWriteTool(ws), `{"path":"a/b/c.txt","content":"hello"}`)
	if result.IsError {
		t.Fatalf("write failed: %s", text(result))
	}

	content, err := os.ReadFile(filepath.Join(root, "a", "b", "c.txt"))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(content) != "hello" {
		t.Fatalf("content = %q", content)
	}
}

// TestGlobSupportsDoubleStar: ** is what models actually type, and neither
// filepath.Match nor path.Match implements it.
func TestGlobSupportsDoubleStar(t *testing.T) {
	ws, root := workspace(t)
	for _, name := range []string{"a.go", "sub/b.go", "sub/deep/c.go", "notes.txt"} {
		full := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, []byte("package x\n"), 0o644); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	result := text(run(t, tools.NewGlobTool(ws), `{"pattern":"**/*.go"}`))

	for _, want := range []string{"a.go", "sub/b.go", "sub/deep/c.go"} {
		if !strings.Contains(result, want) {
			t.Fatalf("expected %s in %q", want, result)
		}
	}
	if strings.Contains(result, "notes.txt") {
		t.Fatalf("a non-matching file was returned: %q", result)
	}

	// Results are sorted, so the same tree always produces the same bytes.
	if !strings.HasPrefix(result, "a.go") {
		t.Fatalf("results should be sorted, got %q", result)
	}
}

// TestGrepFindsAndSkipsBinary covers matching, case folding, and the binary
// guard that stops a search from dumping a compiled artifact into history.
func TestGrepFindsAndSkipsBinary(t *testing.T) {
	ws, root := workspace(t)
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("Hello\nworld\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin.dat"), []byte("Hello\x00\x01\x02"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	tool := tools.NewGrepTool(ws)

	hit := text(run(t, tool, `{"pattern":"world"}`))
	if !strings.Contains(hit, "a.txt:2:world") {
		t.Fatalf("expected a path:line:text hit, got %q", hit)
	}

	folded := text(run(t, tool, `{"pattern":"hello","ignore_case":true}`))
	if !strings.Contains(folded, "a.txt:1:Hello") {
		t.Fatalf("ignore_case did not match, got %q", folded)
	}
	if strings.Contains(folded, "bin.dat") {
		t.Fatalf("a binary file must not be searched: %q", folded)
	}

	none := text(run(t, tool, `{"pattern":"zzz-not-present"}`))
	if !strings.Contains(none, "No matches") {
		t.Fatalf("expected a clear no-match message, got %q", none)
	}
}

// TestBashReportsExitCodesAsResults: a failing command is data for the model,
// not a harness failure, so the step must continue.
func TestBashReportsExitCodesAsResults(t *testing.T) {
	ws, _ := workspace(t)
	tool := tools.NewBashTool(ws)

	ok := run(t, tool, `{"command":"echo hello"}`)
	if ok.IsError || !strings.Contains(text(ok), "hello") {
		t.Fatalf("expected success with output, got %q (error=%v)", text(ok), ok.IsError)
	}
	if !strings.Contains(text(ok), "exited 0") {
		t.Fatalf("expected an exit status in the result, got %q", text(ok))
	}

	failed := run(t, tool, `{"command":"exit 3"}`)
	if !failed.IsError {
		t.Fatal("a non-zero exit must be flagged")
	}
	if !strings.Contains(text(failed), "exited 3") {
		t.Fatalf("expected the exit code, got %q", text(failed))
	}

	// stderr is captured with stdout, because a model reads them the same way.
	both := run(t, tool, `{"command":"echo out; echo err 1>&2"}`)
	if !strings.Contains(text(both), "out") || !strings.Contains(text(both), "err") {
		t.Fatalf("expected both streams, got %q", text(both))
	}
}

// TestBashTimesOut covers the case that would otherwise pin a session forever.
func TestBashTimesOut(t *testing.T) {
	ws, _ := workspace(t)

	result := run(t, tools.NewBashTool(ws), `{"command":"sleep 5","timeout_ms":300}`)

	if !strings.Contains(text(result), "timed out") {
		t.Fatalf("expected a timeout report, got %q", text(result))
	}
}

// TestBashOutputIsDeterministic guards the cache: the same command must produce
// the same bytes, or two identical steps would present different history.
func TestBashOutputIsDeterministic(t *testing.T) {
	ws, _ := workspace(t)
	tool := tools.NewBashTool(ws)

	first := text(run(t, tool, `{"command":"printf 'a%.0s' $(seq 1 200)"}`))
	for i := 0; i < 5; i++ {
		again := text(run(t, tool, `{"command":"printf 'a%.0s' $(seq 1 200)"}`))
		if again != first {
			t.Fatalf("bash output is not deterministic at iteration %d", i)
		}
	}
}

// TestBashBoundsLargeOutput prevents a single command from becoming unbounded
// history that is re-billed on every later step.
func TestBashBoundsLargeOutput(t *testing.T) {
	ws, _ := workspace(t)

	result := run(t, tools.NewBashTool(ws), `{"command":"yes abcdefghij | head -n 20000"}`)

	if result.Omitted.Kind != tools.OmittedExact {
		t.Fatalf("expected an exact omission count, got %v", result.Omitted.Kind)
	}
	if result.Omitted.Count <= 0 {
		t.Fatalf("expected dropped bytes, got %d", result.Omitted.Count)
	}
	if len(text(result)) > 64*1024 {
		t.Fatalf("output is not bounded: %d bytes", len(text(result)))
	}
	if !strings.Contains(text(result), "omitted") {
		t.Fatal("the result must say that it was truncated")
	}
}
