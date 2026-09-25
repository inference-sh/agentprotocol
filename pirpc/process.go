package pirpc

import (
	"fmt"
	"io"
	"time"

	"github.com/inference-sh/agentprotocol/internal/agentproc"
)

// DefaultCommand is the binary launched when Options.Command is empty: the
// user's installed pi, found on PATH.
const DefaultCommand = "pi"

// Options describes how to launch pi.
type Options struct {
	// Command is the pi binary. Empty means DefaultCommand.
	Command string

	// Dir is the working directory, which is also the project the session
	// is filed under: pi keeps sessions per directory, so a resume must use
	// the same one.
	Dir string

	// Env is the child environment. Nil inherits the parent's, which is how
	// the user's own login and provider keys are used. PI_CODING_AGENT_DIR
	// selects another profile.
	Env []string

	// Model is passed as --model when set: an exact id, a fuzzy match, or
	// provider/id, as pi's own flag takes it.
	Model string

	// SessionID reopens the project session with this exact id
	// (--session-id). pi creates the session when there is none, so a caller
	// that means "resume" checks that the session file exists afterwards.
	SessionID string

	// AppendSystemPrompt is passed as --append-system-prompt when set. pi
	// reads it as a file when it names one that exists.
	AppendSystemPrompt string

	// ExtraArgs are appended after the protocol flags.
	ExtraArgs []string

	// Stderr receives pi's diagnostics. Nil discards them.
	Stderr io.Writer
}

// Args builds the argv (without the binary) for these options.
func Args(o Options) []string {
	args := []string{"--mode", "rpc"}
	if o.Model != "" {
		args = append(args, "--model", o.Model)
	}
	if o.SessionID != "" {
		args = append(args, "--session-id", o.SessionID)
	}
	if o.AppendSystemPrompt != "" {
		args = append(args, "--append-system-prompt", o.AppendSystemPrompt)
	}
	return append(args, o.ExtraArgs...)
}

// Process is pi running as a child with a Client on its stdio.
type Process struct {
	*Client
	proc *agentproc.Process
}

// Spawn launches pi in RPC mode and starts reading. pi sends nothing until
// it is asked, so a caller that needs to know pi is up sends get_state.
func Spawn(o Options, h Handler) (*Process, error) {
	command := o.Command
	if command == "" {
		command = DefaultCommand
	}
	proc, err := agentproc.Start(agentproc.Options{Command: command, Args: Args(o), Dir: o.Dir, Env: o.Env, Stderr: o.Stderr})
	if err != nil {
		return nil, fmt.Errorf("pirpc: %w", err)
	}
	p := &Process{Client: NewClient(proc.Stdout, proc.Stdin, h), proc: proc}
	p.Start()
	proc.Reap(p.Client.Done())
	return p, nil
}

// Exited closes when pi has been reaped and its output read.
func (p *Process) Exited() <-chan struct{} { return p.proc.Exited() }

// ExitErr is pi's exit status once Exited has closed.
func (p *Process) ExitErr() error { return p.proc.ExitErr() }

// DefaultShutdownGrace is how long Close lets pi dispose of its session
// after input ends.
const DefaultShutdownGrace = 5 * time.Second

// Close ends input, which pi treats as a request to shut down (rpc.md,
// Shutdown), and waits up to grace for it to exit before killing it.
func (p *Process) Close(grace time.Duration) error {
	if grace <= 0 {
		grace = DefaultShutdownGrace
	}
	return p.proc.Close(grace)
}

// Kill terminates pi and the processes it started at once (SIGKILL).
func (p *Process) Kill() error { return p.proc.Kill() }

// Pid is the child's process id.
func (p *Process) Pid() int { return p.proc.Pid() }
