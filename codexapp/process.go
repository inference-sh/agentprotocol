package codexapp

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"
)

// Process is `codex app-server` running as a child, with a Client on its
// stdio that has completed the handshake.
type Process struct {
	*Client
	cmd *exec.Cmd

	// Init is what the server reported during initialize, including the
	// CODEX_HOME it resolved.
	Init InitializeResponse

	stdin     io.Closer
	waitOnce  sync.Once
	waitErr   error
	waited    chan struct{}
	closeOnce sync.Once
}

// ProcessConfig describes how to launch the server.
type ProcessConfig struct {
	// Command is the codex binary. Empty means "codex" on PATH.
	Command string

	// Args follow "app-server" on the command line, for example config
	// overrides as "-c", "key=value".
	Args []string

	// Dir is the child's working directory.
	Dir string

	// Env is the child environment. Nil inherits the parent's.
	//
	// This is where an account profile is selected, by pointing CODEX_HOME at
	// a directory the user logged into with codex itself. Nothing here reads
	// the credential; the codex binary does.
	Env []string

	// Stderr receives codex's logs. Nil discards them.
	Stderr io.Writer
}

// DefaultShutdownGrace is how long Close waits for the server to exit after
// its stdin closes before killing it.
const DefaultShutdownGrace = 3 * time.Second

// Spawn launches the server, starts reading, and completes initialize.
//
// On failure the child is killed before returning.
func Spawn(ctx context.Context, cfg ProcessConfig, info ClientInfo, h Handler) (*Process, error) {
	command := cfg.Command
	if command == "" {
		command = "codex"
	}
	cmd := exec.Command(command, append([]string{"app-server"}, cfg.Args...)...)
	cmd.Dir = cfg.Dir
	cmd.Env = cfg.Env
	// A command codex runs could inherit and hold the pipes; do not let that
	// keep Wait from returning once codex itself has exited.
	cmd.WaitDelay = time.Second
	if cfg.Stderr != nil {
		cmd.Stderr = cfg.Stderr
	} else {
		cmd.Stderr = io.Discard
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("codex app-server: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("codex app-server: stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("codex app-server: launch %s: %w", command, err)
	}

	p := &Process{
		Client: NewClient(stdout, stdin, h),
		cmd:    cmd,
		stdin:  stdin,
		waited: make(chan struct{}),
	}
	p.Start()
	go p.reap()

	init, err := p.Initialize(ctx, info)
	if err != nil {
		_ = p.Kill()
		return nil, fmt.Errorf("codex app-server: initialize: %w", err)
	}
	p.Init = init
	return p, nil
}

// reap waits for the child once the stream has ended, so the exit status is
// collected even if nobody calls Close.
func (p *Process) reap() {
	<-p.Done()
	p.wait()
}

func (p *Process) wait() error {
	p.waitOnce.Do(func() {
		p.waitErr = p.cmd.Wait()
		close(p.waited)
	})
	<-p.waited
	return p.waitErr
}

// Close ends the server: stdin closes, which codex treats as the end of the
// connection, and the child is killed if it has not exited within
// DefaultShutdownGrace. It is safe to call more than once.
func (p *Process) Close() error {
	p.closeOnce.Do(func() { _ = p.stdin.Close() })
	select {
	case <-p.Done():
	case <-time.After(DefaultShutdownGrace):
		_ = p.cmd.Process.Kill()
	}
	return p.wait()
}

// Kill stops the child at once.
func (p *Process) Kill() error {
	p.closeOnce.Do(func() { _ = p.stdin.Close() })
	_ = p.cmd.Process.Kill()
	return p.wait()
}

// Exited closes when the child has been reaped.
func (p *Process) Exited() <-chan struct{} { return p.waited }
