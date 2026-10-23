package greenfield

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	goruntime "runtime"
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
var outsideCommand = regexp.MustCompile(`(?i)(\.\.[/\\]|~[/\\]|/(?:Users|home|etc|root|private|tmp|var|Applications|Volumes)(?:[/\s]|$)|\b(?:curl|wget|ssh|scp|sftp|rsync|security|sudo)\b|\.maxlabs|\.git[/\\]|\b(?:npm|pnpm|yarn|bun)\s+publish\b)`)

type flexibleGuard struct {
	root     string
	qa       bool
	qaBefore map[string]string
	base     agent.Guard
	fetches  int
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
		if g.qa && !qaWritable(g.root, text(input, "path"), g.qaBefore) {
			return "Independent QA may add or refine new regression tests, but cannot change application source or pre-existing tests", nil
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
		if !ProjectAutoCommand(g.root, text(input, "command")) {
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
	record func(string, tools.Result, bool) error
}

func (t verificationTool) Definition() tools.Definition {
	return tools.Definition{Name: "verify", Description: "Run a real acceptance, test, build, or lint command. Set final:true only for comprehensive verification of this task after all repairs; a passing final run supersedes earlier failed attempts without deleting their logs. The runner records actual exit status for independent QA. Echo, version probes, skipped tests, and installation are not acceptance.", Parameters: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"},"timeout_ms":{"type":"integer"},"final":{"type":"boolean"}},"required":["command"],"additionalProperties":false}`)}
}
func (t verificationTool) Execute(ctx context.Context, args json.RawMessage) (tools.Result, error) {
	var input map[string]any
	if err := json.Unmarshal(args, &input); err != nil || input == nil || strings.TrimSpace(text(input, "command")) == "" {
		return tools.Error("Verification requires a command."), nil
	}
	originalCommand := text(input, "command")
	var result tools.Result
	var err error
	if goruntime.GOOS == "windows" && strings.Contains(text(input, "command"), "|") {
		result = tools.Error("MaxLabs verification requires an unpiped command on Windows so a failed test cannot be hidden by an output filter.")
	} else {
		execution := input
		if goruntime.GOOS != "windows" {
			execution["command"] = "set -e\n" + text(input, "command")
		}
		shellArgs, _ := json.Marshal(execution)
		result, err = t.shell.Execute(ctx, shellArgs)
	}
	if err != nil {
		return result, err
	}
	final, _ := input["final"].(bool)
	if err = t.record(originalCommand, result, final); err != nil {
		return tools.Result{}, err
	}
	return result, nil
}

type flexibleJob struct {
	id, root, branch, baseCommit string
	task                         map[string]any
	run                          *Run
	packet                       map[string]any
	contextSources               map[string]string
}

func (r *Runtime) runFlexible(ctx context.Context, id string) (tools.Result, error) {
	return r.runFlexibleWave(ctx, []string{id})
}
func (r *Runtime) runFlexibleWave(ctx context.Context, ids []string) (tools.Result, error) {
	var output tools.Result
	if len(ids) == 0 || len(ids) > 3 || r.workersUsed+len(ids) > 8 {
		return output, fmt.Errorf("parallel wave exceeds budget")
	}
	seenIDs := map[string]bool{}
	for _, id := range ids {
		if seenIDs[id] {
			return output, fmt.Errorf("duplicate task in parallel wave")
		}
		seenIDs[id] = true
	}
	for _, id := range ids {
		if err := r.recoverBudgetTask(ctx, id); err != nil {
			return output, err
		}
	}
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
		contextSources := map[string]string{}
		for _, a := range state.Artifacts {
			d, err := r.Store.Content(a)
			if err != nil {
				return err
			}
			rawContext, _ := json.Marshal(d)
			contextSources["artifact/"+a.Kind+"/"+a.ID] = string(rawContext)
			if a.Kind == "task" {
				tasks[a.ID] = d
			}
			if a.Kind == "brief" || a.Kind == "spec" || a.Kind == "design" || a.Kind == "integration" {
				delete(d, "prototype_html")
				briefs = append(briefs, map[string]any{"kind": a.Kind, "content": d})
			}
		}
		for id, previous := range state.Runs {
			rawContext, _ := json.Marshal(previous)
			contextSources["run/"+id] = string(rawContext)
			for ci, check := range previous.Checks {
				rawCheck, _ := json.Marshal(check)
				contextSources[fmt.Sprintf("check/%s/%d", id, ci)] = string(rawCheck)
			}
			checks := compactChecks(previous.Checks)
			if previous.Status == "passed" && len(checks) > 0 {
				checks = checks[len(checks)-1:]
			}
			for i := range checks {
				checks[i].Command = truncate(checks[i].Command, 256)
			}
			evidence[id] = map[string]any{"status": previous.Status, "summary": truncate(previous.Output, 240), "checks": checks, "check_count": len(previous.Checks), "context_key": "run/" + id, "check_context_prefix": "check/" + id + "/", "branch": previous.Branch, "stop_reason": previous.StopReason, "steps_used": previous.StepsUsed}
		}
		for _, id := range ids {
			task := tasks[id]
			previous := state.Runs[id]
			attempt := 1
			if previous != nil {
				attempt += previous.Attempts
				if previous.Status == "paused" {
					attempt--
				}
			}
			run := &Run{ID: fmt.Sprintf("%d", timeNow()), Status: "running", Attempts: attempt, Checks: []Check{}}
			if previous != nil {
				run.QARepairRounds = previous.QARepairRounds
				run.RepairFrom = previous.RepairFrom
				if previous.Status == "paused" {
					run.Checks = append([]Check{}, previous.Checks...)
					run.QATestFiles = append([]string{}, previous.QATestFiles...)
				}
			}
			relative := filepath.ToSlash(filepath.Join(filepath.Dir(state.Execution.Path), "workers", id+"-"+run.ID))
			root, err := r.Store.Path(relative)
			if err != nil {
				return err
			}
			branch := "task-" + id + "-" + run.ID
			// A stopped older build may have left work uncommitted. Preserve it
			// before branching a retry instead of silently starting from integration.
			if previous != nil && previous.Commit == "" && previous.Workspace != "" {
				oldRoot, e := r.Store.Path(previous.Workspace)
				if e != nil {
					return e
				}
				previous.Commit, e = checkpoint(ctx, oldRoot, &ExecutionWorkspace{}, "Preserve interrupted task "+id)
				if e != nil {
					return fmt.Errorf("preserve interrupted task %s: %w", id, e)
				}
				if e = r.Store.Persist(state); e != nil {
					return e
				}
			}
			base := "HEAD"
			if previous != nil && previous.Status != "passed" && previous.Commit != "" {
				base = previous.Commit
			}
			resume := previous != nil && previous.StopReason == "step_limit" && previous.Recovery != nil && previous.Recovery.Plan.Action == "continue"
			if previous != nil && previous.Status == "paused" && previous.Workspace != "" {
				relative = previous.Workspace
				root, err = r.Store.Path(relative)
				if err != nil {
					return err
				}
				branch = previous.Branch
			} else if resume {
				run.RecoveryTrail = append(append([]*Recovery{}, previous.RecoveryTrail...), previous.Recovery)
				relative = previous.Workspace
				root, err = r.Store.Path(relative)
				if err != nil {
					return err
				}
				branch = previous.Branch
				current, e := sourceHash(root)
				if e != nil {
					return e
				}
				if current != previous.Recovery.SourceHash {
					return fmt.Errorf("worker changed after recovery review")
				}
				if _, err = checkpoint(ctx, root, &ExecutionWorkspace{}, "Preserve coordinator continuation "+id); err != nil {
					return err
				}
			} else if _, err = gitCommand(ctx, integration, "worktree", "add", "-b", branch, root, base); err != nil {
				return err
			}
			packet := map[string]any{"task": task, "briefs": briefs, "workspace": root, "branch": branch, "implementation_evidence": evidence, "context_index": contextIndex(contextSources)}
			if previous != nil {
				prior := map[string]any{}
				for key, value := range evidence[id].(map[string]any) {
					prior[key] = value
				}
				prior["summary"] = truncate(previous.Output, 1600)
				prior["recent_actions"] = previous.RecentActions
				packet["previous_attempt"] = prior
				if previous.RepairFrom != "" {
					packet["qa_repair"] = map[string]any{"qa_id": previous.RepairFrom, "instruction": "Repair in-scope QA defects. Preserve failing regression tests and run comprehensive final verification; do not weaken tests or change approved requirements."}
				}
				if resume {
					packet["recovery_plan"] = previous.Recovery.Plan
				}
			}
			if base != "HEAD" {
				if _, err = gitCommand(ctx, root, "merge", "--no-edit", state.Execution.Commit); err != nil {
					conflicts, _ := gitCommand(ctx, root, "diff", "--name-only", "--diff-filter=U")
					packet["merge_conflicts"] = conflicts
				}
			}
			if text(task, "role") == "qa" {
				diff, summaryErr := applicationChangeSummary(ctx, root, state.Execution.BaseCommit)
				if summaryErr != nil {
					return summaryErr
				}
				packet["change_summary"] = diff
			}
			run.Branch = branch
			run.Workspace = relative
			state.Runs[id] = run
			jobs = append(jobs, flexibleJob{id: id, root: root, branch: branch, baseCommit: state.Execution.BaseCommit, task: task, run: run, packet: packet, contextSources: contextSources})
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
					verified, err := recoverySourceFiles(done.job.root)
					if err != nil {
						return err
					}
					combined, err := recoverySourceFiles(integration)
					if err != nil {
						return err
					}
					valid, _ := qaSourceChanges(combined, verified)
					if !valid {
						run.Status = "failed"
						run.Output = "Integration changed during QA; verify the combined result again."
					} else {
						if len(run.QATestFiles) > 0 {
							if _, err = gitCommand(ctx, integration, "merge", "--no-edit", "--no-ff", run.Branch); err != nil {
								run.Status = "failed"
								run.Output = "QA test merge needs review: " + err.Error()
							} else {
								state.Execution.Commit, _ = gitCommand(ctx, integration, "rev-parse", "HEAD")
							}
						}
						if run.Status == "passed" {
							state.Execution.VerifiedFiles, err = sourceFiles(integration)
							if err != nil {
								return err
							}
						}
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
			receipts = append(receipts, map[string]any{"id": done.job.id, "status": run.Status, "branch": run.Branch, "merged": run.Merged, "output": truncate(run.Output, 4000), "stop_reason": run.StopReason, "steps_used": run.StepsUsed, "checks": run.Checks})
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
	if qa {
		before, err = recoverySourceFiles(root)
	}
	if err != nil {
		return run, err
	}
	registry := tools.NewRegistry()
	shell := tools.NewBashTool(ws).WithEnvironment(workerEnvironment())
	available := []tools.Tool{tools.NewReadTool(ws), tools.NewGlobTool(ws), tools.NewGrepTool(ws), shell, tools.NewWebFetchTool()}
	available = append(available, tools.NewWriteTool(ws), tools.NewEditTool(ws))
	verify := verificationTool{shell: shell, record: func(command string, result tools.Result, final bool) error {
		check, err := r.Store.recordCheck(id+"-"+run.ID, attempt, len(run.Checks)+1, command, resultText(result), !result.IsError)
		if err == nil {
			check.Final = final
			run.Checks = append(run.Checks, check)
		}
		return err
	}}
	available = append(available, verify, cachedContextTool{sources: job.contextSources})
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
	limit := workerStepLimit(r.MaxSteps)
	guard := &flexibleGuard{root: root, qa: qa, qaBefore: before, base: runtimeGuard{r}}
	run.StepLimit = limit
	workerLog := session.New(nil)
	worker, err := agent.New(agent.Config{InputSource: "runner", Assembly: assembly, Provider: r.Provider, Tools: registry, Guard: guard, MaxSteps: limit, MaxRetries: 2, MaxToolResultBytes: 16000, OnStep: func(step int) {
		run.StepsUsed = step
		if limit >= 4 && step == limit-2 {
			_, _ = workerLog.Append(session.EventUserMessage, session.Message{Role: session.RoleUser, Source: "runner", Content: []session.Block{session.Text("Runner budget notice: three model calls remain. Run final verification and return your report. If work remains, report concrete remaining work and blockers so a retry can continue from this branch.")}}, session.AppendOp())
		}
	}}, workerLog)
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
		run.RecentActions = append(run.RecentActions, captureWorkerAction(root, call, value))
		if len(run.RecentActions) > 4 {
			run.RecentActions = run.RecentActions[len(run.RecentActions)-4:]
		}
		if r.OnTool != nil {
			r.progressMu.Lock()
			r.OnTool(call, value)
			r.progressMu.Unlock()
		}
	})
	instruction := "Deliver the supplied outcomes in this isolated Git working branch. Inspect the actual repository and project instructions. You choose files, implementation, dependency installation, and shell commands; listed read/write/check paths are suggestions, not a file allowlist. Use the approved stack and decisions; when prose conflicts, the approved specification wins. Scaffold using official tools in staging if an existing partial project prevents an in-place generator; preserve useful existing work. You may investigate and repair test failures within this task. After repairs, use verify with final:true for comprehensive task acceptance; chain required commands with && so any failure is reported. Do not publish, deploy, change workflow metadata, or access other projects. Use verify for real outcome checks; its output is recorded by the runner. Keep test logs/reports in the system temporary directory or .cache so they do not alter delivery source. If the project uses .env, initialize it with cp -n .env.example .env rather than overwriting existing configuration; never print its contents. Finish ONLY with JSON {\"outcome\":\"completed\" or \"blocked\",\"summary\":\"...\",\"blockers\":[]}. A completed report without recorded passing verification is blocked."
	if qa {
		instruction = "Independently verify the supplied outcomes. Use team_context with context_index keys for detailed prior reports and checks. The change summary is a bounded navigation aid, not verification evidence. Inspect the actual diff and source, look for regressions, missing behavior, weakened tests, and contradictory evidence. Run your own checks with verify; do not merely trust implementation reports. You may add new regression tests, but must not change application source or pre-existing tests. Report observed defects with failing verify evidence so the orchestrator can dispatch repair. Finish ONLY with JSON {\"outcome\":\"completed\" or \"blocked\",\"summary\":\"...\",\"blockers\":[]}. Report concrete gaps, not assumed success.\n" + instruction
	}
	instruction += fmt.Sprintf(" Your budget is %d model steps; reserve the final steps for verification and a handoff. Fresh worktrees omit installed dependencies; install from the project manifests before testing. Verification output is already bounded: prefer unpiped checks, avoid head (which may interrupt the test), and rerun any failed check after repairs.", limit)
	turn, runErr := worker.Run(ctx, instruction+"\n\n"+string(encoded))
	if runErr == nil && reportOutcome(turn.Text) == "" {
		repairAssembly := assembly
		repairAssembly.Tools = nil
		repairAssembly.MaxTokens = 2048
		repair, e := agent.New(agent.Config{InputSource: "runner", Assembly: repairAssembly, Provider: r.Provider, Tools: tools.NewRegistry(), MaxSteps: 1, OnUsage: usageHook}, session.New(nil))
		if e == nil {
			fixed, e := repair.Run(ctx, "Format this report as ONLY JSON with outcome (completed or blocked), summary, blockers. Preserve its outcome and blockers; if ambiguous use blocked. No tools. Report:\n"+truncate(turn.Text, 16000))
			if e == nil {
				turn.Text = fixed.Text
			}
		}
	}
	run.Output = truncate(turn.Text, 48000)
	run.Status = "failed"
	var quota *llm.ServiceError
	after, scanErr := sourceFiles(root)
	if qa {
		after, scanErr = recoverySourceFiles(root)
	}
	if errors.As(runErr, &quota) && quota.Resumable() {
		run.Status = "paused"
		run.StopReason = "quota"
		run.QuotaWindow = quota.Window
		run.QuotaReset = quota.ResetsAt
		run.Output = quota.Error()
	} else if errors.Is(runErr, agent.ErrStepLimit) {
		run.StopReason = "step_limit"
		run.Output = budgetHandoff(run)
	} else if runErr != nil {
		run.Output = runErr.Error()
	} else if scanErr != nil {
		run.Output = scanErr.Error()
	} else if valid, _ := qaSourceChanges(before, after); qa && !valid {
		run.StopReason = "qa_source_changed"
		run.Output = "Independent QA changed application source or pre-existing tests; review the preserved branch before continuing."
	} else if CompletedReport(turn.Text) && verifiedChecks(run.Checks) {
		run.Status = "passed"
	} else if CompletedReport(turn.Text) {
		run.StopReason = "verification_failed"
		run.Output = "Completion was not accepted: verification is missing or earlier failures remain unresolved. After repairs, run comprehensive verify with final:true. All check logs are preserved.\nWorker report: " + run.Output
	}
	if ctx.Err() != nil {
		run.Status = "interrupted"
	}
	if qa && scanErr == nil {
		valid, added := qaSourceChanges(before, after)
		if !valid {
			run.Status = "failed"
			run.StopReason = "qa_source_changed"
			run.Output = "Independent QA changed application source or pre-existing tests; review the preserved branch before continuing."
		}
		if valid {
			run.QATestFiles = append(run.QATestFiles, added...)
			run.ReviewedSourceHash = recoveryFingerprint(after)
			if run.Status == "failed" && runErr == nil {
				for _, c := range run.Checks {
					if !c.Passed {
						run.StopReason = "qa_findings"
						break
					}
				}
			}
		}
	}
	if scanErr == nil && (!qa || run.StopReason != "qa_source_changed") {
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
	start := 0
	for i, check := range checks {
		if check.Final && check.Passed {
			start = i
		}
	}
	latest := map[string]bool{}
	for _, check := range checks[start:] {
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
