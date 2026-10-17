package greenfield

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"censi/harness/internal/agent"
	"censi/harness/internal/audit"
	"censi/harness/internal/llm"
	"censi/harness/internal/session"
	"censi/harness/internal/tools"
)

// Broad project commands are allowed after plan approval. High-impact operations
// still use the ordinary approval guard; this is Git isolation, not an OS sandbox.
var outsideCommand = regexp.MustCompile(`(?i)(\.\.[/\\]|~[/\\]|/(?:Users|home|etc|root|private|tmp|var|Applications|Volumes)/|\b(?:curl|wget|ssh|scp|sftp|rsync|security|sudo)\b|\.maxlabs|\.git[/\\])`)

type flexibleGuard struct {
	root    string
	qa      bool
	base    agent.Guard
	fetches int
}

func (g *flexibleGuard) Permit(ctx context.Context, name string, args json.RawMessage, readOnly bool, t audit.Transcript) (string, error) {
	var input map[string]any
	if json.Unmarshal(args, &input) != nil {
		return "invalid tool arguments", nil
	}
	if name == "team" || name == "task" {
		return "workers cannot control workflow state or delegate", nil
	}
	if name == "write" || name == "edit" {
		if g.qa {
			return "independent QA cannot edit application source", nil
		}
		if _, err := SafeWorkerPath(g.root, text(input, "path")); err != nil {
			return err.Error(), nil
		}
		return "", nil
	}
	if name == "webfetch" {
		g.fetches++
		if g.fetches > 6 {
			return "documentation budget reached", nil
		}
		return "", nil
	}
	if name == "bash" || name == "verify" {
		normalized, _ := json.Marshal(map[string]any{"command": text(input, "command")})
		decision := audit.Assess("bash", normalized, t)
		if decision.RefusedNow() {
			return decision.Explain(), nil
		}
		fresh, _ := audit.RequiresFreshApproval("bash", normalized)
		if fresh || outsideCommand.MatchString(text(input, "command")) {
			if g.base != nil {
				return g.base.Permit(ctx, "bash", normalized, false, t)
			}
			return "this command requires a human approval guard", nil
		}
		return "", nil
	}
	if name == "read" {
		if _, err := SafeWorkerPath(g.root, text(input, "path")); err != nil {
			return err.Error(), nil
		}
	}
	return "", nil
}

type verificationTool struct {
	shell  tools.Tool
	record func(string, tools.Result) error
}

