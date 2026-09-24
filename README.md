# agentprotocol

[![Go Reference](https://pkg.go.dev/badge/github.com/inference-sh/agentprotocol.svg)](https://pkg.go.dev/github.com/inference-sh/agentprotocol)

A Go library for describing what an AI agent is doing, and for driving agents that run somewhere else.

Source-available, not open source. See [LICENSE](LICENSE).

Every agent run has the same shape no matter who built it. It starts. It streams output. It calls tools. Sometimes it needs a human to approve something. Then it finishes. This library gives that shape a set of Go types, and adds adapters for two protocols that carry it across a process or network boundary.

The core has no dependencies outside the standard library.

## Drive a coding agent

The `acp` package speaks the [Agent Client Protocol](https://agentclientprotocol.com), which is how editors talk to coding agents running as local processes. Zed, JetBrains and VS Code extensions use it. Gemini CLI and Cursor answer it directly; Claude Code and several others answer it through a small bridge process.

```go
proc, err := acp.Spawn(ctx, acp.ProcessConfig{
	Command: "claude-code-acp",
	Dir:     "/path/to/project",
}, acp.ClientInfo{Name: "my-app", Version: "1.0"}, acp.Handler{
	OnUpdate: func(n acp.UpdateNotification) {
		fmt.Print(n.Update.Text())
	},
	OnPermission: func(ctx context.Context, r acp.PermissionRequest) (acp.PermissionResponse, error) {
		// The agent wants to run something. Decide however you like:
		// prompt a user, check a policy, forward it to a web UI.
		if id, ok := r.PickOption(acp.OptionKindAllowOnce); ok {
			return acp.Selected(id), nil
		}
		return acp.Cancelled(), nil
	},
})
if err != nil {
	return err
}
defer proc.Wait()

if _, err := proc.NewSession(ctx, "/path/to/project", nil); err != nil {
	return err
}
_, err = proc.Prompt(ctx, "add a test for the parser")
```

The client handles the protocol and nothing else. Every decision is yours, supplied as a `Handler`. Leave a field nil and the library takes the conservative option: an unanswered permission request is cancelled, never allowed, and an agent is told during the handshake that it may only ask for what you can actually answer.

No credentials pass through this library. The agent authenticates itself, the way it does when a person runs it. `ProcessConfig.Env` is how you choose which account it uses, by pointing the agent at one of its own profile directories:

```go
Env: acp.Environ("CLAUDE_CONFIG_DIR", "/home/me/.claude-work")
```

## The model

The root package is the vocabulary, as plain data.

**Events.** `AgentEvent` carries a type, the run it belongs to, and a JSON payload. Thirteen payload types cover the run lifecycle: started, state changed, turn started and completed, content delta, tool started and completed, approval required and resolved, hook executed, usage updated, context compacted, error.

```go
ev := agentprotocol.NewEvent(agentprotocol.AgentEventToolStarted, runID, chatID,
	agentprotocol.ToolStartedPayload{ToolName: "bash", ToolType: agentprotocol.ToolTypeCall})

if p, ok := agentprotocol.PayloadAs[agentprotocol.ToolStartedPayload](ev, agentprotocol.AgentEventToolStarted); ok {
	fmt.Println(p.ToolName)
}
```

Payloads stay as raw JSON until asked for, so a consumer can route on type without decoding bodies it will discard, and an event written by a newer producer still survives a round trip through an older reader.

**State.** `AgentRunState` is the run's lifecycle, and it knows its own legal transitions:

```go
if state.CanTransitionTo(agentprotocol.AgentRunStateCompleted) { ... }
state.IsTerminal()
state.IsInterrupted()
```

`ToolInvocationStatus` does the same for a single tool call. Both expose their transition rules rather than leaving each caller to rediscover them.

**Interrupts.** When a run stops to wait for a person, `InterruptReason` says why, `InterruptStatus` tracks the wait, and `InterruptResolution` records the answer.

**Hooks.** `HookEvent`, `HookDecision`, and the request and response shapes a lifecycle hook handler exchanges.

**Tools.** `Tool` definitions, `ToolCall`, `ToolType`, and helpers for building parameter schemas.

## Packages

```
agentprotocol      the model above, standard library only
├── a2a/           Agent2Agent wire types and mappings
├── acp/           Agent Client Protocol client
├── codexapp/      `codex app-server` JSON-RPC client, types generated from codex's own schema
├── driver/        one interface for running an agent over any transport
├── pirpc/         `pi --mode rpc` JSONL client
└── harness/       the registry of coding-agent CLIs: which exist, how to find them, how to drive them
```

Imports only ever flow downward. `a2a` and `acp` depend on the root; `codexapp` and `pirpc` depend on nothing but the standard library. `driver` depends on all of them. `harness` depends on nothing but the standard library. The transport packages never reference each other, which is deliberate: adapters that translate directly between formats grow as the square of how many formats you support, while adapters that translate to a shared model grow linearly.

### harness

The registry of coding-agent CLIs — claude, codex, gemini, cursor, goose, kiro and the rest — with what each one needs to be found and driven: binary name, config and hook directories, hook file format, the mode it answers ACP in, install command, known quirks. `DetectInstalled` reports which are on a machine and where.

It lives here rather than in a test suite because three things consume it at runtime: a CLI installing hooks into an agent, a daemon reporting which agents a machine hosts, and a server deciding how to launch one. One registry, or the ids drift. The conformance suite that proves it against real agents is [harness-test](https://github.com/belt-sh/harness-test).

### a2a

Wire types for [A2A](https://a2a-protocol.org) v1.0, and total mappings between an A2A task state and a run state, both directions. The mapping functions are pure, so a server answering A2A calls and a client making them can share one definition.

A state neither side recognises maps to failed rather than working. A caller can retry a failure; it waits forever on a task nobody is advancing.

### driver

`Backend` opens sessions. `Session` runs one conversation. Progress leaves through a single channel of `AgentEvent`, and decisions go back through a single `Resolve` call, so code above a driver never branches on what kind of agent is underneath.

```go
sess, err := backend.Open(ctx, driver.SessionConfig{RunID: id, WorkDir: dir})
if err != nil {
	return err
}
defer sess.Close()

go func() {
	for ev := range sess.Events() {
		switch ev.Type {
		case agentprotocol.AgentEventContentDelta:
			// stream it to a user
		case agentprotocol.AgentEventApprovalRequired:
			p, _ := agentprotocol.PayloadAs[agentprotocol.ApprovalRequiredPayload](ev, ev.Type)
			// ask someone, then:
			sess.Resolve(ctx, p.ToolInvocationID, driver.Allow())
		}
	}
}()

err = sess.Prompt(ctx, driver.TextInput("what changed in this repo today?"))
```

`ACPBackend` drives any agent that speaks ACP. `CodexBackend` drives Codex natively through `codex app-server`, the server its IDE extension uses. `PiBackend` drives pi over its own RPC mode; pi has no ACP. `Capabilities()` reports what a backend supports so callers can degrade rather than call something that will fail.

## Notes on the design

- **The core depends on nothing.** Not on a database, a web framework, a transport, or any other module. That is what lets a server, a test suite and a command line tool share it.
- **Mappings between vocabularies are total and written out in full**, even where both sides currently use identical strings. When only one of two specifications is yours to change, the day they diverge should break a test rather than quietly produce a wrong status on the wire.
- **Every request from an agent gets an answer**, including methods this library does not know, which receive a JSON-RPC method-not-found. Dropping a request silently leaves the agent blocked on its own timeout, which shows up as a hang a long way from the cause.
- **`sql.go` is optional.** It implements `driver.Valuer` on the string enums so they survive a round trip through a database, because named string types are not encoded by some drivers otherwise. Nothing else in the package imports it, and deleting the file leaves the protocol fully described.

## Status

Pre-1.0, and versioned accordingly.

The event names, run states and tool contracts are in production at [inference.sh](https://inference.sh), which is where this came from, and are unlikely to move. `driver` is the newest part and the most likely to change as more backends arrive.

Bug reports are useful, particularly agents whose ACP behaviour differs from what this client expects. That corner of the ecosystem is less uniform than the specification suggests. Open an issue or write to hello@inference.sh.
