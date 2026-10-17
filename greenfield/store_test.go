package greenfield

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"censi/harness/internal/agent"
	"censi/harness/internal/assemble"
	"censi/harness/internal/audit"
	"censi/harness/internal/llm"
	"censi/harness/internal/session"
	"censi/harness/internal/tools"
	"censi/harness/internal/wire"
)

type fixture struct {
	Kind    string         `json:"kind"`
	ID      string         `json:"id"`
	Content map[string]any `json:"content"`
}

func fixtures(t *testing.T) []fixture {
	t.Helper()
	raw, e := os.ReadFile("fixtures.json")
	if e != nil {
		t.Fatal(e)
	}
	var out []fixture
	if e = json.Unmarshal(raw, &out); e != nil {
		t.Fatal(e)
	}
	return out
}
func seeded(t *testing.T) *Store {
	t.Helper()
	s, e := NewStore(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	s.Locked(func(state *State) error { state.ExecutionMode = "scoped"; return s.Persist(state) })
	for _, f := range fixtures(t) {
		if _, e = s.Save(f.Kind, f.ID, f.Content); e != nil {
			t.Fatal(e)
		}
	}
	return s
}
func approve(t *testing.T, s *Store) Status {
	t.Helper()
	status, e := s.Status(nil)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Approve("design", status.Gates["design"]); e != nil {
		t.Fatal(e)
	}
	status, e = s.Approve("spec", status.Gates["spec"])
	if e != nil {
		t.Fatal(e)
	}
	return status
}
func TestGatesAndPersistence(t *testing.T) {
	s := seeded(t)
	status, e := s.Status(nil)
	if e != nil || len(status.Errors) != 0 {
		t.Fatalf("%v %+v", e, status.Errors)
	}
	if status.Stage != "awaiting_design_approval" || len(status.Ready) != 0 {
		t.Fatal(status.Stage, status.Ready)
	}
	if _, e = s.Approve("spec", status.Gates["spec"]); e == nil {
		t.Fatal("spec bypassed design")
	}
	status = approve(t, s)
	if len(status.Ready) != 1 || status.Ready[0] != "foundation" {
		t.Fatal(status.Ready)
	}
	reopened, e := NewStore(s.Root)
	if e != nil {
		t.Fatal(e)
	}
	status, e = reopened.Status(nil)
	if e != nil || status.Stage != "foundation" {
		t.Fatal(e, status.Stage)
	}
}
func TestChangedContractsAndTamperBlockExecution(t *testing.T) {
	s := seeded(t)
	before := approve(t, s)
	f := fixtures(t)[1]
	f.Content["summary"] = "Revised"
	if _, e := s.Save(f.Kind, f.ID, f.Content); e != nil {
		t.Fatal(e)
	}
	if _, e := s.Approve("design", before.Gates["design"]); e == nil {
		t.Fatal("stale approval accepted")
	}
	status, _ := s.Status(nil)
	if status.Stage != "awaiting_design_approval" {
		t.Fatal(status.Stage)
	}
	a := status.Artifacts[0]
	p, _ := s.Path(a.Path)
	if e := os.WriteFile(p, []byte("{}"), 0600); e != nil {
		t.Fatal(e)
	}
	status, _ = s.Status(nil)
	if len(status.Errors) == 0 || len(status.Ready) != 0 {
		t.Fatal("tampering not detected")
	}
}
func TestDAGAndRequirements(t *testing.T) {
	s := seeded(t)
	f := fixtures(t)[3]
	f.Content["depends_on"] = []any{"qa"}
	f.Content["requirements"] = []any{"invented"}
	if _, e := s.Save(f.Kind, f.ID, f.Content); e != nil {
		t.Fatal(e)
	}
	status, _ := s.Status(nil)
	joined := strings.Join(status.Errors, " ")
	if !strings.Contains(joined, "cycle") || !strings.Contains(joined, "Unknown requirement") {
		t.Fatal(joined)
	}
	qa := fixtures(t)[5]
	qa.Content["write"] = []any{"app/Task.php"}
	if _, e := s.Save(qa.Kind, qa.ID, qa.Content); e == nil {
		t.Fatal("mutable QA accepted")
	}
}
func TestReferenceAndSymlinkBoundaries(t *testing.T) {
	s := seeded(t)
	reference := t.TempDir()
	if e := os.WriteFile(filepath.Join(reference, "theme.css"), []byte("color:red"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := s.Reference("reference-1", "theme.css"); e == nil {
		t.Fatal("unattached reference accepted")
	}
	if _, e := s.Attach(reference); e != nil {
		t.Fatal(e)
	}
	body, e := s.Reference("reference-1", "theme.css")
	if e != nil || !strings.Contains(body, "color:red") {
		t.Fatal(e, body)
	}
	if _, e = s.Reference("reference-1", "../secret"); e == nil {
		t.Fatal("traversal accepted")
	}
	if _, e = s.Reference("reference-1", ".env"); e == nil {
		t.Fatal("credentials accepted")
	}
	if e = os.Symlink(reference, filepath.Join(s.Root, "escape")); e != nil {
		t.Skip(e)
	}
	if _, e = SafePath(s.Root, "escape/missing/file"); e == nil {
		t.Fatal("missing symlink ancestor escape accepted")
	}
}
func TestCommandsAndReports(t *testing.T) {
	for _, c := range []string{"php artisan test; rm -rf app", "npm test $(id)", "php artisan test --configuration evil.xml", "php -r 'eval()'"} {
		if AllowedCheck(c) {
			t.Fatal(c)
		}
	}
	if !AllowedCheck("php artisan test --filter=Tasks") {
		t.Fatal("test command refused")
	}
	for _, p := range []string{"../a", "/tmp/a", ".maxlabs/team/state.json", ".git/config", "app/../a", ".MAXLABS/team/state.json", ".Git/config", "app/*.php", "app/file.php."} {
		if SafeTaskPath(p) {
			t.Fatal(p)
		}
	}
	if CompletedReport("All done") || CompletedReport(`{"outcome":"blocked","summary":"Missing contract","blockers":["unknown"]}`) || !CompletedReport(`{"outcome":"completed","summary":"Done","blockers":[]}`) {
		t.Fatal("invalid report classification")
	}
}

type completedProvider struct{}

func (completedProvider) Complete(context.Context, wire.Request) (llm.Response, error) {
	return llm.Response{Message: session.Message{Role: session.RoleAssistant, Content: []session.Block{session.Text(`{"outcome":"completed","summary":"Implemented","blockers":[]}`)}}}, nil
}

type checkTool struct{ fail bool }

func (c checkTool) Definition() tools.Definition {
	return tools.Definition{Name: "bash", Description: "Test command runner", Parameters: json.RawMessage(`{"type":"object"}`)}
}
func (c checkTool) Execute(context.Context, json.RawMessage) (tools.Result, error) {
	if c.fail {
		return tools.Error("Assertion failed"), nil
	}
	return tools.Text("Passed"), nil
}
func runtime(t *testing.T, s *Store, fail bool) *Runtime {
	t.Helper()
	registry := tools.NewRegistry()
	if e := registry.Register(checkTool{fail}); e != nil {
		t.Fatal(e)
	}
	schemas, e := assemble.CanonicalizeTools(registry.Schemas(), []string{"bash"})
	if e != nil {
		t.Fatal(e)
	}
	return &Runtime{Store: s, Registry: registry, Provider: completedProvider{}, Assembly: assemble.Config{Model: "test", Identity: "Test", Tools: schemas}, MaxSteps: 5}
}
func execute(t *testing.T, r *Runtime, id string) tools.Result {
	t.Helper()
	raw, _ := json.Marshal(map[string]string{"action": "run", "id": id})
	result, e := r.Execute(context.Background(), raw)
	if e != nil {
		t.Fatal(e)
	}
	return result
}
func TestRunnerOwnsChecksAndDependencyOrder(t *testing.T) {
	s := seeded(t)
	approve(t, s)
	r := runtime(t, s, false)
	if !execute(t, r, "qa").IsError {
		t.Fatal("QA ran early")
	}
	for _, id := range []string{"foundation", "authorization", "qa"} {
		result := execute(t, r, id)
		if result.IsError {
			t.Fatal(resultText(result))
		}
	}
	status, _ := s.Status(nil)
	status, e := s.Approve("release", status.Gates["release"])
	if e != nil || status.Stage != "complete" {
		t.Fatal(e, status.Stage)
	}
}
func TestFailedChecksBoundRetries(t *testing.T) {
	s := seeded(t)
	approve(t, s)
	r := runtime(t, s, true)
	for i := 0; i < 3; i++ {
		if !execute(t, r, "foundation").IsError {
			t.Fatal("failed check marked passed")
		}
	}
	status, _ := s.Status(nil)
	if status.Runs["foundation"].Status != "failed" || status.Runs["foundation"].Attempts != 3 || len(status.Ready) != 0 {
		t.Fatal(status.Runs, status.Ready)
	}
}
func TestExternalInputChangeRequiresReapproval(t *testing.T) {
	s := seeded(t)
	approve(t, s)
	if e := os.MkdirAll(filepath.Join(s.Root, "app"), 0755); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(s.Root, "app/Task.php"), []byte("changed"), 0600); e != nil {
		t.Fatal(e)
	}
	result := execute(t, runtime(t, s, false), "foundation")
	if !result.IsError || !strings.Contains(resultText(result), "changed since approval") {
		t.Fatal(resultText(result))
	}
}
func TestPlannerAndWorkerCannotBypassWorkflow(t *testing.T) {
	planner := PlannerGuard{}
	for _, name := range []string{"bash", "task", "write", "edit"} {
		denied, e := planner.Permit(context.Background(), name, json.RawMessage(`{}`), false, nil)
		if e != nil || denied == "" {
			t.Fatal(name, e)
		}
	}
	workerRoot, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	guard := roleGuard{root: workerRoot.Root, role: "worker", task: map[string]any{"write": []any{"app/Task.php"}}}
	for _, raw := range []string{`{"path":".maxlabs/team/state.json"}`, `{"path":"other.php"}`} {
		denied, _ := guard.Permit(context.Background(), "write", json.RawMessage(raw), false, nil)
		if denied == "" {
			t.Fatal(raw)
		}
	}
	denied, _ := guard.Permit(context.Background(), "write", json.RawMessage(`{"path":"app/Task.php"}`), false, nil)
	if denied != "" {
		t.Fatal(denied)
	}
}

func TestInterruptedExecutionCanResume(t *testing.T) {
	s := seeded(t)
	approve(t, s)
	if e := s.Locked(func(state *State) error {
		state.Runs["foundation"] = &Run{Status: "running", Attempts: 1, Checks: []Check{}}
		return s.Persist(state)
	}); e != nil {
		t.Fatal(e)
	}
	status, _ := s.Status(nil)
	if status.Runs["foundation"].Status != "interrupted" || len(status.Ready) != 1 {
		t.Fatal(status.Runs, status.Ready)
	}
	if result := execute(t, runtime(t, s, false), "foundation"); result.IsError {
		t.Fatal(resultText(result))
	}
}
func TestEvidenceDoesNotInvalidateApprovedSpec(t *testing.T) {
	s := seeded(t)
	before := approve(t, s)
	if _, e := s.Save("evidence", "source-test", map[string]any{"summary": "Official API reference", "source_url": "https://example.test/docs", "retrieved_at": "2026-10-16T00:00:00Z", "content": "API schema", "content_hash": "fixture"}); e != nil {
		t.Fatal(e)
	}
	after, _ := s.Status(nil)
	if after.Gates["spec"] != before.Gates["spec"] || len(after.Ready) != 1 {
		t.Fatal(after.Stage, after.Ready)
	}
	r := runtime(t, s, false)
	result, e := r.Execute(context.Background(), json.RawMessage(`{"action":"save","kind":"evidence","id":"forged","content":{}}`))
	if e != nil || !result.IsError {
		t.Fatal(e, result)
	}
}

type skeletonTool struct{ root string }

func (s skeletonTool) Definition() tools.Definition {
	return tools.Definition{Name: "bash", Description: "Offline skeleton fixture", Parameters: json.RawMessage(`{"type":"object"}`)}
}
func (s skeletonTool) Execute(_ context.Context, raw json.RawMessage) (tools.Result, error) {
	var args map[string]any
	_ = json.Unmarshal(raw, &args)
	command := text(args, "command")
	if !strings.HasPrefix(command, "composer create-project laravel/laravel ") {
		return tools.Error("unexpected command"), nil
	}
	parts := strings.Split(command, "\"")
	if len(parts) < 3 {
		return tools.Error("missing quoted staging directory"), nil
	}
	path := filepath.Join(s.root, parts[1])
	if e := os.MkdirAll(filepath.Join(path, "database"), 0755); e != nil {
		return tools.Result{}, e
	}
	if e := os.MkdirAll(filepath.Join(path, "bootstrap"), 0755); e != nil {
		return tools.Result{}, e
	}
	for _, name := range []string{"artisan", "bootstrap/app.php"} {
		if e := os.WriteFile(filepath.Join(path, name), []byte("<?php"), 0600); e != nil {
			return tools.Result{}, e
		}
	}
	if e := os.WriteFile(filepath.Join(path, "composer.json"), []byte("{}"), 0600); e != nil {
		return tools.Result{}, e
	}
	if e := os.WriteFile(filepath.Join(path, ".env.example"), []byte("APP_KEY=\nDB_CONNECTION=sqlite\n"), 0600); e != nil {
		return tools.Result{}, e
	}
	return tools.Text("Created"), nil
}
func TestBootstrapInitializesLaravelWithoutOverwriting(t *testing.T) {
	s, e := NewStore(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	if e = s.Locked(func(state *State) error { return s.Persist(state) }); e != nil {
		t.Fatal(e)
	}
	registry := tools.NewRegistry()
	if e = registry.Register(skeletonTool{s.Root}); e != nil {
		t.Fatal(e)
	}
	r := &Runtime{Store: s, Registry: registry}
	result := r.bootstrap(context.Background(), "13.*")
	if result.IsError {
		t.Fatal(resultText(result))
	}
	environment, e := os.ReadFile(filepath.Join(s.Root, ".env"))
	if e != nil || !strings.Contains(string(environment), "APP_KEY=base64:") {
		t.Fatal("environment was not initialized", e)
	}
	if _, e = os.Stat(filepath.Join(s.Root, "database/database.sqlite")); e != nil {
		t.Fatal(e)
	}
	if !r.bootstrap(context.Background(), "12.*").IsError {
		t.Fatal("existing project overwritten")
	}
}
func TestSymlinkAliasCannotOverwriteWorkflowState(t *testing.T) {
	s := seeded(t)
	metadata, _ := s.Path("")
	if e := os.Symlink(metadata, filepath.Join(s.Root, "planning-alias")); e != nil {
		t.Skip(e)
	}
	if _, e := SafeWorkerPath(s.Root, "planning-alias/state.json"); e == nil {
		t.Fatal("metadata alias was writable")
	}
	reference := t.TempDir()
	if e := os.WriteFile(filepath.Join(reference, ".env"), []byte("SECRET"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink(filepath.Join(reference, ".env"), filepath.Join(reference, "theme.css")); e != nil {
		t.Fatal(e)
	}
	if _, e := s.Attach(reference); e != nil {
		t.Fatal(e)
	}
	if _, e := s.Reference("reference-1", "theme.css"); e == nil {
		t.Fatal("credential alias exposed")
	}
}

func TestDocumentationFetchBudgets(t *testing.T) {
	planner := &PlannerGuard{}
	researcher := &roleGuard{role: "research"}
	for i := 1; i <= 7; i++ {
		for _, guard := range []agent.Guard{planner, researcher} {
			denied, err := guard.Permit(context.Background(), "webfetch", json.RawMessage(`{"url":"https://laravel.com"}`), false, nil)
			if err != nil || (denied == "") != (i <= 6) {
				t.Fatal(i, denied, err)
			}
		}
	}
}

type formattingProvider struct {
	calls         int
	toolsInRepair int
}

func (p *formattingProvider) Complete(_ context.Context, req wire.Request) (llm.Response, error) {
	p.calls++
	text := `Done. {"outcome":"completed","summary":"Implemented","blockers":[]}`
	if p.calls > 1 {
		p.toolsInRepair = len(req.Tools)
		text = `{"outcome":"completed","summary":"Implemented","blockers":[]}`
	}
	return llm.Response{Message: session.Message{Role: session.RoleAssistant, Content: []session.Block{session.Text(text)}}}, nil
}
func TestReportRepairIsToolFreeAndChecksStillRun(t *testing.T) {
	s := seeded(t)
	approve(t, s)
	r := runtime(t, s, false)
	p := &formattingProvider{}
	r.Provider = p
	result := execute(t, r, "foundation")
	status, _ := s.Status(nil)
	if result.IsError || p.calls != 2 || p.toolsInRepair != 0 || len(status.Runs["foundation"].Checks) == 0 {
		t.Fatal(resultText(result), p, status.Runs)
	}
}

func TestDeterministicDispatchStopsAtFirstFailedTask(t *testing.T) {
	s := seeded(t)
	approve(t, s)
	text, count, err := runtime(t, s, true).RunApproved(context.Background())
	if err == nil || count != 1 || text != "" {
		t.Fatal(text, count, err)
	}
	status, _ := s.Status(nil)
	if len(status.Runs) != 1 {
		t.Fatal("dependent task ran", status.Runs)
	}
}
func TestDeterministicDispatchCompletesDAG(t *testing.T) {
	s := seeded(t)
	approve(t, s)
	text, count, err := runtime(t, s, false).RunApproved(context.Background())
	if err != nil || count != 3 || !strings.Contains(text, "All tasks passed") {
		t.Fatal(text, count, err)
	}
	status, _ := s.Status(nil)
	if status.Stage != "awaiting_release_approval" {
		t.Fatal(status.Stage)
	}
	for _, run := range status.Runs {
		if run.Archive == "" {
			t.Fatal("missing immutable run evidence")
		}
	}
}
func TestCompactFailuresRetainEveryFailureAndAssertionTail(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"tool": "phpunit", "failures": []any{map[string]any{"test": "one", "message": "PDO exception\nStack trace:\n" + strings.Repeat("stack", 10000)}, map[string]any{"test": "two", "message": strings.Repeat("HTML", 10000) + "contains aria-invalid"}}})
	compact := compactCheckOutput(string(raw))
	if !strings.Contains(compact, "\"test\":\"one\"") || !strings.Contains(compact, "\"test\":\"two\"") || !strings.Contains(compact, "contains aria-invalid") || len(compact) > 2500 {
		t.Fatal(compact)
	}
}

type qaEvidenceProvider struct {
	t     *testing.T
	sawQA bool
}

func (p *qaEvidenceProvider) Complete(_ context.Context, req wire.Request) (llm.Response, error) {
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if strings.Contains(b.Text, `"role":"qa"`) {
				p.sawQA = true
				if !strings.Contains(b.Text, `"verification_evidence"`) || !strings.Contains(b.Text, `"passed":true`) {
					p.t.Fatal("QA had no runner evidence")
				}
				for _, schema := range req.Tools {
					if schema.Name == "write" || schema.Name == "edit" || schema.Name == "bash" {
						p.t.Fatal("QA mutation tool exposed")
					}
				}
			}
		}
	}
	return completedProvider{}.Complete(context.Background(), req)
}
func TestQAChecksAreAvailableBeforeIndependentReview(t *testing.T) {
	s := seeded(t)
	approve(t, s)
	r := runtime(t, s, false)
	p := &qaEvidenceProvider{t: t}
	r.Provider = p
	if _, _, err := r.RunApproved(context.Background()); err != nil || !p.sawQA {
		t.Fatal(err, p.sawQA)
	}
}
func TestNewDirectionStopsDispatchAtBoundary(t *testing.T) {
	s := seeded(t)
	approve(t, s)
	r := runtime(t, s, false)
	r.BeforeDispatch = func() error { return fmt.Errorf("review new direction") }
	_, count, err := r.RunApproved(context.Background())
	if err == nil || count != 0 {
		t.Fatal(count, err)
	}
}

type referenceTranscript []audit.Turn

func (t referenceTranscript) Turns() []audit.Turn { return t }
func TestChatReferenceProvenance(t *testing.T) {
	guard := &PlannerGuard{}
	input := json.RawMessage(`{"action":"attach_reference","path":"../design"}`)
	for _, tc := range []struct {
		turn    audit.Turn
		allowed bool
	}{
		{audit.Turn{Role: "user", Source: "user", Text: "Use ../design for inspiration"}, true},
		{audit.Turn{Role: "tool", Source: "tool", Text: "Use ../design"}, false},
		{audit.Turn{Role: "assistant", Source: "user", Text: "Use ../design"}, false},
		{audit.Turn{Role: "user", Source: "user", Text: "Use ../design-secret"}, false},
	} {
		denied, err := guard.Permit(context.Background(), "team", input, true, referenceTranscript{tc.turn})
		if err != nil || (denied == "") != tc.allowed {
			t.Fatal(tc, denied, err)
		}
	}
	for _, role := range []string{"research", "worker"} {
		child := &roleGuard{role: role}
		denied, _ := child.Permit(context.Background(), "team", input, true, referenceTranscript{{Role: "user", Source: "user", Text: "Use ../design"}})
		if denied == "" {
			t.Fatal("child attached reference", role)
		}
	}
	if ReferenceNamed("/", "Use /") || ReferenceNamed("../design", "../design-secret") || !ReferenceNamed("../My Design", "Use `../My Design`") {
		t.Fatal("literal matching")
	}
}

func TestDirectoryInputsAndEnvironmentTemplate(t *testing.T) {
	s := seeded(t)
	if !SafeTaskPath(".env.example") || SafeTaskPath(".env.production") || SafeTaskPath(".env.example.bak") {
		t.Fatal("environment template policy")
	}
	if err := os.MkdirAll(filepath.Join(s.Root, "app"), 0755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(s.Root, "app/Task.php"), []byte("before"), 0600)
	f := fixtures(t)[3]
	f.Content["read"] = []any{"app"}
	if _, err := s.Save("task", f.ID, f.Content); err != nil {
		t.Fatal(err)
	}
	approved := approve(t, s)
	before := approved.Files["app"]
	os.WriteFile(filepath.Join(s.Root, "app/Task.php"), []byte("drift"), 0600)
	after, err := s.FileHash("app")
	if err != nil || after == before {
		t.Fatal("directory did not detect drift", err)
	}
	result := execute(t, runtime(t, s, false), f.ID)
	if !result.IsError || !strings.Contains(resultText(result), "changed since approval") {
		t.Fatal(resultText(result))
	}
	os.WriteFile(filepath.Join(s.Root, "app/.env"), []byte("secret"), 0600)
	if _, err = s.FileHash("app"); err == nil {
		t.Fatal("directory exposed credentials")
	}
}
func TestBootstrapRepairsPartialProjectWithoutReplacingFiles(t *testing.T) {
	s := seeded(t)
	os.MkdirAll(filepath.Join(s.Root, "app"), 0755)
	os.WriteFile(filepath.Join(s.Root, "app/Task.php"), []byte("preserve"), 0600)
	registry := tools.NewRegistry()
	registry.Register(skeletonTool{s.Root})
	r := &Runtime{Store: s, Registry: registry}
	if result := r.bootstrap(context.Background(), "12.*"); result.IsError {
		t.Fatal(resultText(result))
	}
	data, _ := os.ReadFile(filepath.Join(s.Root, "app/Task.php"))
	if string(data) != "preserve" {
		t.Fatal("worker files overwritten")
	}
	if !skeletonReady(s.Root) {
		t.Fatal("bootstrap did not materialize framework")
	}
}
