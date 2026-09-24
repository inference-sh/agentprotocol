package acp

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"
)

// Process is an ACP agent running as a local subprocess, with a Client already
// wired to its stdio.
//
// Most ACP agents are launched this way, so this saves every caller the same
// pipe plumbing. Callers that already have a stream, over a socket or in a
// test, should use NewClient directly instead.
type Process struct {
	*Client
	cmd   *exec.Cmd
	state *os.ProcessState

	// ShutdownGrace is how long Wait lets the agent finish after
	// session/close before the stream is closed. Zero means
	// DefaultShutdownGrace; negative means close at once.
	ShutdownGrace time.Duration

	// ExitGrace is how long Wait lets the agent exit after its stdin is
	// closed before killing it. Zero means DefaultExitGrace.
	ExitGrace time.Duration
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

	// AuthMethodID selects one of the agent's advertised auth methods, called
	// between initialize and session setup.
	//
	// Empty skips authentication, which is right for almost every agent: the
	// user logged in with the vendor's own CLI and the credential is already
	// where the agent looks for it. Set this only for an agent that advertises
	// a method and refuses to work without one — Client.AuthMethods reports
	// what it offered.
	AuthMethodID string

	// Stderr receives the agent's diagnostics. Nil discards them. Agents are
	// chatty here and it is usually the only clue when a launch fails, so
	// capturing it is worth the buffer.
	Stderr io.Writer
}

// Spawn launches the agent, starts the client's read loop, and completes the
// ACP handshake through initialize, then authenticate if the caller named a
// method. It does not open a session; call NewSession for a new one or
// LoadSession to resume one the agent persisted, so the caller can choose the
// working directory and the MCP servers to inject.
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

	if cfg.AuthMethodID != "" {
		if err := p.Authenticate(ctx, cfg.AuthMethodID); err != nil {
			_ = p.Kill()
			return nil, fmt.Errorf("acp: authenticate %s: %w", cfg.Command, err)
		}
	}
	return p, nil
}

// DefaultShutdownGrace is how long Wait gives an agent to finish its own
// shutdown after session/close before the stream is taken away.
//
// Agents run end-of-session work here: writing a transcript, firing Stop
// hooks, flushing telemetry. Closing stdin immediately after session/close
// cuts that off partway, and the symptom is a hook that mostly fires.
const DefaultShutdownGrace = 5 * time.Second

// DefaultExitGrace is how long Wait gives an agent to exit once its stdin is
// closed before it kills it.
//
// Most agents exit on end of input. Cursor's `agent acp` (2026.09.23) does
// not: it keeps running with stdin closed, and a Wait that waited for the
// stream to end never returned.
const DefaultExitGrace = 5 * time.Second

// Wait shuts the session down and waits for the child to exit.
//
// It sends session/close, gives the agent ShutdownGrace to finish and close
// its own side, then closes the stream and reaps the process. An agent that
// exits promptly is not delayed: the grace ends as soon as the stream does.
// An agent still running ExitGrace after its stdin closed is killed.
//
// An agent exiting non-zero after being asked to stop is common, so the exit
// error is returned for the caller to interpret rather than swallowed.
func (p *Process) Wait() error {
	_ = p.CloseSession()

	grace := p.ShutdownGrace
	if grace == 0 {
		grace = DefaultShutdownGrace
	}
	if grace > 0 {
		timer := time.NewTimer(grace)
		select {
		case <-p.Done():
		case <-timer.C:
		}
		timer.Stop()
	}

	_ = p.Close()
	exit := p.ExitGrace
	if exit <= 0 {
		exit = DefaultExitGrace
	}
	timer := time.NewTimer(exit)
	defer timer.Stop()
	select {
	case <-p.Done():
		return p.cmd.Wait()
	case <-timer.C:
	}
	// cmd.Wait closes our end of stdout once the child is gone, which ends
	// the read loop even if a grandchild still holds the pipe.
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
	err := p.cmd.Wait()
	<-p.Done()
	return err
}

// Kill terminates the child without waiting for it to shut down cleanly and
// reaps it. Use it when the agent is unresponsive, when a launch failed
// partway, or to reap an agent that has already exited on its own; in that
// last case ExitState reports how it ended rather than the kill.
func (p *Process) Kill() error {
	_ = p.Close()
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
	st, err := p.cmd.Process.Wait()
	p.state = st
	return err
}

// ExitState is how the child ended, once Kill has reaped it; nil before that
// or after Wait, which reports the exit through its error instead.
func (p *Process) ExitState() *os.ProcessState {
	return p.state
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
