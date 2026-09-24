package pirpc

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"
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
	cmd   *exec.Cmd
	stdin io.WriteCloser

	exited  chan struct{}
	waitErr error
}

// Spawn launches pi in RPC mode and starts reading. pi sends nothing until
// it is asked, so a caller that needs to know pi is up sends get_state.
func Spawn(o Options, h Handler) (*Process, error) {
	command := o.Command
	if command == "" {
		command = DefaultCommand
	}
	cmd := exec.Command(command, Args(o)...)
	cmd.Dir = o.Dir
	cmd.Env = o.Env
	cmd.Stderr = o.Stderr
	if cmd.Stderr == nil {
		cmd.Stderr = io.Discard
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("pirpc: stdin pipe: %w", err)
	}
	// An os.Pipe rather than StdoutPipe: pi's tools start processes of their
	// own, and one that outlives pi can hold the write end open. Reaping must
	// not wait for that EOF, and reading must not be cut short by Wait.
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("pirpc: stdout pipe: %w", err)
	}
	cmd.Stdout = stdoutW
	if err := cmd.Start(); err != nil {
		stdoutR.Close()
		stdoutW.Close()
		return nil, fmt.Errorf("pirpc: launch %s: %w", command, err)
	}
	stdoutW.Close()

	p := &Process{
		Client: NewClient(stdoutR, stdin, h),
		cmd:    cmd,
		stdin:  stdin,
		exited: make(chan struct{}),
	}
	p.Start()
	go func() {
		err := cmd.Wait()
		// Let the read loop drain what pi wrote before it died; stop waiting
		// for a descendant that kept the pipe.
		select {
		case <-p.Client.Done():
		case <-time.After(2 * time.Second):
			stdoutR.Close()
			<-p.Client.Done()
		}
		p.waitErr = err
		close(p.exited)
	}()
	return p, nil
}

// Exited closes when pi has been reaped and its output read.
func (p *Process) Exited() <-chan struct{} { return p.exited }

// ExitErr is pi's exit status once Exited has closed.
func (p *Process) ExitErr() error {
	<-p.exited
	return p.waitErr
}

// DefaultShutdownGrace is how long Close lets pi dispose of its session
// after input ends.
const DefaultShutdownGrace = 5 * time.Second

// Close ends input, which pi treats as a request to shut down (rpc.md,
// Shutdown), and waits up to grace for it to exit before killing it.
func (p *Process) Close(grace time.Duration) error {
	if grace <= 0 {
		grace = DefaultShutdownGrace
	}
	_ = p.stdin.Close()
	t := time.NewTimer(grace)
	defer t.Stop()
	select {
	case <-p.exited:
	case <-t.C:
		_ = p.Kill()
		<-p.exited
	}
	return p.waitErr
}

// Kill terminates pi at once (SIGKILL).
func (p *Process) Kill() error {
	if p.cmd.Process == nil {
		return nil
	}
	return p.cmd.Process.Kill()
}

// Pid is the child's process id.
func (p *Process) Pid() int {
	if p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}
