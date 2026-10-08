package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"censi/harness/internal/session"
)

// Shell budgets. A command that produces a gigabyte must not become a gigabyte
// of history, and a command that never returns must not pin a session forever.
const (
	defaultShellTimeout = 60 * time.Second
	maxShellTimeout     = 10 * time.Minute
	defaultShellBytes   = 32 * 1024
	shellTailBytes      = 4 * 1024
)

// BashTool runs a shell command in the workspace.
//
// KV Cache effect: a result appends to history like any other. Its size is
// bounded deterministically, so the same command produces the same bytes and
// cannot perturb the cached prefix of a later step.
type BashTool struct {
	ws      *Workspace
	shell   string
	timeout time.Duration
}

// NewBashTool returns a shell tool bound to a workspace.
func NewBashTool(ws *Workspace) *BashTool {
	return &BashTool{ws: ws, shell: "/bin/sh", timeout: defaultShellTimeout}
}

// Definition implements Tool.
func (t *BashTool) Definition() Definition {
	return Definition{
		Name: "bash",
		Description: "Run a shell command in the workspace root. Returns the combined output and " +
			"the exit code. Long output is truncated from the middle; a failing command is " +
			"reported as a result, not as an error.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"command": {"type": "string", "description": "The command to run."},
				"timeout_ms": {"type": "integer", "description": "Optional timeout in milliseconds."}
			},
			"required": ["command"]
		}`),
	}
}

type bashArgs struct {
	Command   string `json:"command"`
	TimeoutMS int    `json:"timeout_ms"`
}

// Execute implements Tool.
func (t *BashTool) Execute(ctx context.Context, raw json.RawMessage) (Result, error) {
	var args bashArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return Error("bash: the arguments were not valid JSON: " + err.Error()), nil
	}

	if strings.TrimSpace(args.Command) == "" {
		return Error("bash: a command is required."), nil
	}

	timeout := t.timeout
	if args.TimeoutMS > 0 {
		timeout = time.Duration(args.TimeoutMS) * time.Millisecond
	}
	if timeout > maxShellTimeout {
		timeout = maxShellTimeout
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, t.shell, "-c", args.Command)
	cmd.Dir = t.ws.Root()

	var combined bytes.Buffer
	cmd.Stdout = &combined
	cmd.Stderr = &combined

	// The environment is inherited deliberately: a coding agent that cannot see
	// PATH or a language toolchain is not useful. A deployment that needs a
	// tighter environment should set one on the process, not here.
	runErr := cmd.Run()

	output := combined.String()
	body, omitted := BoundText(output, defaultShellBytes, shellTailBytes)

	exitCode := 0
	timedOut := false

	switch {
	case runErr == nil:
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		timedOut = true
		exitCode = -1
	default:
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			return Error("bash: could not run the command: " + runErr.Error()), nil
		}
	}

	var status string
	switch {
	case timedOut:
		status = fmt.Sprintf("Command timed out after %s.", timeout)
	case exitCode == 0:
		status = "Command exited 0."
	default:
		status = fmt.Sprintf("Command exited %d.", exitCode)
	}

	if strings.TrimSpace(body) == "" {
		body = "(no output)"
	}

	return Result{
		Content: []session.Block{session.Text(joinNotice(body, omitted) + "\n\n" + status)},
		Omitted: omitted,
		IsError: exitCode != 0,
	}, nil
}
