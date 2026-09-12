package acp

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
)

// Process is an ACP agent running as a local subprocess, with a Client already
// wired to its stdio.
//
// Most ACP agents are launched this way, so this saves every caller the same
// pipe plumbing. Callers that already have a stream, over a socket or in a
// test, should use NewClient directly instead.
type Process struct {
	*Client
	cmd *exec.Cmd
}

// ProcessConfig describes how to launch the agent.
type ProcessConfig struct {
	// Command and Args launch the agent in ACP mode, for example
	// "claude-code-acp", or "grok" with "agent", "stdio".
	Command string
	Args    []string

	// Dir is the working directory, which also becomes the session root
	// unless the caller passes a different cwd to NewSession.
	Dir string

	// Env is the child environment. Nil inherits the parent's.
	//
	// This is where a caller selects which account the agent uses, by
	// pointing the agent's own config-directory variable at a profile the
	// user logged into normally. The credential is written and read by the
	// vendor's binary; nothing here reads or transmits it.
	Env []string

	// Stderr receives the agent's diagnostics. Nil discards them. Agents are
	// chatty here and it is usually the only clue when a launch fails, so
	// capturing it is worth the buffer.
	Stderr io.Writer
}

// Spawn launches the agent, starts the client's read loop, and completes the
// ACP handshake through initialize. It does not open a session; call
// NewSession for that, so the caller can choose the working directory and the
// MCP servers to inject.
//
// On any failure the child is killed before returning, so a caller that gets
// an error has no process to clean up.
func Spawn(ctx context.Context, cfg ProcessConfig, info ClientInfo, h Handler) (*Process, error) {
	if cfg.Command == "" {
		return nil, fmt.Errorf("acp: no command to launch")
	}

	cmd := exec.Command(cfg.Command, cfg.Args...)
	cmd.Dir = cfg.Dir
	cmd.Env = cfg.Env
	if cfg.Stderr != nil {
		cmd.Stderr = cfg.Stderr
	} else {
		cmd.Stderr = io.Discard
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("acp: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("acp: stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("acp: launch %s: %w", cfg.Command, err)
	}

	p := &Process{
		Client: NewClient(stdout, stdin, info, h),
		cmd:    cmd,
	}
	p.Start()

	if _, err := p.Initialize(ctx); err != nil {
		_ = p.Kill()
		return nil, fmt.Errorf("acp: initialize %s: %w", cfg.Command, err)
	}
	return p, nil
}

// Wait closes the session and the stream, then waits for the child to exit.
//
// An agent that exits non-zero after being asked to stop is normal, so the
// caller gets the exit error to interpret rather than having it swallowed.
func (p *Process) Wait() error {
	_ = p.CloseSession()
	_ = p.Close()
	<-p.Done()
	return p.cmd.Wait()
}

// Kill terminates the child without waiting for it to shut down cleanly. Use
// it when the agent is unresponsive or when a launch failed partway.
func (p *Process) Kill() error {
	_ = p.Close()
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
	_, err := p.cmd.Process.Wait()
	return err
}

// Pid is the child's process id, useful for logging and for a supervisor that
// wants to observe the process independently.
func (p *Process) Pid() int {
	if p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

// Environ returns the current environment with the given key set to value,
// replacing any existing entry. It is the usual way to build ProcessConfig.Env
// for selecting an agent profile.
func Environ(key, value string) []string {
	out := make([]string, 0, len(os.Environ())+1)
	prefix := key + "="
	for _, kv := range os.Environ() {
		if len(kv) >= len(prefix) && kv[:len(prefix)] == prefix {
			continue
		}
		out = append(out, kv)
	}
	return append(out, prefix+value)
}