func (t verificationTool) Definition() tools.Definition {
	return tools.Definition{Name: "verify", Description: "Run a real acceptance, test, build, or lint command. The runner records the command, exit status, and output for independent QA. Choose commands appropriate to the actual project; do not substitute echo, version probes, skipped tests, or mocks for acceptance.", Parameters: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"},"timeout_ms":{"type":"integer"}},"required":["command"],"additionalProperties":false}`)}
}
func (t verificationTool) Execute(ctx context.Context, args json.RawMessage) (tools.Result, error) {
	var input map[string]any
	_ = json.Unmarshal(args, &input)
	result, err := t.shell.Execute(ctx, args)
	if err != nil {
		return result, err
	}
	if err = t.record(text(input, "command"), result); err != nil {
		return tools.Result{}, err
	}
	return result, nil
}

type flexibleJob struct {
	id, root, branch, baseCommit string
	task                         map[string]any
	run                          *Run
	packet                       map[string]any
}

func (r *Runtime) runFlexible(ctx context.Context, id string) (tools.Result, error) {
	return r.runFlexibleWave(ctx, []string{id})
}
func (r *Runtime) runFlexibleWave(ctx context.Context, ids []string) (tools.Result, error) {
	var output tools.Result
	err := r.Store.Locked(func(state *State) error {
		status, err := r.Store.Status(state)
		if err != nil {
			return err
		}
		if len(ids) == 0 || len(ids) > 3 || r.workersUsed+len(ids) > 8 {
			return fmt.Errorf("parallel wave exceeds budget")
		}
		seen := map[string]bool{}
		for _, id := range ids {
			if seen[id] {
				return fmt.Errorf("duplicate task in parallel wave")
			}
			seen[id] = true
			if len(ids) > 1 {
				for _, a := range state.Artifacts {
					if a.Kind == "task" && a.ID == id {
						d, e := r.Store.Content(a)
						if e != nil {
							return e
						}
						if serial, _ := d["serial"].(bool); serial {
							return fmt.Errorf("serial tasks must run alone")
						}
					}
				}
			}
			found := false
			for _, ready := range status.Ready {
				if ready == id {
					found = true
				}
			}
			if !found {
				return fmt.Errorf("task %s is not ready", id)
			}
		}
		integration, err := r.Store.prepareExecution(ctx, state)
		if err != nil {
			return err
		}
		jobs := []flexibleJob{}
		briefs := []map[string]any{}
		tasks := map[string]map[string]any{}
		evidence := map[string]any{}
		for _, a := range state.Artifacts {
			d, err := r.Store.Content(a)
			if err != nil {
				return err
			}
			if a.Kind == "task" {
				tasks[a.ID] = d
			}
			if a.Kind == "brief" || a.Kind == "spec" || a.Kind == "design" || a.Kind == "integration" {
				delete(d, "prototype_html")
				briefs = append(briefs, map[string]any{"kind": a.Kind, "content": d})
			}
		}
		for id, previous := range state.Runs {
			checks := []Check{}
			for _, check := range previous.Checks {
				check.Output = truncate(check.Output, 1600)
				checks = append(checks, check)
			}
			evidence[id] = map[string]any{"status": previous.Status, "summary": truncate(previous.Output, 1000), "checks": checks, "branch": previous.Branch}
		}
		for _, id := range ids {
			task := tasks[id]
			previous := state.Runs[id]
			attempt := 1
			if previous != nil {
				attempt += previous.Attempts
			}
			run := &Run{ID: fmt.Sprintf("%d", timeNow()), Status: "running", Attempts: attempt, Checks: []Check{}}
			relative := filepath.ToSlash(filepath.Join(filepath.Dir(state.Execution.Path), "workers", id+"-"+run.ID))
			root, err := r.Store.Path(relative)
			if err != nil {
				return err
			}
			branch := "task-" + id + "-" + run.ID
			base := "HEAD"
			if previous != nil && previous.Status != "passed" && previous.Commit != "" {
				base = previous.Commit
			}
			if _, err = gitCommand(ctx, integration, "worktree", "add", "-b", branch, root, base); err != nil {
				return err
			}
			packet := map[string]any{"task": task, "briefs": briefs, "workspace": root, "branch": branch, "implementation_evidence": evidence}
			if previous != nil {
				packet["previous_attempt"] = map[string]any{"status": previous.Status, "summary": truncate(previous.Output, 1600), "branch": previous.Branch}
			}
			if base != "HEAD" {
				if _, err = gitCommand(ctx, root, "merge", "--no-edit", state.Execution.Commit); err != nil {
					conflicts, _ := gitCommand(ctx, root, "diff", "--name-only", "--diff-filter=U")
					packet["merge_conflicts"] = conflicts
				}
			}
			if text(task, "role") == "qa" {
				diff, _ := gitCommand(ctx, root, "diff", "--stat", state.Execution.BaseCommit, "HEAD")
				packet["change_summary"] = diff
			}
			run.Branch = branch
			run.Workspace = relative
			state.Runs[id] = run
			jobs = append(jobs, flexibleJob{id: id, root: root, branch: branch, baseCommit: state.Execution.BaseCommit, task: task, run: run, packet: packet})
		}
		r.workersUsed += len(jobs)
		delete(state.Approvals, "release")
		state.Execution.VerifiedFiles = nil
		if err = r.Store.Persist(state); err != nil {
			return err
		}
		type finished struct {
			job flexibleJob
			run *Run
			err error
		}
		results := make(chan finished, len(jobs))
		for _, job := range jobs {
			go func(job flexibleJob) {
				copy := *job.run
				job.run = &copy
				run, err := r.executeFlexibleJob(ctx, job)
				results <- finished{job, run, err}
			}(job)
		}
		receipts := []map[string]any{}
		failed := false
		for range jobs {
			done := <-results
			run := done.run
			if done.err != nil {
				run.Status = "failed"
				run.Output = done.err.Error()
			}
			if run.Status == "passed" {
				if text(done.job.task, "role") == "qa" {
					verified, err := sourceFiles(done.job.root)
					if err != nil {
						return err
					}
					combined, err := sourceFiles(integration)
					if err != nil {
						return err
					}
					if !sameFiles(verified, combined) {
						run.Status = "failed"
						run.Output = "Integration changed during QA; verify the combined result again."
					} else {
						state.Execution.VerifiedFiles = verified
					}
				} else {
					if _, err = gitCommand(ctx, integration, "merge", "--no-edit", "--no-ff", run.Branch); err != nil {
						_, _ = gitCommand(ctx, integration, "merge", "--abort")
						run.Status = "failed"
						run.Output = "Merge conflict; task branch preserved for resolution.\n" + err.Error()
					} else {
						state.Execution.Commit, _ = gitCommand(ctx, integration, "rev-parse", "HEAD")
						run.Merged = true
					}
				}
			}
			state.Runs[done.job.id] = run
			if err = r.Store.archiveRun(done.job.id, run); err != nil {
				return err
			}
			if err = r.Store.Persist(state); err != nil {
				return err
			}
			if run.Status != "passed" {
				failed = true
			}
			receipts = append(receipts, map[string]any{"id": done.job.id, "status": run.Status, "branch": run.Branch, "merged": run.Merged, "output": truncate(run.Output, 1600), "checks": run.Checks})
		}
		output = jsonResult(map[string]any{"results": receipts, "workspace": integration, "branch": state.Execution.Branch})
		output.IsError = failed
		return nil
	})
	return output, err
}
func (r *Runtime) executeFlexibleJob(ctx context.Context, job flexibleJob) (*Run, error) {
	id, root, task, run := job.id, job.root, job.task, job.run
	started := time.Now()
	defer func() { run.DurationMS = time.Since(started).Milliseconds() }()
	run.Usage = &RunUsage{}
	attempt := run.Attempts
	ws, err := tools.NewWorkspace(root)
	if err != nil {
		return run, err
	}
	qa := text(task, "role") == "qa"
	before, err := sourceFiles(root)
	if err != nil {
		return run, err
	}
	registry := tools.NewRegistry()
	shell := tools.NewBashTool(ws).WithEnvironment(workerEnvironment())
	available := []tools.Tool{tools.NewReadTool(ws), tools.NewGlobTool(ws), tools.NewGrepTool(ws), shell, tools.NewWebFetchTool()}
	if !qa {
		available = append(available, tools.NewWriteTool(ws), tools.NewEditTool(ws))
	}
	verify := verificationTool{shell: shell, record: func(command string, result tools.Result) error {
		check, err := r.Store.recordCheck(id+"-"+run.ID, attempt, len(run.Checks)+1, command, resultText(result), !result.IsError)
		if err == nil {
			run.Checks = append(run.Checks, check)
		}
		return err
	}}
	available = append(available, verify)
	for _, tool := range available {
		if err = registry.Register(tool); err != nil {
			return run, err
		}
	}
	packet := job.packet
	encoded, _ := json.Marshal(packet)
	if len(encoded) > 96000 {
		return run, fmt.Errorf("task brief exceeds context budget; split or shorten it")
	}
	assembly := r.Assembly
	assembly.Tools = registry.Schemas()
	if r.WorkerModel != "" {
		assembly.Model = r.WorkerModel
	}
	limit := r.MaxSteps
	if limit <= 0 || limit > 40 {
		limit = 40
	}
	guard := &flexibleGuard{root: root, qa: qa, base: runtimeGuard{r}}
	worker, err := agent.New(agent.Config{Assembly: assembly, Provider: r.Provider, Tools: registry, Guard: guard, MaxSteps: limit, MaxRetries: 2, MaxToolResultBytes: 16000}, session.New(nil))
	if err != nil {
		return run, err
	}
	usageHook := func(usage llm.Usage) {
		run.Usage.Add(usage)
		if r.OnUsage != nil {
			r.progressMu.Lock()
			r.OnUsage(usage)
			r.progressMu.Unlock()
		}
	}
	worker.SetUsageHook(usageHook)
	worker.SetToolResultHook(func(call session.Block, value tools.Result) {
		if r.OnTool != nil {
			r.progressMu.Lock()
			r.OnTool(call, value)
			r.progressMu.Unlock()
		}
	})
	instruction := "Deliver the supplied outcomes in this isolated Git working branch. Inspect the actual repository and project instructions. You choose files, implementation, dependency installation, and shell commands; listed read/write/check paths are suggestions, not a file allowlist. Use the approved stack and decisions; when prose conflicts, the approved specification wins. Scaffold using official tools in staging if an existing partial project prevents an in-place generator; preserve useful existing work. You may investigate and repair test failures within this task. Do not publish, deploy, change workflow metadata, or access other projects. Use verify for real outcome checks; its output is recorded by the runner. Finish ONLY with JSON {\"outcome\":\"completed\" or \"blocked\",\"summary\":\"...\",\"blockers\":[]}. A completed report without recorded passing verification is blocked."
	if qa {
		instruction = "Independently verify the supplied outcomes. Inspect the actual diff and source, look for regressions, missing behavior, weakened tests, and contradictory evidence. Run your own checks with verify; do not merely trust implementation reports. Do not edit application source. Finish ONLY with JSON {\"outcome\":\"completed\" or \"blocked\",\"summary\":\"...\",\"blockers\":[]}. Report concrete gaps, not assumed success.\n" + instruction
	}
	turn, runErr := worker.Run(ctx, instruction+"\n\n"+string(encoded))
	if runErr == nil && reportOutcome(turn.Text) == "" {
		repairAssembly := assembly
		repairAssembly.Tools = nil
		repairAssembly.MaxTokens = 2048
		repair, e := agent.New(agent.Config{Assembly: repairAssembly, Provider: r.Provider, Tools: tools.NewRegistry(), MaxSteps: 1, OnUsage: usageHook}, session.New(nil))
		if e == nil {
			fixed, e := repair.Run(ctx, "Format this report as ONLY JSON with outcome (completed or blocked), summary, blockers. Preserve its outcome and blockers; if ambiguous use blocked. No tools. Report:\n"+truncate(turn.Text, 16000))
			if e == nil {
				turn.Text = fixed.Text
			}
		}
	}
	run.Output = truncate(turn.Text, 48000)
	run.Status = "failed"
	after, scanErr := sourceFiles(root)
	if runErr != nil {
		run.Output = runErr.Error()
	} else if scanErr != nil {
		run.Output = scanErr.Error()
	} else if qa && !sameFiles(before, after) {
		run.Output = "Independent QA changed application source; review these changes before retrying."
	} else if CompletedReport(turn.Text) && verifiedChecks(run.Checks) {
		run.Status = "passed"
	}
	if ctx.Err() != nil {
		run.Status = "interrupted"
	}
	if !qa && scanErr == nil {
		if run.Commit, err = checkpoint(ctx, root, &ExecutionWorkspace{}, "Team task "+id+": "+run.Status); err != nil {
			run.Status = "failed"
			run.Output += "\n" + err.Error()
		}
	}

	if run.Status != "passed" && strings.TrimSpace(run.Output) == "" {
		run.Output = "Worker must return a completion report and record at least one passing verify command."
	}
	return run, nil
}
func verifiedChecks(checks []Check) bool {
	if len(checks) == 0 {
		return false
	}
	latest := map[string]bool{}
	for _, check := range checks {
		latest[check.Command] = check.Passed
	}
	for _, passed := range latest {
		if !passed {
			return false
		}
	}
	return true
}
func timeNow() int64 { return time.Now().UnixNano() }
