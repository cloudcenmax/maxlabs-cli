package greenfield

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"censi/harness/internal/tools"
)

func qaTestPath(path string) bool {
	if !SafeTaskPath(path) {
		return false
	}
	lower := strings.ToLower(filepath.ToSlash(path))
	ext := strings.ToLower(filepath.Ext(path))
	allowed := map[string]bool{".php": true, ".js": true, ".mjs": true, ".cjs": true, ".ts": true, ".tsx": true, ".jsx": true, ".py": true, ".go": true, ".rb": true, ".rs": true, ".java": true, ".kt": true, ".cs": true}
	if !allowed[ext] {
		return false
	}
	parts := strings.Split(lower, "/")
	for _, part := range parts[:len(parts)-1] {
		if part == "tests" || part == "test" || part == "__tests__" {
			return true
		}
	}
	name := filepath.Base(lower)
	return strings.Contains(name, ".test.") || strings.Contains(name, ".spec.") || strings.HasPrefix(name, "test_") || strings.HasSuffix(name, "_test.go") || strings.HasSuffix(name, "_test.py")
}
func qaSourceChanges(before, after map[string]string) (bool, []string) {
	added := []string{}
	for path, value := range before {
		if after[path] != value {
			return false, nil
		}
	}
	for path := range after {
		if _, exists := before[path]; !exists {
			if !qaTestPath(path) {
				return false, nil
			}
			added = append(added, path)
		}
	}
	sort.Strings(added)
	return true, added
}
func (r *Runtime) repairFromQA(ctx context.Context, id, qaID string) (tools.Result, error) {
	err := r.Store.Locked(func(state *State) error {
		status, err := r.Store.Status(state)
		if err != nil {
			return err
		}
		if !flexible(state) || len(status.Errors) > 0 || state.Approvals["spec"] != status.Gates["spec"] || state.Execution == nil {
			return fmt.Errorf("repair requires an unchanged approved flexible plan")
		}
		if id == qaID || r.workersUsed >= 8 {
			return fmt.Errorf("invalid repair target or worker budget exhausted")
		}
		var target, qa map[string]any
		for _, a := range state.Artifacts {
			if a.Kind != "task" {
				continue
			}
			if a.ID == id {
				target, err = r.Store.Content(a)
			}
			if a.ID == qaID {
				qa, err = r.Store.Content(a)
			}
			if err != nil {
				return err
			}
		}
		findings := state.Runs[qaID]
		prior := state.Runs[id]
		if target == nil || qa == nil || text(target, "role") == "qa" || text(qa, "role") != "qa" || prior == nil || prior.Status == "running" || findings == nil || findings.Status != "failed" || findings.StopReason != "qa_findings" || findings.Commit == "" {
			return fmt.Errorf("repair requires recorded QA findings and an existing approved implementation task")
		}
		if findings.QARepairRounds >= 3 || (prior.Status != "passed" && prior.Attempts >= 3) {
			return fmt.Errorf("QA repair budget reached; review findings before another attempt")
		}
		failed := false
		for _, c := range findings.Checks {
			if !c.Passed {
				failed = true
			}
		}
		if !failed {
			return fmt.Errorf("QA repair requires an actual failed recorded check")
		}
		root, err := r.Store.Path(findings.Workspace)
		if err != nil {
			return err
		}
		current, err := sourceHash(root)
		if err != nil {
			return err
		}
		if current != findings.ReviewedSourceHash {
			return fmt.Errorf("QA source changed since findings were recorded")
		}
		integration, err := r.Store.Path(state.Execution.Path)
		if err != nil {
			return err
		}
		base, err := recoverySourceFiles(integration)
		if err != nil {
			return err
		}
		reviewed, err := recoverySourceFiles(root)
		if err != nil {
			return err
		}
		if valid, _ := qaSourceChanges(base, reviewed); !valid {
			return fmt.Errorf("QA branch contains application changes; repair cannot reuse it")
		}
		copy := *prior
		if prior.Status == "passed" {
			copy.Attempts = 0
		}
		copy.Status = "failed"
		copy.StopReason = "qa_repair"
		copy.RepairFrom = qaID
		copy.Commit = findings.Commit
		copy.Workspace = findings.Workspace
		copy.Branch = findings.Branch
		copy.Merged = false
		copy.Checks = append([]Check{}, findings.Checks...)
		copy.Output = "Repair only defects exposed by QA within this task's existing approved outcomes. Preserve and pass the QA regression tests; do not weaken existing tests or change requirements. QA report: " + truncate(findings.Output, 4000)
		state.Runs[id] = &copy
		findings.QARepairRounds++
		delete(state.Approvals, "release")
		state.Execution.VerifiedFiles = nil
		return r.Store.Persist(state)
	})
	if err != nil {
		return tools.Result{}, err
	}
	return r.runFlexible(ctx, id)
}

// Existing projects may have added QA tests in a preserved older worktree.
func qaWritable(root, path string, before map[string]string) bool {
	if filepath.IsAbs(path) {
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return false
		}
		path = relative
	}
	if before == nil || !qaTestPath(path) {
		return false
	}
	if _, exists := before[filepath.ToSlash(filepath.Clean(path))]; exists {
		return false
	}
	_, err := SafeWorkerPath(root, path)
	return err == nil
}
