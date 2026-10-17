package greenfield

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type ExecutionWorkspace struct {
	VerifiedFiles map[string]string `json:"verified_files,omitempty"`
	Path          string            `json:"path"`
	Branch        string            `json:"branch"`
	BaseCommit    string            `json:"base_commit"`
	BaseFiles     map[string]string `json:"base_files"`
	Commit        string            `json:"commit,omitempty"`
	AppliedCommit string            `json:"applied_commit,omitempty"`
}

var omittedSource = map[string]bool{".git": true, ".maxlabs": true, "node_modules": true, "vendor": true, ".venv": true, "venv": true, "__pycache__": true, ".next": true, ".nuxt": true, ".cache": true, ".pytest_cache": true, ".DS_Store": true, ".aws": true, ".ssh": true, ".npmrc": true, ".netrc": true, ".pypirc": true, "auth.json": true}

func sourceFiles(root string) (map[string]string, error) {
	files := map[string]string{}
	var bytes int64
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if omittedSource[entry.Name()] || !SafeTaskPath(rel) || entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("unsupported source file %s", rel)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		bytes += info.Size()
		if len(files) >= 5000 || bytes > 64*1024*1024 {
			return fmt.Errorf("source snapshot exceeds 5000 files or 64 MiB; narrow the project folder")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[rel] = hash(data)
		return nil
	})
	return files, err
}
func workerEnvironment() []string {
	result := []string{}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		upper := strings.ToUpper(key)
		if strings.Contains(upper, "TOKEN") || strings.Contains(upper, "SECRET") || strings.Contains(upper, "PASSWORD") || strings.Contains(upper, "API_KEY") || strings.Contains(upper, "ACCESS_KEY") || strings.Contains(upper, "PRIVATE_KEY") || strings.HasPrefix(upper, "HARNESS_") || strings.HasPrefix(upper, "MAXLABS_") || strings.HasPrefix(upper, "OPENAI_") || strings.HasPrefix(upper, "ANTHROPIC_") || upper == "SSH_AUTH_SOCK" {
			continue
		}
		result = append(result, entry)
	}
	return result
}
func gitCommand(ctx context.Context, root string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", append([]string{"-C", root, "-c", "user.name=MaxLabs Team", "-c", "user.email=team@localhost", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=" + os.DevNull}, args...)...)
	command.Env = append(workerEnvironment(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), truncate(string(output), 1500))
	}
	return strings.TrimSpace(string(output)), nil
}
func (s *Store) prepareExecution(ctx context.Context, state *State) (string, error) {
	if state.Execution != nil && state.Execution.AppliedCommit != "" {
		state.Execution = nil
	}
	if state.Execution != nil {
		root, err := s.Path(state.Execution.Path)
		if err != nil {
			return "", err
		}
		if _, err = gitCommand(ctx, root, "rev-parse", "--is-inside-work-tree"); err != nil {
			return "", fmt.Errorf("build workspace unavailable: %w", err)
		}
		return root, nil
	}
	files, err := sourceFiles(s.Root)
	if err != nil {
		return "", err
	}
	id := fmt.Sprintf("%d", time.Now().UnixNano())
	snapshot, err := s.Path("execution/" + id + "/snapshot")
	if err != nil {
		return "", err
	}
	if err = os.MkdirAll(snapshot, 0700); err != nil {
		return "", err
	}
	for path := range files {
		from, err := SafePath(s.Root, path)
		if err != nil {
			return "", err
		}
		to := filepath.Join(snapshot, filepath.FromSlash(path))
		if err = os.MkdirAll(filepath.Dir(to), 0755); err != nil {
			return "", err
		}
		data, err := os.ReadFile(from)
		if err != nil {
			return "", err
		}
		info, _ := os.Stat(from)
		if err = os.WriteFile(to, data, info.Mode().Perm()); err != nil {
			return "", err
		}
	}
	if _, err = gitCommand(ctx, snapshot, "init", "-b", "baseline"); err != nil {
		return "", err
	}
	// Source filtering above excludes credentials and dependency trees before force-add.
	if _, err = gitCommand(ctx, snapshot, "add", "--all", "--force"); err != nil {
		return "", err
	}
	if _, err = gitCommand(ctx, snapshot, "commit", "--allow-empty", "-m", "Project baseline before team execution"); err != nil {
		return "", err
	}
	base, err := gitCommand(ctx, snapshot, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	relative := "execution/" + id + "/worktree"
	root, err := s.Path(relative)
	if err != nil {
		return "", err
	}
	branch := fmt.Sprintf("team-%d", time.Now().UnixNano())
	if _, err = gitCommand(ctx, snapshot, "worktree", "add", "-b", branch, root, base); err != nil {
		return "", err
	}
	state.Execution = &ExecutionWorkspace{Path: relative, Branch: branch, BaseCommit: base, BaseFiles: files, Commit: base}
	if err = s.Persist(state); err != nil {
		return "", err
	}
	return root, nil
}
func checkpoint(ctx context.Context, root string, execution *ExecutionWorkspace, message string) (string, error) {
	files, err := sourceFiles(root)
	if err != nil {
		return "", err
	}
	if _, err = gitCommand(ctx, root, "add", "--update"); err != nil {
		return "", err
	}
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for start := 0; start < len(paths); start += 100 {
		end := start + 100
		if end > len(paths) {
			end = len(paths)
		}
		if _, err = gitCommand(ctx, root, append([]string{"add", "--force", "--"}, paths[start:end]...)...); err != nil {
			return "", err
		}
	}
	if _, err = gitCommand(ctx, root, "commit", "--allow-empty", "-m", message); err != nil {
		return "", err
	}
	commit, err := gitCommand(ctx, root, "rev-parse", "HEAD")
	if err == nil {
		execution.Commit = commit
	}
	return commit, err
}
func (s *Store) ApplyExecution(state *State) error {
	if state.Execution == nil {
		return nil
	}
	root, err := s.Path(state.Execution.Path)
	if err != nil {
		return err
	}
	files, err := sourceFiles(root)
	if err != nil {
		return err
	}
	if state.Execution.VerifiedFiles == nil || !sameFiles(files, state.Execution.VerifiedFiles) {
		return fmt.Errorf("build workspace changed after QA; run independent verification again before accepting")
	}
	if state.Execution.AppliedCommit == state.Execution.Commit {
		return nil
	}
	original, err := sourceFiles(s.Root)
	if err != nil {
		return err
	}
	if !sameFiles(original, state.Execution.BaseFiles) {
		return fmt.Errorf("original project changed since the build snapshot; reconcile these edits before accepting this release")
	}
	changed := map[string]bool{}
	for path, digest := range files {
		if state.Execution.BaseFiles[path] != digest {
			changed[path] = true
		}
	}
	for path := range state.Execution.BaseFiles {
		if _, ok := files[path]; !ok {
			changed[path] = true
		}
	}
	// Validate the whole change set before writing, so user edits never get silently replaced.
	for path := range changed {
		target, err := SafeWorkerPath(s.Root, path)
		if err != nil {
			return err
		}
		raw, err := os.ReadFile(target)
		current := "missing"
		if err == nil {
			current = hash(raw)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		expected := state.Execution.BaseFiles[path]
		if expected == "" {
			expected = "missing"
		}
		if current != expected {
			return fmt.Errorf("original project changed at %s; review and reconcile it before accepting this release", path)
		}
	}
	for path := range changed {
		target, _ := SafeWorkerPath(s.Root, path)
		if _, ok := files[path]; !ok {
			if err = os.Remove(target); err != nil {
				return err
			}
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			return err
		}
		if err = os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return err
		}
		temporary := target + ".team-apply-" + fmt.Sprint(time.Now().UnixNano())
		if err = os.WriteFile(temporary, data, sourceMode(root, path)); err != nil {
			return err
		}
		if err = os.Rename(temporary, target); err != nil {
			_ = os.Remove(temporary)
			return err
		}
	}
	state.Execution.AppliedCommit = state.Execution.Commit
	return nil
}
func (s *Store) ExecutionInfo(state State) map[string]any {
	if state.Execution == nil {
		return nil
	}
	path, _ := s.Path(state.Execution.Path)
	raw, _ := json.Marshal(state.Execution)
	var info map[string]any
	_ = json.Unmarshal(raw, &info)
	delete(info, "base_files")
	delete(info, "verified_files")
	info["root"] = path
	return info
}

func sameFiles(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func sourceMode(root, path string) os.FileMode {
	info, err := os.Stat(filepath.Join(root, filepath.FromSlash(path)))
	if err != nil {
		return 0644
	}
	return info.Mode().Perm()
}
