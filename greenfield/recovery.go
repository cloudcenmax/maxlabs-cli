package greenfield

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"censi/harness/internal/agent"
	"censi/harness/internal/llm"
	"censi/harness/internal/session"
	"censi/harness/internal/tools"
)

type WorkerAction struct {
	Tool   string `json:"tool"`
	Target string `json:"target"`
	Output string `json:"output"`
	Failed bool   `json:"failed"`
}
type RecoveryPlan struct {
	Action    string   `json:"action"`
	Summary   string   `json:"summary"`
	NextSteps []string `json:"next_steps"`
}
type Recovery struct {
	Plan       RecoveryPlan `json:"plan"`
	SourceHash string       `json:"source_hash"`
	Checks     []Check      `json:"checks"`
	Usage      RunUsage     `json:"usage"`
	Archive    string       `json:"archive"`
}

func sourceHash(root string) (string, error) {
	files, err := sourceFiles(root)
	if err != nil {
		return "", err
	}
	if _, laravel := files["artisan"]; laravel {
		for path := range files {
			if recoveryRuntimeFile(path) {
				delete(files, path)
			}
		}
	}
	keys := make([]string, 0, len(files))
	for key := range files {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var raw strings.Builder
	for _, key := range keys {
		raw.WriteString(key)
		raw.WriteByte(0)
		raw.WriteString(files[key])
		raw.WriteByte('\n')
	}
	return hash([]byte(raw.String())), nil
}

func recoveryRuntimeFile(path string) bool {
	if filepath.Base(path) == ".gitignore" {
		return false
	}
	for _, prefix := range []string{"storage/logs/", "storage/framework/cache/", "storage/framework/sessions/", "storage/framework/views/", "bootstrap/cache/"} {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return path == "database/database.sqlite" || strings.HasPrefix(path, "database/database.sqlite-")
}

func parseRecovery(raw string) (RecoveryPlan, error) {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimSuffix(raw, "```")
	var plan RecoveryPlan
	if json.Unmarshal([]byte(raw), &plan) != nil || (plan.Action != "continue" && plan.Action != "blocked") || strings.TrimSpace(plan.Summary) == "" || len(plan.Summary) > 2400 || len(plan.NextSteps) > 6 {
		return plan, fmt.Errorf("coordinator did not return a valid recovery diagnosis")
	}
	if plan.Action == "continue" && len(plan.NextSteps) == 0 {
		return plan, fmt.Errorf("coordinator continuation has no concrete next steps")
	}
	for _, step := range plan.NextSteps {
		if strings.TrimSpace(step) == "" || len(step) > 1600 {
			return plan, fmt.Errorf("coordinator next steps are invalid")
		}
	}
	return plan, nil
}
func (r *Runtime) recoverBudgetTask(ctx context.Context, id string) error {
	return r.Store.Locked(func(state *State) error {
		run := state.Runs[id]
		if run == nil || run.Status != "failed" || run.StopReason != "step_limit" {
			return nil
		}
		status, err := r.Store.Status(state)
		if err != nil {
			return err
		}
		ready := false
		for _, candidate := range status.Ready {
			if candidate == id {
				ready = true
			}
		}
		if !ready || run.Attempts >= 3 {
			return fmt.Errorf("task %s has exhausted its approved attempts or is no longer ready", id)
		}
		root, err := r.Store.Path(run.Workspace)
		if err != nil || run.Workspace == "" {
			return fmt.Errorf("preserved worker workspace is unavailable")
		}
		if state.Execution == nil {
			return fmt.Errorf("execution workspace is unavailable")
		}
		before, err := sourceHash(root)
		if err != nil {
			return err
		}
		if run.Recovery != nil {
			if run.Recovery.Plan.Action != "continue" {
				return fmt.Errorf("coordinator needs attention for %s: %s", id, run.Recovery.Plan.Summary)
			}
			if before != run.Recovery.SourceHash {
				return fmt.Errorf("worker source changed after coordinator review; review the preserved branch before retrying")
			}
			return nil
		}
		if r.recoveryUsed >= 3 {
			return fmt.Errorf("coordinator recovery budget reached (three diagnostic passes per batch)")
		}
		r.recoveryUsed++
		var task map[string]any
		briefs := []map[string]any{}
		for _, a := range state.Artifacts {
			if a.Kind == "task" && a.ID == id {
				task, err = r.Store.Content(a)
				if err != nil {
					return err
				}
			}
			if a.Kind == "brief" || a.Kind == "spec" {
				d, e := r.Store.Content(a)
				if e != nil {
					return e
				}
				briefs = append(briefs, map[string]any{"kind": a.Kind, "content": d})
			}
		}
		if r.OnTool != nil {
			r.OnTool(session.ToolCall("coordinator-start-"+id, "team", json.RawMessage(`{"action":"recovery"}`)), tools.Text("Coordinator is inspecting the preserved branch for "+id+" before continuing."))
		}
		recovery := &Recovery{Checks: []Check{}}
		recoveryID := fmt.Sprintf("%s-%s-%d", id, run.ID, timeNow())
		ws, err := tools.NewWorkspace(root)
		if err != nil {
			return err
		}
		registry := tools.NewRegistry()
		shell := tools.NewBashTool(ws).WithEnvironment(workerEnvironment())
		verify := verificationTool{shell: shell, record: func(command string, result tools.Result) error {
			check, e := r.Store.recordCheck("recovery-"+recoveryID, 1, len(recovery.Checks)+1, command, resultText(result), !result.IsError)
			if e == nil {
				recovery.Checks = append(recovery.Checks, check)
			}
			return e
		}}
		for _, tool := range []tools.Tool{tools.NewReadTool(ws), tools.NewGlobTool(ws), tools.NewGrepTool(ws), verify} {
			if err = registry.Register(tool); err != nil {
				return err
			}
		}
		checks := []Check{}
		for _, check := range run.Checks {
			check.Output = truncate(check.Output, 2400)
			checks = append(checks, check)
		}
		if len(checks) > 3 {
			checks = checks[len(checks)-3:]
		}
		diff, _ := gitCommand(ctx, root, "diff", "--stat", state.Execution.BaseCommit, "HEAD")
		packet := map[string]any{"task": task, "briefs": briefs, "workspace": root, "branch": run.Branch, "prior_summary": truncate(run.Output, 4000), "recent_actions": run.RecentActions, "checks": checks, "change_summary": diff, "attempt": run.Attempts}
		raw, _ := json.Marshal(packet)
		if len(raw) > 64000 {
			return fmt.Errorf("recovery context exceeds budget; refine the task before continuing")
		}
		assembly := r.Assembly
		assembly.Tools = registry.Schemas()
		coordinator, err := agent.New(agent.Config{Assembly: assembly, Provider: r.Provider, Tools: registry, Guard: &flexibleGuard{root: root, qa: true, base: runtimeGuard{r}}, MaxSteps: 4, MaxRetries: 1, MaxToolResultBytes: 6000}, session.New(nil))
		if err != nil {
			return err
		}
		investigated := false
		coordinator.SetUsageHook(func(usage llm.Usage) {
			recovery.Usage.Add(usage)
			if r.OnUsage != nil {
				r.progressMu.Lock()
				r.OnUsage(usage)
				r.progressMu.Unlock()
			}
		})
		coordinator.SetToolResultHook(func(call session.Block, result tools.Result) {
			if !result.IsError {
				investigated = true
			}
			if r.OnTool != nil {
				r.progressMu.Lock()
				r.OnTool(call, result)
				r.progressMu.Unlock()
			}
		})
		turn, runErr := coordinator.Run(ctx, Contract.RecoveryInstructions+"\n\n"+string(raw))
		plan, parseErr := parseRecovery(turn.Text)
		after, scanErr := sourceHash(root)
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case scanErr != nil:
			return scanErr
		case after != before:
			plan = RecoveryPlan{Action: "blocked", Summary: "Diagnostic coordinator changed application source; review the preserved branch before continuing.", NextSteps: []string{}}
		case runErr != nil:
			plan = RecoveryPlan{Action: "blocked", Summary: "Coordinator diagnosis could not finish: " + runErr.Error(), NextSteps: []string{}}
		case parseErr != nil:
			plan = RecoveryPlan{Action: "blocked", Summary: parseErr.Error(), NextSteps: []string{}}
		case plan.Action == "continue" && !investigated && len(recovery.Checks) == 0:
			plan = RecoveryPlan{Action: "blocked", Summary: "Coordinator did not inspect source or run a diagnostic check; refusing a blind retry.", NextSteps: []string{}}
		}
		recovery.Plan = plan
		recovery.SourceHash = after
		recovery.Archive = filepath.ToSlash(filepath.Join(".maxlabs/team/recovery", recoveryID+".json"))
		target, err := r.Store.Path(strings.TrimPrefix(recovery.Archive, ".maxlabs/team/"))
		if err != nil {
			return err
		}
		if err = os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return err
		}
		saved, _ := json.MarshalIndent(recovery, "", "  ")
		if err = os.WriteFile(target, saved, 0600); err != nil {
			return err
		}
		run.Recovery = recovery
		if err = r.Store.Persist(state); err != nil {
			return err
		}
		if r.OnTool != nil {
			r.OnTool(session.ToolCall("recovery-"+recoveryID, "team", json.RawMessage(`{"action":"recovery"}`)), jsonResult(map[string]any{"id": id, "diagnosis": plan, "usage": recovery.Usage}))
		}
		if plan.Action != "continue" {
			return fmt.Errorf("coordinator needs attention for %s: %s", id, plan.Summary)
		}
		return nil
	})
}

func captureWorkerAction(root string, call session.Block, result tools.Result) WorkerAction {
	var args map[string]any
	_ = json.Unmarshal(call.Arguments, &args)
	target := text(args, "command")
	if target == "" {
		target = text(args, "path")
	}
	target = strings.ReplaceAll(target, root, ".")
	target = strings.TrimPrefix(target, "cd . && ")
	output, _ := tools.BoundText(strings.ReplaceAll(resultText(result), root, "."), 2400, 1200)
	return WorkerAction{Tool: call.Name, Target: truncate(target, 1024), Output: output, Failed: result.IsError}
}
func budgetHandoff(run *Run) string {
	summary := fmt.Sprintf("Worker reached its %d-step budget. Work is checkpointed on %s for coordinator diagnosis.", run.StepLimit, run.Branch)
	for i := len(run.RecentActions) - 1; i >= 0; i-- {
		a := run.RecentActions[i]
		if a.Failed {
			return summary + "\nLatest failure: " + a.Tool + " " + a.Target + "\n" + a.Output
		}
	}
	if len(run.RecentActions) > 0 {
		a := run.RecentActions[len(run.RecentActions)-1]
		summary += "\nLatest action: " + a.Tool + " " + a.Target + "\n" + a.Output
	}
	return summary
}
