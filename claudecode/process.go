package claudecode

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/inference-sh/agentprotocol/internal/agentproc"
)

// DefaultCommand is the binary launched when Options.Command is empty: the
// user's installed claude, found on PATH.
const DefaultCommand = "claude"

// Options describes how to launch the CLI.
type Options struct {
	// Command is the claude binary. Empty means DefaultCommand.
	Command string

	// Dir is the working directory, which is also the project the session
	// belongs to: the CLI files sessions per directory, so a resume must use
	// the same one.
	Dir string

	// Env is the child environment. Nil inherits the parent's, which is how
	// the user's own login is used. Selecting another account is a matter of
	// pointing CLAUDE_CONFIG_DIR at a profile the user logged into; nothing
	// here reads or passes a credential.
	Env []string

	// Model is passed as --model when set.
	Model string

	// SessionID fixes the ID of a new session (--session-id). The CLI names
	// its session only when the first prompt arrives, so choosing the ID up
	// front is the only way to know it before then. Ignored when resuming.
	SessionID string

	// ResumeSessionID resumes a persisted session (--resume).
	ResumeSessionID string

	// PermissionMode is passed as --permission-mode when set. Empty leaves
	// the user's configured mode. bypassPermissions is refused.
	PermissionMode string

	// MCPConfig is passed as --mcp-config when set: a JSON object of the form
	// {"mcpServers": {...}}.
	MCPConfig string

	// ExtraArgs are appended after the protocol flags.
	ExtraArgs []string

	// Stderr receives the CLI's diagnostics. Nil discards them.
	Stderr io.Writer
}

// Args builds the argv (without the binary) the SDK would pass for these
// options.
func Args(o Options) []string {
	args := []string{
		"--output-format", "stream-json",
		"--verbose",
		"--input-format", "stream-json",
		"--include-partial-messages",
		"--permission-prompt-tool", "stdio",
	}
	if o.Model != "" {
		args = append(args, "--model", o.Model)
	}
	if o.PermissionMode != "" {
		args = append(args, "--permission-mode", o.PermissionMode)
	}
	if o.MCPConfig != "" {
		args = append(args, "--mcp-config", o.MCPConfig)
	}
	// The SDK writes both of these in the --flag=value form.
	if o.ResumeSessionID != "" {
		args = append(args, "--resume="+o.ResumeSessionID)
	} else if o.SessionID != "" {
		args = append(args, "--session-id="+o.SessionID)
	}
	return append(args, o.ExtraArgs...)
}

// Process is the CLI running as a child with a Client on its stdio.
type Process struct {
	*Client
	proc *agentproc.Process
}

// Spawn launches the CLI and starts reading. It does not send initialize;
// the caller does, so it can choose the system prompt and hooks.
func Spawn(o Options, h Handler) (*Process, error) {
	if o.PermissionMode == PermissionModeBypassPermissions {
		return nil, errors.New("claudecode: bypassPermissions is not offered")
	}
	for _, a := range o.ExtraArgs {
		if strings.Contains(a, "dangerously-skip-permissions") || strings.Contains(a, PermissionModeBypassPermissions) {
			return nil, fmt.Errorf("claudecode: refusing %q; approvals go to the person", a)
		}
	}
	command := o.Command
	if command == "" {
		command = DefaultCommand
	}
	proc, err := agentproc.Start(agentproc.Options{Command: command, Args: Args(o), Dir: o.Dir, Env: withEntrypoint(o.Env), Stderr: o.Stderr})
	if err != nil {
		return nil, fmt.Errorf("claudecode: %w", err)
	}
	p := &Process{Client: NewClient(proc.Stdout, proc.Stdin, h), proc: proc}
	p.Start()
	proc.Reap(p.Client.Done())
	return p, nil
}

// withEntrypoint marks the child as driven by an SDK host, as the TypeScript
// and Python SDKs do (sdk-ts, sdk-py). An entrypoint the caller already set
// is kept.
func withEntrypoint(env []string) []string {
	if env == nil {
		env = os.Environ()
	}
	for _, kv := range env {
		if strings.HasPrefix(kv, "CLAUDE_CODE_ENTRYPOINT=") {
			return env
		}
	}
	return append(append([]string(nil), env...), "CLAUDE_CODE_ENTRYPOINT=sdk-go")
}

// Exited closes when the child has been reaped and its output read.
func (p *Process) Exited() <-chan struct{} { return p.proc.Exited() }

// ExitErr is the child's exit status once Exited has closed.
func (p *Process) ExitErr() error { return p.proc.ExitErr() }

// DefaultShutdownGrace is how long Close lets the CLI finish after its input
// ends: it writes the session transcript and runs SessionEnd hooks then.
const DefaultShutdownGrace = 5 * time.Second

// Close ends input, which the CLI treats as the end of the session, and waits
// up to grace for it to exit before killing it. Zero grace means
// DefaultShutdownGrace.
func (p *Process) Close(grace time.Duration) error {
	if grace <= 0 {
		grace = DefaultShutdownGrace
	}
	return p.proc.Close(grace)
}

// Kill terminates the child and the processes it started at once.
func (p *Process) Kill() error { return p.proc.Kill() }

// Pid is the child's process id.
func (p *Process) Pid() int { return p.proc.Pid() }

// MCPConfigHTTP builds an --mcp-config value declaring one HTTP MCP server.
func MCPConfigHTTP(name, url string, headers map[string]string) string {
	server := map[string]any{"type": "http", "url": url}
	if len(headers) > 0 {
		server["headers"] = headers
	}
	data, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{name: server}})
	return string(data)
}
