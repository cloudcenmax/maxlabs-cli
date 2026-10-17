package greenfield

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"censi/harness/internal/agent"
	"censi/harness/internal/assemble"
	"censi/harness/internal/audit"
	"censi/harness/internal/llm"
	"censi/harness/internal/session"
	"censi/harness/internal/tools"
)

type Runtime struct {
	BeforeDispatch func() error
	permissionMu   sync.Mutex
	progressMu     sync.Mutex
	researchUsed   int
	workersUsed    int
	OnTool         func(session.Block, tools.Result)
	Store          *Store
	Provider       llm.Provider
	Registry       *tools.Registry
	Guard          agent.Guard
	Assembly       assemble.Config
	WorkerModel    string
	MaxSteps       int
}

func (r *Runtime) Definition() tools.Definition {
	return tools.Definition{Name: "team", Description: "Greenfield SDE Team (Beta). Persist contracts, inspect readiness, attach a folder explicitly supplied in chat, read attached references, research in up to three agents, or run one approved task. Human approvals are only available through client controls.", ReadOnly: true, Parameters: json.RawMessage(`{"type":"object","properties":{"action":{"type":"string","enum":["status","environment","artifact","save","reference","attach_reference","research","run"]},"kind":{"type":"string"},"id":{"type":"string"},"path":{"type":"string"},"content":{"type":"object"},"jobs":{"type":"array","maxItems":3,"items":{"type":"string"}}},"required":["action"],"additionalProperties":false}`)}
}
func jsonResult(v any) tools.Result {
	raw, e := json.Marshal(v)
	if e != nil {
		return tools.Error(e.Error())
	}
	return tools.Text(string(raw))
}
func (r *Runtime) Execute(ctx context.Context, raw json.RawMessage) (tools.Result, error) {
	var args struct {
		Action  string         `json:"action"`
		Kind    string         `json:"kind"`
		ID      string         `json:"id"`
		Path    string         `json:"path"`
		Content map[string]any `json:"content"`
		Jobs    []string       `json:"jobs"`
	}
	if e := json.Unmarshal(raw, &args); e != nil {
		return tools.Error(e.Error()), nil
	}
	fail := func(e error) (tools.Result, error) { return tools.Error("team: " + e.Error()), nil }
	switch args.Action {
	case "environment":
		observed := []map[string]any{}
		for _, command := range []string{"php --version", "composer --version", "node --version", "npm --version"} {
			result := r.executeCommand(ctx, command, true)
			observed = append(observed, map[string]any{"command": command, "available": !result.IsError, "output": truncate(resultText(result), 1500)})
		}
		return jsonResult(map[string]any{"observed": observed, "note": "Observed local tools; verify framework support in official docs. These probes do not establish the latest Laravel release."}), nil
	case "status":
		s, e := r.Store.Status(nil)
		if e != nil {
			return fail(e)
		}
		compact := map[string]any{}
		encoded, _ := json.Marshal(s)
		_ = json.Unmarshal(encoded, &compact)
		delete(compact, "files")
		runs := map[string]any{}
		for id, run := range s.Runs {
			checks := []map[string]any{}
			for _, c := range run.Checks {
				checks = append(checks, map[string]any{"command": c.Command, "passed": c.Passed, "log": c.Log})
			}
			runs[id] = map[string]any{"status": run.Status, "attempts": run.Attempts, "summary": truncate(run.Output, 600), "checks": checks}
		}
		artifacts := []Artifact{}
		for _, a := range s.Artifacts {
			a.Summary = truncate(a.Summary, 160)
			artifacts = append(artifacts, a)
		}
		compact["artifacts"] = artifacts
		compact["artifact_root"] = ".maxlabs/team"
		compact["runs"] = runs
		return jsonResult(compact), nil
	case "artifact":
		state, err := r.Store.Load()
		if err != nil {
			return fail(err)
		}
		for _, a := range state.Artifacts {
			if a.Kind == args.Kind && a.ID == args.ID {
				content, err := r.Store.Content(a)
				if err != nil {
					return fail(err)
				}
				delete(content, "prototype_html")
				if a.Kind == "evidence" {
					if body, ok := content["content"].(string); ok {
						content["content"] = truncate(body, 16000)
					}
				}
				return jsonResult(map[string]any{"artifact": a, "content": content, "note": "Prototype HTML is available in client preview, omitted from planner context."}), nil
			}
		}
		return fail(fmt.Errorf("artifact not found"))
	case "save":
		if args.Kind == "evidence" {
			return fail(fmt.Errorf("evidence is recorded only by successful research fetches"))
		}
		s, e := r.Store.Save(args.Kind, args.ID, args.Content)
		if e != nil {
			return fail(e)
		}
		var artifact Artifact
		for _, a := range s.Artifacts {
			if a.Kind == args.Kind && a.ID == args.ID {
				artifact = a
			}
		}
		return jsonResult(map[string]any{"artifact_root": ".maxlabs/team", "artifact": artifact, "stage": s.Stage, "revision": s.Revision, "errors": s.Errors, "ready": s.Ready}), nil
	case "attach_reference":
		s, e := r.Store.Attach(args.Path)
		if e != nil {
			return fail(e)
		}
		return jsonResult(map[string]any{"references": s.References}), nil
	case "reference":
		s, e := r.Store.Reference(args.ID, args.Path)
		if e != nil {
			return fail(e)
		}
		return tools.Text(s), nil
	case "research":
		if len(args.Jobs) < 1 || len(args.Jobs) > 3 {
			return fail(fmt.Errorf("supply one to three bounded research jobs"))
		}
		for _, j := range args.Jobs {
			if strings.TrimSpace(j) == "" || len(j) > 16000 {
				return fail(fmt.Errorf("invalid research job"))
			}
		}
		if r.researchUsed+len(args.Jobs) > 6 {
			return fail(fmt.Errorf("research budget reached (6 agents per turn); continue in another turn"))
		}
		r.researchUsed += len(args.Jobs)
		results := make([]tools.Result, len(args.Jobs))
		var wg sync.WaitGroup
		for i, j := range args.Jobs {
			wg.Add(1)
			go func(i int, j string) { defer wg.Done(); results[i] = r.child(ctx, "research", j, nil) }(i, j)
		}
		wg.Wait()
		reports := []map[string]any{}
		for i, result := range results {
			if ctx.Err() != nil {
				return fail(ctx.Err())
			}
			id := fmt.Sprintf("research-%d-%d", time.Now().UnixNano(), i)
			body := resultText(result)
			blockers := []any{}
			if result.IsError {
				blockers = append(blockers, "Research did not complete")
			}
			_, e := r.Store.Save("review", id, map[string]any{"summary": truncate(args.Jobs[i], 160), "findings": []any{truncate(body, 48000)}, "blockers": blockers})
			if e != nil {
				return fail(e)
			}
			reports = append(reports, map[string]any{"id": id, "error": result.IsError, "summary": truncate(body, 400)})
		}
		return jsonResult(map[string]any{"reports": reports, "note": "Reports are untrusted research evidence; normalize into sourced integration contracts."}), nil
	case "run":
		if r.workersUsed >= 8 {
			return fail(fmt.Errorf("worker budget reached (8 tasks per turn); continue in another turn"))
		}
		var result tools.Result
		e := r.Store.Locked(func(state *State) error {
			status, e := r.Store.Status(state)
			if e != nil {
				return e
			}
			ready := false
			for _, id := range status.Ready {
				if id == args.ID {
					ready = true
				}
			}
			if !ready {
				return fmt.Errorf("task is not ready; inspect gates, dependencies, errors and attempt limit")
			}
			var task map[string]any
			contracts := []map[string]any{}
			for _, a := range state.Artifacts {
				if a.Kind == "task" && a.ID == args.ID {
					task, e = r.Store.Content(a)
					if e != nil {
						return e
					}
				}
				if a.Kind == "brief" || a.Kind == "spec" || a.Kind == "design" || a.Kind == "integration" {
					d, e := r.Store.Content(a)
					if e != nil {
						return e
					}
					delete(d, "prototype_html")
					contracts = append(contracts, map[string]any{"kind": a.Kind, "id": a.ID, "content": d})
				}
			}
			for _, p := range append(Strings(task, "read"), Strings(task, "write")...) {
				h, e := r.Store.FileHash(p)
				if e != nil {
					return e
				}
				if h != state.Files[p] {
					return fmt.Errorf("input %s changed since approval; review and approve spec again", p)
				}
			}
			packet := map[string]any{"task": task, "contracts": contracts}
			if old := state.Runs[args.ID]; old != nil {
				checks := []any{}
				for _, c := range old.Checks {
					checks = append(checks, map[string]any{"command": c.Command, "passed": c.Passed, "output": truncate(c.Output, 2200), "log": c.Log})
				}
				packet["previous_attempt"] = map[string]any{"status": old.Status, "summary": truncate(old.Output, 1000), "checks": checks}
			}
			input, _ := json.Marshal(packet)
			if len(input) > 96000 {
				return fmt.Errorf("worker context exceeds beta budget; reduce contracts or release scope")
			}
			attempts := 1
			if old := state.Runs[args.ID]; old != nil {
				attempts += old.Attempts
			}
			run := &Run{ID: fmt.Sprintf("%d", time.Now().UnixNano()), Status: "running", Attempts: attempts, Checks: []Check{}}
			state.Runs[args.ID] = run
			delete(state.Approvals, "release")
			if e = r.Store.Persist(state); e != nil {
				return e
			}
			r.workersUsed++
			var child tools.Result
			if version := text(task, "bootstrap"); version != "" {
				child = r.bootstrap(ctx, version)
				if !child.IsError {
					for p := range state.Files {
						h, e := r.Store.FileHash(p)
						if e != nil {
							return e
						}
						state.Files[p] = h
					}
				}
			}
			if !child.IsError && text(task, "role") == "qa" {
				for _, command := range Strings(task, "checks") {
					check := r.verify(ctx, command)
					record, err := r.Store.recordCheck(args.ID+"-"+run.ID, attempts, len(run.Checks)+1, command, resultText(check), !check.IsError)
					if err != nil {
						return err
					}
					run.Checks = append(run.Checks, record)
					if check.IsError {
						child = tools.Error("QA runner checks failed before independent review; see check evidence")
					}
				}
				if !child.IsError {
					packet["verification_evidence"] = run.Checks
					input, _ = json.Marshal(packet)
					if len(input) > 96000 {
						child = tools.Error("QA context exceeds beta budget; refine contracts")
					}
				}
			}
			if !child.IsError {
				child = r.child(ctx, "worker", string(input), task)
			}
			run.Output = truncate(resultText(child), 48000)
			run.Status = "checking"
			failed := child.IsError
			if !failed && text(task, "role") != "qa" {
				for _, command := range Strings(task, "checks") {
					if ctx.Err() != nil {
						failed = true
						break
					}
					check := r.verify(ctx, command)
					record, logErr := r.Store.recordCheck(args.ID+"-"+run.ID, attempts, len(run.Checks)+1, command, resultText(check), !check.IsError)
					if logErr != nil {
						return logErr
					}
					run.Checks = append(run.Checks, record)
					if check.IsError {
						failed = true
					}
				}
			}
			run.Status = "passed"
			if failed {
				run.Status = "failed"
			}
			if ctx.Err() != nil {
				run.Status = "interrupted"
			}
			for _, p := range Strings(task, "write") {
				h, e := r.Store.FileHash(p)
				if e != nil {
					return e
				}
				state.Files[p] = h
			}
			if e = r.Store.archiveRun(args.ID, run); e != nil {
				return e
			}
			if e = r.Store.Persist(state); e != nil {
				return e
			}
			checks := []Check{}
			for _, c := range run.Checks {
				limit := 4000
				if c.Passed {
					limit = 500
				}
				checks = append(checks, Check{Command: c.Command, Passed: c.Passed, Output: truncate(c.Output, limit), Log: c.Log})
			}
			result = jsonResult(map[string]any{"id": args.ID, "status": run.Status, "attempts": run.Attempts, "output": truncate(run.Output, 2000), "checks": checks})
			result.IsError = run.Status != "passed"
			return nil
		})
		if e != nil {
			return fail(e)
		}
		return result, nil
	default:
		return fail(fmt.Errorf("unknown team action"))
	}
}
func (r *Runtime) verify(ctx context.Context, command string) tools.Result {
	if !AllowedCheck(command) {
		return tools.Error("unsupported verification command")
	}
	return r.executeCommand(ctx, command, true)
}
func (r *Runtime) executeCommand(ctx context.Context, command string, approved bool) tools.Result {
	tool, ok := r.Registry.Get("bash")
	if !ok {
		return tools.Error("verification runner unavailable")
	}
	args, _ := json.Marshal(map[string]any{"command": command, "timeout_ms": 300000})
	if r.Guard != nil && !approved {
		denied, e := r.Guard.Permit(ctx, "bash", args, false, nil)
		if e != nil {
			return tools.Error(e.Error())
		}
		if denied != "" {
			return tools.Error("verification denied: " + denied)
		}
	}
	result, e := tool.Execute(ctx, args)
	if e != nil {
		return tools.Error(e.Error())
	}
	return result
}
func (r *Runtime) child(ctx context.Context, role, prompt string, task map[string]any) tools.Result {
	assembly := r.Assembly
	assembly.Tools = nil
	for _, schema := range r.Registry.Schemas() {
		if schema.Name == "read" || schema.Name == "glob" || schema.Name == "grep" || (role == "research" && (schema.Name == "webfetch" || schema.Name == "team")) || (role == "worker" && text(task, "role") != "qa" && (schema.Name == "write" || schema.Name == "edit")) {
			assembly.Tools = append(assembly.Tools, schema)
		}
	}
	if role == "worker" && r.WorkerModel != "" {
		assembly.Model = r.WorkerModel
	}
	guard := &roleGuard{root: r.Store.Root, base: runtimeGuard{r}, role: role, task: task}
	limit := r.MaxSteps
	if limit <= 0 || limit > 40 {
		limit = 40
	}
	child, e := agent.New(agent.Config{Assembly: assembly, Provider: r.Provider, Tools: r.Registry, Guard: guard, MaxSteps: limit, MaxRetries: 2, MaxToolResultBytes: 16000}, session.New(nil))
	if e != nil {
		return tools.Error(e.Error())
	}
	var evidenceErr error
	child.SetToolResultHook(func(call session.Block, result tools.Result) {
		if role == "research" && call.Name == "webfetch" && !result.IsError {
			var args map[string]any
			_ = json.Unmarshal(call.Arguments, &args)
			body := resultText(result)
			_, e := r.Store.Save("evidence", fmt.Sprintf("source-%d", time.Now().UnixNano()), map[string]any{"summary": "API source: " + text(args, "url"), "source_url": text(args, "url"), "retrieved_at": time.Now().UTC().Format(time.RFC3339Nano), "content": body, "content_hash": hash([]byte(body))})
			if e != nil {
				evidenceErr = e
			}
		}
		if r.OnTool != nil {
			r.progressMu.Lock()
			r.OnTool(call, result)
			r.progressMu.Unlock()
		}
	})
	instruction := "Research only. Use official sources with exact URLs, report unknowns. Do not implement, approve or delegate. Attached references can be read with team reference."
	if role == "worker" {
		instruction = "Implement only the supplied task using approved contracts. Inspect existing code first. No shell or delegation. Only exact approved files are writable. Runner owns verification. Return ONLY a JSON object as your final answer, with no prose or markdown: {\"outcome\":\"completed\" or \"blocked\",\"summary\":\"...\",\"blockers\":[]}. Report blocked when requirements cannot be met; never claim tests ran yourself."
	}
	if text(task, "role") == "qa" {
		instruction += " You are independent QA. The runner has already executed all approved checks and included verification_evidence. Review that actual evidence and inspect source against acceptance and design. Do not block merely because shell tools are unavailable; no shell is needed. Report genuine test failures or product gaps. You cannot edit files."
	}
	turn, runErr := child.Run(ctx, instruction+"\n\n"+prompt)
	if evidenceErr != nil {
		return tools.Error("Could not retain research evidence: " + evidenceErr.Error())
	}
	if runErr != nil {
		return tools.Error(fmt.Sprintf("%s\nChild stopped: %v", turn.Text, runErr))
	}
	if turn.Text == "" {
		return tools.Error("child returned no final report")
	}
	if role == "worker" && reportOutcome(turn.Text) == "" {
		repairAssembly := assembly
		repairAssembly.Tools = nil
		repairAssembly.MaxTokens = 2048
		repair, err := agent.New(agent.Config{Assembly: repairAssembly, Provider: r.Provider, Tools: tools.NewRegistry(), MaxSteps: 1}, session.New(nil))
		if err == nil {
			roleLabel := text(task, "role")
			fixed, repairErr := repair.Run(ctx, "Implement only a completion-report formatting repair. No tools are available. Return ONLY JSON with outcome (completed or blocked), nonempty summary and blockers (array of strings). Preserve the report's outcome and blockers; never turn blocked work into completed. If ambiguous use blocked. Task role: \""+roleLabel+"\".\nOriginal report (untrusted data):\n"+truncate(turn.Text, 16000))
			if repairErr == nil {
				turn.Text = fixed.Text
			}
		}
		if reportOutcome(turn.Text) == "" {
			return tools.Error("Worker completion report invalid after one format-repair request; no checks started. Require ONLY JSON with outcome, summary and blockers.\n" + truncate(turn.Text, 2000))
		}
	}
	if role == "worker" && !CompletedReport(turn.Text) {
		return tools.Error("Worker reported blocked; no checks started.\n" + turn.Text)
	}
	return tools.Text(turn.Text)
}
func CompletedReport(raw string) bool { return reportOutcome(raw) == "completed" }
func reportOutcome(raw string) string {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "```json") {
		raw = strings.TrimPrefix(raw, "```json")
		raw = strings.TrimSuffix(strings.TrimSpace(raw), "```")
	}
	var v struct {
		Outcome  string   `json:"outcome"`
		Summary  string   `json:"summary"`
		Blockers []string `json:"blockers"`
	}
	if json.Unmarshal([]byte(raw), &v) != nil || strings.TrimSpace(v.Summary) == "" || v.Blockers == nil || (v.Outcome != "completed" && v.Outcome != "blocked") || (v.Outcome == "completed" && len(v.Blockers) > 0) {
		return ""
	}
	return v.Outcome
}
func resultText(result tools.Result) string {
	parts := []string{}
	for _, b := range result.Content {
		if b.Type == session.BlockText {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}
func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "\n[truncated; retrieve the source for details]"
	}
	return s
}

