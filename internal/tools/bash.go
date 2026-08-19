package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"time"
)

// DefaultBashTimeout bounds how long a single command may run before it is
// killed and reported as an error, so a hung command cannot block the loop.
const DefaultBashTimeout = 30 * time.Second

// bashMaxOutputBytes caps combined output so a chatty command cannot flood the
// model's context. Anything beyond is dropped with a note.
const bashMaxOutputBytes = 64 * 1024

// capOutput returns out as text, truncated to bashMaxOutputBytes with a note.
func capOutput(out []byte) string {
	if len(out) <= bashMaxOutputBytes {
		return string(out)
	}
	return string(out[:bashMaxOutputBytes]) + fmt.Sprintf("\n[bash: output truncated at %d KB]", bashMaxOutputBytes/1024)
}

type bashTool struct {
	timeout time.Duration
}

// NewBashTool returns the bash tool. A non-positive timeout falls back to
// DefaultBashTimeout. The timeout is configurable so tests can exercise the
// timeout path quickly.
func NewBashTool(timeout time.Duration) Tool {
	if timeout <= 0 {
		timeout = DefaultBashTimeout
	}
	return bashTool{timeout: timeout}
}

func (bashTool) Name() string { return "bash" }

func (bashTool) SideEffecting() bool { return true }

func (b bashTool) Description() string {
	return fmt.Sprintf("Run a command, return its combined stdout/stderr and exit status. Killed after %s.", b.timeout)
}

func (bashTool) Schema() map[string]any {
	return objectSchema(map[string]any{
		"command": strProp("Command line."),
	}, "command")
}

func (b bashTool) Execute(ctx context.Context, input json.RawMessage) (string, bool) {
	var args struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return fmt.Sprintf("invalid arguments: %v", err), true
	}
	if args.Command == "" {
		return "command is required", true
	}

	runCtx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()

	// cmd := exec.CommandContext(runCtx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", args.Command)
	cmd := exec.CommandContext(runCtx, "bash", "-c", args.Command)
	out, err := cmd.CombinedOutput()

	if runCtx.Err() == context.DeadlineExceeded {
		return fmt.Sprintf("command timed out after %s\n%s", b.timeout, capOutput(out)), true
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return fmt.Sprintf("%s\n(exit code %d)", capOutput(out), ee.ExitCode()), true
		}
		return fmt.Sprintf("failed to run command: %v\n%s", err, capOutput(out)), true
	}
	return capOutput(out), false
}
