package greenfield

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestApplicationSummaryOmitsTrackedDependenciesAndPreservesDeletedSource(t *testing.T) {
	root := t.TempDir()
	ctx := context.Background()
	if _, err := gitCommand(ctx, root, "init"); err != nil {
		t.Fatal(err)
	}
	_ = os.MkdirAll(filepath.Join(root, "app"), 0700)
	_ = os.WriteFile(filepath.Join(root, "app/deleted.php"), []byte("old"), 0600)
	_, _ = gitCommand(ctx, root, "add", ".")
	_, _ = gitCommand(ctx, root, "commit", "-m", "baseline")
	base, _ := gitCommand(ctx, root, "rev-parse", "HEAD")
	_ = os.Remove(filepath.Join(root, "app/deleted.php"))
	_ = os.WriteFile(filepath.Join(root, "app/kept.php"), []byte("new"), 0600)
	_ = os.WriteFile(filepath.Join(root, "artisan"), []byte("fixture"), 0600)
	_ = os.MkdirAll(filepath.Join(root, "node_modules/pkg"), 0700)
	for i := 0; i < 2000; i++ {
		_ = os.WriteFile(filepath.Join(root, fmt.Sprintf("node_modules/pkg/%04d-long-dependency-name.js", i)), []byte("dependency"), 0600)
	}
	_ = os.MkdirAll(filepath.Join(root, "storage/logs"), 0700)
	_ = os.WriteFile(filepath.Join(root, "storage/logs/laravel.log"), []byte("runtime"), 0600)
	_, _ = gitCommand(ctx, root, "add", "--all", "--force")
	_, _ = gitCommand(ctx, root, "commit", "-m", "fixture changes")
	full, err := gitCommand(ctx, root, "diff", "--stat", base, "HEAD")
	if err != nil || len(full) < 96000 {
		t.Fatal("fixture did not exceed old packet budget", len(full), err)
	}
	summary, err := applicationChangeSummary(ctx, root, base)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary) > 8500 || strings.Contains(summary, "node_modules") || strings.Contains(summary, "laravel.log") || !strings.Contains(summary, "deleted.php") || !strings.Contains(summary, "kept.php") {
		t.Fatal(summary)
	}
	cwd, _ := os.Getwd()
	module := filepath.Join(cwd, "..", "src", "team-context.ts")
	script := `import {pathToFileURL} from 'node:url';const {applicationChangeSummary}=await import(pathToFileURL(process.argv[1]).href);process.stdout.write(await applicationChangeSummary(process.argv[2],process.argv[3]));`
	node, err := exec.Command("node", "--input-type=module", "-e", script, module, root, base).CombinedOutput()
	if err != nil {
		t.Fatal(err, string(node))
	}
	if string(node) != summary {
		t.Fatal("CLI/desktop summary mismatch", string(node), summary)
	}
}
func TestCachedContextIsBoundedReadOnlyAndKeepsFullEvidence(t *testing.T) {
	raw := strings.Repeat("🧪", 10000)
	tool := cachedContextTool{sources: map[string]string{"check/task/0": raw}}
	if !tool.Definition().ReadOnly {
		t.Fatal("context lookup is not read-only")
	}
	result, err := tool.Execute(context.Background(), json.RawMessage(`{"key":"check/task/0","offset":4000,"limit":6000}`))
	if err != nil || result.IsError {
		t.Fatal(err, result)
	}
	var page struct {
		Text string `json:"text"`
		Next int    `json:"next_offset"`
	}
	_ = json.Unmarshal([]byte(resultText(result)), &page)
	if len([]rune(page.Text)) != 1500 || page.Next != 5500 || tool.sources["check/task/0"] != raw {
		t.Fatal("evidence pagination changed or lost content")
	}
	for _, args := range []string{`{"key":"../../.env"}`, `{"key":"check/task/0","limit":6001}`, `{"key":"check/task/0","offset":-1}`} {
		r, _ := tool.Execute(context.Background(), json.RawMessage(args))
		if !r.IsError {
			t.Fatal("invalid lookup allowed", args)
		}
	}
	checks := []Check{}
	for i := 0; i < 100; i++ {
		checks = append(checks, Check{Command: "test", Passed: i%2 == 0, Output: strings.Repeat("log", 10000)})
	}
	compact := compactChecks(checks)
	if len(compact) != 3 || len(compact[2].Output) > 750 || len(checks[0].Output) != 30000 {
		t.Fatal("check evidence was not compacted without mutating history")
	}
}