// PlannerGuard prevents direct application changes, bypass delegation and state
// edits. Only the team tool can advance this workflow. Existing approvals still
// govern network and verification operations.
type PlannerGuard struct {
	Base    agent.Guard
	Fetches int
}

func (g *PlannerGuard) Permit(ctx context.Context, name string, args json.RawMessage, readOnly bool, t audit.Transcript) (string, error) {
	if name == "team" {
		var input struct {
			Action string `json:"action"`
			Path   string `json:"path"`
		}
		if json.Unmarshal(args, &input) != nil {
			return "invalid tool arguments", nil
		}
		if input.Action == "attach_reference" {
			if t != nil {
				for _, turn := range t.Turns() {
					if turn.Role == "user" && turn.Source == "user" && ReferenceNamed(input.Path, turn.Text) {
						return "", nil
					}
				}
			}
			return "Use only an exact folder path supplied by the user in chat. Ask which design to use if no path was provided", nil
		}
	}
	if name == "webfetch" {
		g.Fetches++
		if g.Fetches > 6 {
			return "Documentation budget reached (6 fetches per turn). Use evidence or record unknowns", nil
		}
		return "", nil
	}
	if name == "write" || name == "edit" || name == "bash" || name == "task" {
		return "Greenfield planning uses team save/research/run; direct application mutation and unscoped delegation are disabled", nil
	}
	if g.Base != nil {
		return g.Base.Permit(ctx, name, args, readOnly, t)
	}
	return "", nil
}

