package codexapp

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/inference-sh/agentprotocol/internal/agentproc"
)

// Process is `codex app-server` running as a child, with a Client on its
// stdio that has completed the handshake.
type Process struct {
	*Client
	proc *agentproc.Process

	// Init is what the server reported during initialize, including the
	// CODEX_HOME it resolved.
	Init InitializeResponse
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
	proc, err := agentproc.Start(agentproc.Options{
		Command: command,
		Args:    append([]string{"app-server"}, cfg.Args...),
		Dir:     cfg.Dir,
		Env:     cfg.Env,
		Stderr:  cfg.Stderr,
	})
	if err != nil {
		return nil, fmt.Errorf("codex app-server: %w", err)
	}
	p := &Process{Client: NewClient(proc.Stdout, proc.Stdin, h), proc: proc}
	p.Start()
	proc.Reap(p.Done())

	init, err := p.Initialize(ctx, info)
	if err != nil {
		_ = p.Kill()
		return nil, fmt.Errorf("codex app-server: initialize: %w", err)
	}
	p.Init = init
	return p, nil
}

// Close ends the server: stdin closes, which codex treats as the end of the
// connection, and the child is killed if it has not exited within
// DefaultShutdownGrace. It is safe to call more than once.
func (p *Process) Close() error { return p.proc.Close(DefaultShutdownGrace) }

// Kill stops the child and the processes it started at once, and reaps it.
func (p *Process) Kill() error {
	p.proc.CloseStdin()
	_ = p.proc.Kill()
	return p.proc.ExitErr()
}

// Exited closes when the child has been reaped.
func (p *Process) Exited() <-chan struct{} { return p.proc.Exited() }
