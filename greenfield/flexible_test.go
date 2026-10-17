package greenfield

import (
	"context"
	"encoding/json"
	"fmt"
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
	command := "node --test acceptance.test.mjs 2>&1"
	if id != "qa" || p.mutateQA {
		file := id + ".mjs"
		if p.conflict {
			file = "shared.mjs"
		}
		args, _ := json.Marshal(map[string]string{"path": file, "content": "export const value = '" + id + "';\n"})
		calls = append(calls, session.ToolCall("write-"+id, "write", args))
		command = "node --check " + file + " > test-output.log 2>&1"
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
func TestRetryRecoversInterruptedUncommittedWork(t *testing.T) {
	s := flexibleSeed(t)
	r := runtime(t, s, false)
	r.Provider = &flexibleProvider{missing: true}
	if _, e := r.runFlexible(context.Background(), "alpha"); e != nil {
		t.Fatal(e)
	}
	state, _ := s.Load()
	run := state.Runs["alpha"]
	oldRoot, _ := s.Path(run.Workspace)
	if e := os.WriteFile(filepath.Join(oldRoot, "recovered.mjs"), []byte("export const recovered=true;"), 0644); e != nil {
		t.Fatal(e)
	}
	run.Commit = ""
	run.Status = "interrupted"
	if e := s.Persist(&state); e != nil {
		t.Fatal(e)
	}
	r = runtime(t, s, false)
	r.Provider = &flexibleProvider{}
	result, e := r.runFlexible(context.Background(), "alpha")
	if e != nil || result.IsError {
		t.Fatal(e, resultText(result))
	}
	state, _ = s.Load()
	root, _ := s.Path(state.Execution.Path)
	if _, e = os.Stat(filepath.Join(root, "recovered.mjs")); e != nil {
		t.Fatal("interrupted work lost", e)
	}
}

type workerResponseFunc func(wire.Request) (llm.Response, error)

func (f workerResponseFunc) Complete(_ context.Context, request wire.Request) (llm.Response, error) {
	return f(request)
}

func TestFailedPipelineCannotPassWorkerVerification(t *testing.T) {
	s := flexibleSeed(t)
	r := runtime(t, s, false)
	calls := 0
	r.Provider = workerResponseFunc(func(req wire.Request) (llm.Response, error) {
		calls++
		if calls > 1 {
			return completedProvider{}.Complete(context.Background(), req)
		}
		return llm.Response{Message: session.Message{Role: session.RoleAssistant, Content: []session.Block{session.ToolCall("verify", "verify", json.RawMessage(`{"command":"false 2>&1 | tail -20"}`))}}}, nil
	})
	result, err := r.runFlexible(context.Background(), "alpha")
	if err != nil || !result.IsError {
		t.Fatal(err, result)
	}
	status, _ := s.Status(nil)
	run := status.Runs["alpha"]
	if run.Status != "failed" || run.Merged || len(run.Checks) != 1 || run.Checks[0].Passed {
		t.Fatalf("masked failure passed: %+v", run)
	}
}

type coordinatorRecoveryProvider struct {
	fallback                   flexibleProvider
	keepCapping, mutate, blind bool
	mu                         sync.Mutex
	originalBranch             string
	recoveryCalls              int
}

func (p *coordinatorRecoveryProvider) Complete(ctx context.Context, req wire.Request) (llm.Response, error) {
	packet := map[string]any{}
	recovery, hasResult := false, false
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if b.Type == session.BlockToolResult {
				hasResult = true
			}
			if strings.HasPrefix(b.Text, "You are the MaxLabs engineering coordinator") {
				recovery = true
			}
			if pos := strings.LastIndex(b.Text, "\n\n{"); pos >= 0 {
				_ = json.Unmarshal([]byte(b.Text[pos+2:]), &packet)
			}
		}
	}
	answer := func(text string) (llm.Response, error) {
		return llm.Response{Message: session.Message{Role: session.RoleAssistant, Content: []session.Block{session.Text(text)}}, Usage: llm.Usage{UncachedInputTokens: 10, CacheReadTokens: 90, OutputTokens: 10}}, nil
	}
	call := func(name string, args string) (llm.Response, error) {
		return llm.Response{Message: session.Message{Role: session.RoleAssistant, Content: []session.Block{session.ToolCall("fixture-"+name, name, json.RawMessage(args))}}, Usage: llm.Usage{UncachedInputTokens: 10, CacheReadTokens: 90, OutputTokens: 10}}, nil
	}
	if recovery {
		p.mu.Lock()
		p.recoveryCalls++
		p.mu.Unlock()
		if !hasResult && !p.blind {
			if p.mutate {
				return call("verify", `{"command":"printf unexpected > partial.txt"}`)
			}
			return call("read", `{"path":"partial.txt"}`)
		}
		return answer(`{"action":"continue","summary":"Observed fixture failure; preserve the installed dependency and complete the remaining implementation.","next_steps":["Complete alpha.mjs using the existing partial work, preserve vendor, and run targeted verification."]}`)
	}
	task, _ := packet["task"].(map[string]any)
	if text(task, "summary") != "alpha" {
		return p.fallback.Complete(ctx, req)
	}
	if packet["recovery_plan"] == nil || p.keepCapping {
		p.mu.Lock()
		if p.originalBranch == "" {
			p.originalBranch = text(packet, "branch")
		}
		p.mu.Unlock()
		if hasResult {
			return call("verify", `{"command":"printf 'Invalid signature fixture\n' >&2; false"}`)
		}
		return llm.Response{Message: session.Message{Role: session.RoleAssistant, Content: []session.Block{session.ToolCall("partial", "write", json.RawMessage(`{"path":"partial.txt","content":"preserved work"}`)), session.ToolCall("dependency", "write", json.RawMessage(`{"path":"vendor/retained.txt","content":"installed dependency fixture"}`))}}}, nil
	}
	if hasResult {
		return completedProvider{}.Complete(ctx, req)
	}
	return llm.Response{Message: session.Message{Role: session.RoleAssistant, Content: []session.Block{session.ToolCall("finish", "write", json.RawMessage(`{"path":"alpha.mjs","content":"export const value='alpha';"}`)), session.ToolCall("verify", "verify", json.RawMessage(`{"command":"test -f partial.txt && test -f vendor/retained.txt && node --check alpha.mjs"}`))}}}, nil
}
func TestOrchestratorDiagnosesBudgetStopAndContinuesAutomatically(t *testing.T) {
	s := flexibleSeed(t)
	r := runtime(t, s, false)
	r.MaxSteps = 2
	p := &coordinatorRecoveryProvider{}
	r.Provider = p
	_, count, err := r.RunApproved(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	status, _ := s.Status(nil)
	run := status.Runs["alpha"]
	if count != 4 || run.Status != "passed" || run.Attempts != 2 || run.Branch != p.originalBranch || p.recoveryCalls != 2 || status.Runs["qa"].Status != "passed" {
		t.Fatalf("automatic recovery failed: %d %+v recovery calls %d", count, run, p.recoveryCalls)
	}
	root, _ := s.Path(run.Workspace)
	raw, err := os.ReadFile(filepath.Join(root, "vendor/retained.txt"))
	if err != nil || !strings.Contains(string(raw), "dependency") {
		t.Fatal("dependency setup was discarded", err)
	}
	directory, _ := s.Path("recovery")
	files, err := os.ReadDir(directory)
	if err != nil || len(files) != 1 {
		t.Fatal("recovery evidence missing", err, len(files))
	}
	record, _ := os.ReadFile(filepath.Join(directory, files[0].Name()))
	var recovery Recovery
	_ = json.Unmarshal(record, &recovery)
	if recovery.Plan.Action != "continue" || recovery.Usage.Calls != 2 {
		t.Fatal(recovery)
	}
	if len(run.RecoveryTrail) != 1 || run.RecoveryTrail[0].Archive != recovery.Archive {
		t.Fatal("completed run lost coordinator evidence", run.RecoveryTrail)
	}
}
func TestNewDirectionStopsBeforeAutomaticRecovery(t *testing.T) {
	s := flexibleSeed(t)
	r := runtime(t, s, false)
	r.MaxSteps = 2
	p := &coordinatorRecoveryProvider{}
	r.Provider = p
	dispatches := 0
	r.BeforeDispatch = func() error {
		dispatches++
		if dispatches > 1 {
			return fmt.Errorf("new user direction")
		}
		return nil
	}
	_, _, err := r.RunApproved(context.Background())
	status, _ := s.Status(nil)
	if err == nil || p.recoveryCalls != 0 || status.Runs["alpha"].Attempts != 1 {
		t.Fatal("continued after user interruption", err, p.recoveryCalls)
	}
}
func TestOrchestratorRecoveryIsBounded(t *testing.T) {
	s := flexibleSeed(t)
	r := runtime(t, s, false)
	r.MaxSteps = 2
	p := &coordinatorRecoveryProvider{keepCapping: true}
	r.Provider = p
	_, count, err := r.RunApproved(context.Background())
	if err == nil {
		t.Fatal("unbounded retry")
	}
	if !strings.Contains(err.Error(), "automatic recovery stopped at its approved budget") || !strings.Contains(err.Error(), "Preserved branch:") {
		t.Fatal("missing actionable budget handoff", err)
	}
	status, _ := s.Status(nil)
	if status.Runs["alpha"].Attempts != 3 || count != 4 || p.recoveryCalls != 4 || status.Runs["qa"] != nil {
		t.Fatal(count, p.recoveryCalls, status.Runs)
	}
}
func TestTeamWorkerAttemptLimitIs100(t *testing.T) {
	if Contract.WorkerStepLimit != 100 {
		t.Fatal(Contract.WorkerStepLimit)
	}
	for configured, expected := range map[int]int{0: 100, -1: 100, 100: 100, 101: 100, 60: 60, 2: 2} {
		if actual := workerStepLimit(configured); actual != expected {
			t.Fatalf("configured %d: got %d want %d", configured, actual, expected)
		}
	}
}
func TestCoordinatorCannotMutateSourceOrApproveBlindRetry(t *testing.T) {
	for _, mode := range []string{"mutate", "blind"} {
		t.Run(mode, func(t *testing.T) {
			s := flexibleSeed(t)
			r := runtime(t, s, false)
			r.MaxSteps = 2
			r.Provider = &coordinatorRecoveryProvider{mutate: mode == "mutate", blind: mode == "blind"}
			_, _, err := r.RunApproved(context.Background())
			if err == nil {
				t.Fatal("invalid recovery allowed")
			}
			status, _ := s.Status(nil)
			run := status.Runs["alpha"]
			if run.Attempts != 1 || run.Merged || run.Recovery == nil || run.Recovery.Plan.Action != "blocked" || status.Runs["qa"] != nil {
				t.Fatal(run)
			}
		})
	}
}

func TestRecoveryFingerprintMatchesCLIAndIgnoresLaravelRuntimeData(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"artisan", "résumé&.txt", "\ue000.txt", "\U00010000.txt", "storage/framework/views/.gitignore"} {
		path := filepath.Join(root, name)
		_ = os.MkdirAll(filepath.Dir(path), 0755)
		if err := os.WriteFile(path, []byte("MaxLabs fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	before, err := sourceHash(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"storage/logs/laravel.log", "storage/framework/views/compiled.php", "bootstrap/cache/config.php", "database/database.sqlite"} {
		path := filepath.Join(root, name)
		_ = os.MkdirAll(filepath.Dir(path), 0755)
		if err = os.WriteFile(path, []byte("runtime output"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	after, err := sourceHash(root)
	if err != nil || before != after {
		t.Fatal("runtime data changed source fingerprint", err)
	}
	cwd, _ := os.Getwd()
	module := filepath.ToSlash(filepath.Join(cwd, "..", "src", "team-recovery.ts"))
	script := `import {pathToFileURL} from 'node:url';const {sourceDigest}=await import(pathToFileURL(process.argv[1]).href);process.stdout.write(await sourceDigest(process.argv[2]));`
	raw, err := exec.Command("node", "--input-type=module", "-e", script, module, root).CombinedOutput()
	if err != nil || strings.TrimSpace(string(raw)) != after {
		t.Fatal("CLI and Go fingerprints differ", err, string(raw), after)
	}
	_ = os.WriteFile(filepath.Join(root, "résumé&.txt"), []byte("changed source"), 0600)
	changed, _ := sourceHash(root)
	if changed == after {
		t.Fatal("application source edits were ignored")
	}
}
