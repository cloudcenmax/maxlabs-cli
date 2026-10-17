package greenfield

import (
	"censi/harness/internal/llm"
	"censi/harness/internal/session"
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// RunApproved dispatches the checked DAG. Hard failures stop execution; budget
// stops receive bounded coordinator diagnosis and continuation.
func (r *Runtime) RunApproved(ctx context.Context) (string, int, error) {
	lines := []string{}
	count := 0
	for count < 8 {
		if err := ctx.Err(); err != nil {
			return strings.Join(lines, "\n"), count, err
		}
		if r.BeforeDispatch != nil {
			if err := r.BeforeDispatch(); err != nil {
				return strings.Join(lines, "\n"), count, err
			}
		}
		status, err := r.Store.Status(nil)
		if err != nil {
			return "", count, err
		}
		if len(status.Errors) > 0 {
			return "", count, fmt.Errorf("resolve readiness errors: %s", strings.Join(status.Errors, "; "))
		}
		if len(status.Ready) == 0 {
			break
		}
		if status.ExecutionMode == "flexible" {
			ids := []string{}
			for _, candidate := range status.Ready {
				serial := false
				for _, a := range status.Artifacts {
					if a.Kind == "task" && a.ID == candidate {
						d, e := r.Store.Content(a)
						if e != nil {
							return "", count, e
						}
						serial, _ = d["serial"].(bool)
					}
				}
				if serial && len(ids) > 0 {
					continue
				}
				ids = append(ids, candidate)
				if serial || len(ids) >= 3 || len(ids) >= 8-count {
					break
				}
			}
			result, err := r.runFlexibleWave(ctx, ids)
			count += len(ids)
			if err != nil {
				return "", count, err
			}
			if r.OnTool != nil {
				raw, _ := json.Marshal(map[string]any{"action": "run", "ids": ids})
				r.OnTool(session.ToolCall(fmt.Sprintf("dispatch-%d", count), "team", raw), result)
			}
			if result.IsError {
				current, e := r.Store.Status(nil)
				if e != nil {
					return "", count, e
				}
				recoverable := count < 8
				for _, id := range ids {
					if paused := current.Runs[id]; paused != nil && paused.Status == "paused" && paused.StopReason == "quota" {
						return strings.Join(lines, "\n"), count, &llm.ServiceError{Code: "request_limit", Message: "Quota exhausted. Progress is saved; resume when allowance is available.", Window: paused.QuotaWindow, ResetsAt: paused.QuotaReset, CanRetry: false}
					}
				}
				for _, id := range ids {
					run := current.Runs[id]
					if run != nil && run.Status == "passed" {
						lines = append(lines, "Verified and integrated "+id+".")
						continue
					}
					if run == nil || run.Status != "failed" || run.StopReason != "step_limit" || run.Attempts >= 3 {
						recoverable = false
					}
				}
				if recoverable {
					lines = append(lines, "Coordinator is diagnosing the budget stop before continuing preserved work.")
					continue
				}
				stops := []string{}
				for _, id := range ids {
					if run := current.Runs[id]; run != nil && run.StopReason == "step_limit" {
						stops = append(stops, fmt.Sprintf("%s reached its %d-call attempt limit after %d attempt(s). Preserved branch: %s.", id, run.StepLimit, run.Attempts, run.Branch))
					}
				}
				if len(stops) > 0 {
					return strings.Join(lines, "\n"), count, fmt.Errorf("automatic recovery stopped at its approved budget. %s Review saved worker reports and diagnoses before retrying; dependent tasks were not run", strings.Join(stops, " "))
				}
				return strings.Join(lines, "\n"), count, &ExecutionReview{Summary: "A worker wave needs coordinator review. " + truncate(resultText(result), 12000)}
			}
			for _, id := range ids {
				lines = append(lines, "Verified and integrated "+id+".")
			}
			continue
		}
		id := status.Ready[0]
		raw, _ := json.Marshal(map[string]string{"action": "run", "id": id})
		result, err := r.Execute(ctx, raw)
		if r.OnTool != nil {
			r.OnTool(session.ToolCall(fmt.Sprintf("dispatch-%d", count), "team", raw), result)
		}
		count++
		if err != nil {
			return "", count, err
		}
		if result.IsError {
			return "", count, fmt.Errorf("task %s failed; dependent tasks were not run.\n%s", id, resultText(result))
		}
		lines = append(lines, "Verified "+id+" with all approved checks.")
	}
	status, err := r.Store.Status(nil)
	if err != nil {
		return "", count, err
	}
	allPassed, hasTasks, hasRelease := true, false, false
	verification := []any{}
	var setup any = "See approved specification deployment instructions."
	for _, a := range status.Artifacts {
		if a.Kind == "release" {
			hasRelease = true
		}
		if a.Kind == "spec" {
			if content, e := r.Store.Content(a); e == nil {
				if value := content["deployment"]; value != nil {
					setup = value
				}
			}
		}
		if a.Kind == "task" {
			hasTasks = true
			run := status.Runs[a.ID]
			if run == nil || run.Status != "passed" {
				allPassed = false
				continue
			}
			checks := []any{}
			for _, check := range run.Checks {
				checks = append(checks, map[string]any{"command": check.Command, "passed": check.Passed})
			}
			verification = append(verification, map[string]any{"task": a.ID, "attempts": run.Attempts, "checks": checks})
		}
	}
	if hasTasks && allPassed {
		if !hasRelease {
			_, err = r.Store.Save("release", "verified-delivery", map[string]any{"summary": "All approved implementation and independent QA tasks passed their runner checks.", "setup": setup, "verification": verification, "limitations": []any{"No deployment or publication performed.", "Browser, visual, accessibility and external integration testing are only established by separately recorded evidence; passing configured checks alone does not prove them."}})
			if err != nil {
				return "", count, err
			}
		}
		lines = append(lines, "All tasks passed. Review the release artifact and inspect the application before accepting delivery.")
	} else if len(status.Ready) > 0 {
		lines = append(lines, "Batch limit reached. Continue approved tasks to run the next batch.")
	} else {
		return "", count, fmt.Errorf("no ready tasks; inspect approvals, failed attempts and dependencies")
	}
	return strings.Join(lines, "\n"), count, nil
}
