package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Workspace confines filesystem tools to a single directory.
//
// This is a correctness guard, not a security boundary. It stops a model from
// wandering out of the project by accident - which is by far the common case -
// but it does not defend against a determined escape: a symlink inside the
// workspace pointing outside it is only resolved when the target already
// exists. A real deployment layers an OS sandbox underneath, and this type is
// written on the assumption that it does.
type Workspace struct {
	root string
}

// NewWorkspace resolves root and returns a workspace anchored to it.
func NewWorkspace(root string) (*Workspace, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("tools: workspace root is required")
	}

	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("tools: resolving workspace root: %w", err)
	}

	// Resolve symlinks on the root itself, so the containment check compares
	// like with like.
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}

	return &Workspace{root: abs}, nil
}

// Root returns the workspace's absolute root.
func (w *Workspace) Root() string { return w.root }

// Resolve maps a model-supplied path to an absolute path inside the workspace.
//
// An empty path is the workspace root itself. Relative paths resolve against the
// root; absolute paths are accepted only when they already fall inside it.
func (w *Workspace) Resolve(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return w.root, nil
	}

	candidate := path
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(w.root, candidate)
	}

	candidate = filepath.Clean(candidate)

	// Resolve symlinks where the path exists, so a link cannot smuggle the
	// check past a path that merely looks contained.
	if resolved, err := filepath.EvalSymlinks(candidate); err == nil {
		candidate = resolved
	}

	rel, err := filepath.Rel(w.root, candidate)
	if err != nil {
		return "", fmt.Errorf("tools: %q is outside the workspace", path)
	}

	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("tools: %q is outside the workspace", path)
	}

	return candidate, nil
}

// Display renders an absolute path relative to the workspace, so that what the
// model sees does not depend on where the workspace happens to live.
//
// This matters for caching as much as for readability: an absolute path baked
// into a tool result is a byte that differs per machine, and a tool result is
// part of the history the provider caches.
func (w *Workspace) Display(path string) string {
	rel, err := filepath.Rel(w.root, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(path)
	}

	return filepath.ToSlash(rel)
}

// writeFile writes content, creating parent directories as needed.
func (w *Workspace) writeFile(path string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	return os.WriteFile(path, content, 0o644)
}