type roleGuard struct {
	root    string
	base    agent.Guard
	role    string
	fetches int
	task    map[string]any
}

func (g *roleGuard) Permit(ctx context.Context, name string, raw json.RawMessage, readOnly bool, t audit.Transcript) (string, error) {
	var args map[string]any
	if e := json.Unmarshal(raw, &args); e != nil {
		return "invalid tool arguments", nil
	}
	if name == "team" {
		if g.role == "research" && (text(args, "action") == "reference" || text(args, "action") == "status") {
			return "", nil
		}
		return "children cannot control the workflow", nil
	}
	if name == "task" || name == "bash" {
		return "child shell and delegation are disabled; runner owns checks", nil
	}
	if name == "write" || name == "edit" {
		if g.role != "worker" {
			return "research is read-only", nil
		}
		p := text(args, "path")
		allowed := false
		for _, v := range Strings(g.task, "write") {
			if p == v {
				allowed = true
			}
		}
		if !allowed || !SafeTaskPath(p) {
			return "worker write is outside its approved file scope", nil
		}
		if _, e := SafeWorkerPath(g.root, p); e != nil {
			return e.Error(), nil
		}
		return "", nil // exact file scope was authorized by spec approval
	}
	if name == "webfetch" && g.role != "research" {
		return "use the supplied integration contracts", nil
	}
	if name == "webfetch" && g.role == "research" {
		g.fetches++
		if g.fetches > 6 {
			return "Research documentation budget reached (6 fetches). Reuse evidence and record unknowns", nil
		}
		return "", nil
	}
	if g.base != nil {
		return g.base.Permit(ctx, name, raw, readOnly, t)
	}
	return "", nil
}

type runtimeGuard struct{ runtime *Runtime }

func (g runtimeGuard) Permit(ctx context.Context, name string, args json.RawMessage, readOnly bool, t audit.Transcript) (string, error) {
	g.runtime.permissionMu.Lock()
	defer g.runtime.permissionMu.Unlock()
	if g.runtime.Guard == nil {
		return "", nil
	}
	return g.runtime.Guard.Permit(ctx, name, args, readOnly, t)
}
