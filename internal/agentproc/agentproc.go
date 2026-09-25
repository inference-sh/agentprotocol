// Package agentproc runs an agent CLI as a child process for the protocol
// packages: one way to launch it, read its stdout, stop it and reap it,
// including when a process the agent started outlives it.
package agentproc

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"
)

// DrainTimeout is how long reaping waits, once the child has exited, for the
// reader to finish with stdout before closing it. A process the agent's tools
// started can inherit the pipe and hold it open after the agent is gone;
// without a bound, reaping would wait for that process instead.
const DrainTimeout = 2 * time.Second

// Options describes the child.
type Options struct {
	Command string
	Args    []string
	Dir     string

	// Env is the child environment. Nil inherits the parent's.
	Env []string

	// Stderr receives the child's diagnostics. Nil discards them.
	Stderr io.Writer
}

// Process is a started child. The caller reads Stdout and writes Stdin, and
// calls Reap once its reader is running.
type Process struct {
	Stdin  io.WriteCloser
	Stdout io.ReadCloser

	cmd       *exec.Cmd
	stdinOnce sync.Once
	exited    chan struct{}
	waitErr   error
}

// Start launches the child in a process group of its own, so Kill reaches
// the processes it started as well.
//
// Stdout is an os.Pipe rather than StdoutPipe: cmd.Wait closes a StdoutPipe,
// which cuts off the last lines, while waiting for EOF first hangs on a
// grandchild that kept the write end. Reap bounds that wait instead.
func Start(o Options) (*Process, error) {
	cmd := exec.Command(o.Command, o.Args...)
	cmd.Dir = o.Dir
	cmd.Env = o.Env
	// Nil goes to the null device. Any other writer that is not a file is
	// fed by a copier that a grandchild holding stderr would keep Wait on.
	cmd.Stderr = o.Stderr
	cmd.WaitDelay = DrainTimeout
	setGroup(cmd)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stdout = stdoutW
	if err := cmd.Start(); err != nil {
		stdoutR.Close()
		stdoutW.Close()
		return nil, fmt.Errorf("launch %s: %w", o.Command, err)
	}
	stdoutW.Close()
	return &Process{Stdin: stdin, Stdout: stdoutR, cmd: cmd, exited: make(chan struct{})}, nil
}

// Reap collects the child's exit in the background. readDone closes when the
// caller's reader has finished with Stdout. Exited closes once the child has
// been reaped and the reader has finished, or DrainTimeout after the child
// exited, when Stdout is closed to end a reader a grandchild is holding.
func (p *Process) Reap(readDone <-chan struct{}) {
	go func() {
		err := p.cmd.Wait()
		select {
		case <-readDone:
		case <-time.After(DrainTimeout):
			p.Stdout.Close()
			<-readDone
		}
		p.waitErr = err
		close(p.exited)
	}()
}

// Exited closes when the child has been reaped and its output read.
func (p *Process) Exited() <-chan struct{} { return p.exited }

// ExitErr waits for Exited and returns the child's exit status.
func (p *Process) ExitErr() error {
	<-p.exited
	return p.waitErr
}

// State is how the child ended, once Exited has closed; nil before that.
func (p *Process) State() *os.ProcessState {
	select {
	case <-p.exited:
		return p.cmd.ProcessState
	default:
		return nil
	}
}

// CloseStdin ends the child's input. It is safe to call more than once.
func (p *Process) CloseStdin() {
	p.stdinOnce.Do(func() { _ = p.Stdin.Close() })
}

// Close ends input, which agents take as the request to shut down, and waits
// up to grace for the child to exit before killing it. It returns the exit
// status.
func (p *Process) Close(grace time.Duration) error {
	p.CloseStdin()
	t := time.NewTimer(grace)
	defer t.Stop()
	select {
	case <-p.exited:
	case <-t.C:
		_ = p.Kill()
	}
	return p.ExitErr()
}

// Kill sends SIGKILL to the child and every process in its group. It does
// not wait; Exited closes once the child is reaped. After Exited it does
// nothing: the group may be empty by then and its id free for reuse.
func (p *Process) Kill() error {
	select {
	case <-p.exited:
		return nil
	default:
	}
	return kill(p.cmd.Process)
}

// Pid is the child's process id.
func (p *Process) Pid() int {
	if p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}
