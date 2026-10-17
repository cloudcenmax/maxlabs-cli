package greenfield

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"censi/harness/internal/llm"
	"censi/harness/internal/session"
	"censi/harness/internal/wire"
)

func flexibleSeed(t *testing.T) *Store {
	t.Helper()
	s, e := NewStore(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	docs := []struct {
		kind, id string
		d        map[string]any
	}{
		{"brief", "product", map[string]any{"summary": "A personal task tool", "scope": []any{"Two operations"}, "open_questions": []any{}}},
		{"design", "direction", map[string]any{"summary": "Simple UI", "prototype_html": "<h1>Tasks</h1>"}},
		{"spec", "product", map[string]any{"summary": "Node application", "stack": map[string]any{"runtime": "node"}, "acceptance": []any{"Create tasks", "Complete tasks"}, "open_questions": []any{}}},
		{"task", "alpha", map[string]any{"summary": "alpha", "acceptance": []any{"First operation works"}}},
		{"task", "beta", map[string]any{"summary": "beta", "acceptance": []any{"Second operation works"}}},
		{"task", "qa", map[string]any{"summary": "qa", "role": "qa", "depends_on": []any{"alpha", "beta"}, "acceptance": []any{"Combined behavior works"}}},
		{"review", "review", map[string]any{"summary": "Reviewed", "findings": []any{}, "blockers": []any{}}},
	}
	for _, doc := range docs {
		if _, e = s.Save(doc.kind, doc.id, doc.d); e != nil {
			t.Fatal(e)
		}
	}
	status, e := s.Status(nil)
	if e != nil || len(status.Errors) > 0 {
		t.Fatal(e, status.Errors)
	}
	if _, e = s.Approve("design", status.Gates["design"]); e != nil {
		t.Fatal(e)
	}
	status, _ = s.Status(nil)
	if _, e = s.Approve("spec", status.Gates["spec"]); e != nil {
		t.Fatal(e)
	}
	raw := `import test from 'node:test';import assert from 'node:assert/strict';import {value as a} from './alpha.mjs';import {value as b} from './beta.mjs';test('both outcomes',()=>{assert.equal(a,'alpha');assert.equal(b,'beta')});`
	if e = os.WriteFile(filepath.Join(s.Root, "acceptance.test.mjs"), []byte(raw), 0644); e != nil {
		t.Fatal(e)
	}
	return s
}

type flexibleProvider struct {
	mu                          sync.Mutex
	active, peak                int
	conflict, missing, mutateQA bool
}

func (p *flexibleProvider) Complete(_ context.Context, req wire.Request) (llm.Response, error) {
	packet := map[string]any{}
	hasResult := false
	for _, message := range req.Messages {
		for _, block := range message.Content {
			if block.Type == session.BlockToolResult {
				hasResult = true
			}
			if pos := strings.Index(block.Text, "\n\n{\"branch\""); pos >= 0 {
				_ = json.Unmarshal([]byte(block.Text[pos+2:]), &packet)
			}
		}
	}
	// JSON map keys are sorted; branch starts the runner packet.
	if hasResult || p.missing {
		return completedProvider{}.Complete(context.Background(), req)
	}
	task, _ := packet["task"].(map[string]any)
	id := text(task, "summary")
	p.mu.Lock()
	p.active++
	if p.active > p.peak {
		p.peak = p.active
	}
	p.mu.Unlock()
	time.Sleep(40 * time.Millisecond)
	defer func() { p.mu.Lock(); p.active--; p.mu.Unlock() }()
	calls := []session.Block{}
	command := "node --test acceptance.test.mjs"
	if id != "qa" || p.mutateQA {
		file := id + ".mjs"
		if p.conflict {
			file = "shared.mjs"
		}
		args, _ := json.Marshal(map[string]string{"path": file, "content": "export const value = '" + id + "';\n"})
		calls = append(calls, session.ToolCall("write-"+id, "write", args))
		command = "node --check " + file
	}
	args, _ := json.Marshal(map[string]string{"command": command})
	calls = append(calls, session.ToolCall("verify-"+id, "verify", args))
	return llm.Response{Message: session.Message{Role: session.RoleAssistant, Content: calls}}, nil
}
func TestFlexibleParallelBranchesQAAndApply(t *testing.T) {
	s := flexibleSeed(t)
	r := runtime(t, s, false)
	p := &flexibleProvider{}
	r.Provider = p
	_, count, e := r.RunApproved(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	status, _ := s.Status(nil)
	if count != 3 || p.peak < 2 || status.Stage != "awaiting_release_approval" || !status.Runs["alpha"].Merged || status.Runs["qa"].Status != "passed" {
		t.Fatal(count, p.peak, status)
	}
	if _, e = os.Stat(filepath.Join(s.Root, "alpha.mjs")); !os.IsNotExist(e) {
		t.Fatal("original modified before acceptance", e)
	}
	if _, e = s.Approve("release", status.Gates["release"]); e != nil {
		t.Fatal(e)
	}
	raw, e := os.ReadFile(filepath.Join(s.Root, "alpha.mjs"))
	if e != nil || !strings.Contains(string(raw), "alpha") {
		t.Fatal(e, string(raw))
	}
}
func TestFlexibleConflictPreservesBranchesAndStopsDependents(t *testing.T) {
	s := flexibleSeed(t)
	r := runtime(t, s, false)
	r.Provider = &flexibleProvider{conflict: true}
	_, count, e := r.RunApproved(context.Background())
	if e == nil || count != 2 {
		t.Fatal(e, count)
	}
	status, _ := s.Status(nil)
	failed := 0
	for _, run := range status.Runs {
		if run.Status == "failed" && strings.Contains(run.Output, "Merge conflict") && run.Commit != "" {
			failed++
		}
	}
	if failed != 1 || status.Runs["qa"] != nil {
		t.Fatal(status.Runs)
	}
}
func TestFlexibleMissingEvidenceBlocks(t *testing.T) {
	s := flexibleSeed(t)
	r := runtime(t, s, false)
	r.Provider = &flexibleProvider{missing: true}
	result, e := r.runFlexible(context.Background(), "alpha")
	if e != nil || !result.IsError {
		t.Fatal(e, result)
	}
}
func TestFlexibleReleaseDetectsOriginalAndQADrift(t *testing.T) {
	s := flexibleSeed(t)
	r := runtime(t, s, false)
	r.Provider = &flexibleProvider{}
	if _, _, e := r.RunApproved(context.Background()); e != nil {
		t.Fatal(e)
	}
	status, _ := s.Status(nil)
	if e := os.WriteFile(filepath.Join(s.Root, "alpha.mjs"), []byte("user change"), 0644); e != nil {
		t.Fatal(e)
	}
	if _, e := s.Approve("release", status.Gates["release"]); e == nil || !strings.Contains(e.Error(), "original project changed") {
		t.Fatal(e)
	}
	root, _ := s.Path(status.Execution.Path)
	_ = os.WriteFile(filepath.Join(root, "alpha.mjs"), []byte("unreviewed"), 0644)
	if _, e := s.Approve("release", status.Gates["release"]); e == nil || !strings.Contains(e.Error(), "changed after QA") {
		t.Fatal(e)
	}
}
func TestFlexibleEnvironmentAndLegacyMode(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "private")
	t.Setenv("MAXLABS_TEST_SECRET", "private")
	for _, entry := range workerEnvironment() {
		if strings.HasPrefix(entry, "OPENAI_API_KEY=") || strings.HasPrefix(entry, "MAXLABS_TEST_SECRET=") {
			t.Fatal("provider credentials inherited")
		}
	}
	s := flexibleSeed(t)
	if _, e := s.SetExecutionMode("scoped"); e != nil {
		t.Fatal(e)
	}
	state, _ := s.Load()
	if state.Approvals["spec"] != "" {
		t.Fatal("approval survived mode change")
	}
	state.ExecutionMode = ""
	_ = s.Persist(&state)
	state, _ = s.Load()
	if state.ExecutionMode != "scoped" {
		t.Fatal("legacy mode changed silently")
	}
}

func TestFlexibleClientsAgreeOnGateFingerprints(t *testing.T) {
	s := flexibleSeed(t)
	status, _ := s.Status(nil)
	script := `import {TeamStore} from './src/greenfield.ts';import {Workspace} from './src/tools.ts';const s=await new TeamStore(await Workspace.open(process.argv[1])).status();console.log(JSON.stringify({gates:s.gates,ready:s.ready,mode:s.execution_mode}));`
	command := exec.Command("node", "--input-type=module", "-e", script, s.Root)
	command.Dir = ".."
	raw, e := command.CombinedOutput()
	if e != nil {
		t.Fatal(e, string(raw))
	}
	var result struct {
		Gates map[string]string
		Ready []string
		Mode  string
	}
	if e = json.Unmarshal(raw, &result); e != nil {
		t.Fatal(e, string(raw))
	}
	for gate, fingerprint := range status.Gates {
		if result.Gates[gate] != fingerprint {
			t.Fatalf("%s differs across clients: %v %v", gate, status.Gates, result.Gates)
		}
	}
	if strings.Join(result.Ready, ",") != strings.Join(status.Ready, ",") || result.Mode != "flexible" {
		t.Fatal(result, status.Ready)
	}
}

func TestFlexibleFoundationCanFollowSharedSetup(t *testing.T) {
	s := flexibleSeed(t)
	state, _ := s.Load()
	for _, a := range state.Artifacts {
		if a.Kind == "task" && a.ID == "beta" {
			d, e := s.Content(a)
			if e != nil {
				t.Fatal(e)
			}
			d["role"] = "foundation"
			d["depends_on"] = []any{"alpha"}
			if _, e = s.Save("task", "beta", d); e != nil {
				t.Fatal(e)
			}
		}
	}
	status, _ := s.Status(nil)
	if len(status.Errors) > 0 {
		t.Fatal(status.Errors)
	}
}
