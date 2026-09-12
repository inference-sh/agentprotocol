# agentprotocol

The vocabulary an agent run speaks: events, run state, interrupts, lifecycle hooks, and tool contracts.

```
go get github.com/inference-sh/agentprotocol
```

Plain data types, no behaviour beyond state-machine rules and JSON handling. No database, no transport, no dependencies outside the standard library.

## Why it exists

Several programs need to describe the same thing: an agent started, streamed some output, called a tool, needed a human, finished. The server that runs the loop, a conformance suite that judges other agents, and a client that drives a local harness all speak it. When each keeps its own copy the definitions drift, and the drift surfaces as a wrong status somewhere far from the change.

One definition, imported by everyone.

## What's in it

**Events.** `AgentEvent` and its payloads: run started, state changed, turn started and completed, content delta, tool started and completed, approval required and resolved, hook executed, usage updated, context compacted, error.

**State.** `AgentRunState` with its legal transitions, plus `InterruptReason`, `InterruptStatus` and `InterruptResolution`.

**Hooks.** `HookEvent`, `HookDecision`, and the request and response shapes a hook handler exchanges.

**Tools.** `Tool` definitions, `ToolCall`, `ToolType`, and `ToolInvocationStatus` with its transition rules.

## Subpackages

```
agentprotocol      the lifecycle model above — stdlib only
├── a2a/           Agent2Agent wire types, and mapping to and from the model
├── acp/           Agent Client Protocol: wire types and a client
└── driver/        one contract for running an agent over any of them
```

Imports flow downward only. `a2a` and `acp` depend on the root; `driver` depends on all three. Nothing depends on a consumer, and the transport packages never talk to each other. Two adapters translating directly is how you end up writing N² translators instead of N.

### a2a

Wire types for [A2A](https://a2a-protocol.org) v1.0, plus total mappings between task state and run state. The mapping functions are pure, so the server answering A2A calls and a client making them share one definition. An unrecognised state maps to failed rather than working: a caller can retry a failure, but waits forever on a task nobody is advancing.

### acp

[ACP](https://agentclientprotocol.com) is how editors drive coding agents running as local processes. Zed, JetBrains and VS Code speak it; Claude Code, Codex, Gemini CLI and Cursor answer it.

The client holds no policy. What happens when an agent asks permission, or asks to read a file, is the caller's decision, supplied as a `Handler`. A conformance suite auto-approves. A product forwards the question to a human. Defaults are conservative: no handler means cancel, never allow.

```go
proc, err := acp.Spawn(ctx, acp.ProcessConfig{
    Command: "claude-code-acp",
    Env:     acp.Environ("CLAUDE_CONFIG_DIR", profileDir),
}, acp.ClientInfo{Name: "belt"}, acp.Handler{
    OnUpdate:     func(n acp.UpdateNotification) { ... },
    OnPermission: askAHuman,
})
```

That `Env` line is the point: the agent is already logged in as the user, and pointing it at a profile directory selects which account it uses. No credential passes through this package.

### driver

One `Backend` and one `Session`, so the code above never branches on which kind of agent it is talking to. Progress leaves a session exactly one way, through `Events()`. Decisions go back exactly one way, through `Resolve`.

`ACPBackend` is the first implementation, which is why the interface has the shape it does rather than the shape a design document would have given it.

## Design rules

- Nothing here imports anything of ours. This package sits at the bottom.
- State machines expose their transitions (`CanTransitionTo`) rather than leaving callers to rediscover them.
- Mappings between vocabularies are total and written out longhand, even where both sides currently use identical strings. Only one of the two specs is ours to change, and divergence should break a test rather than produce a quietly wrong status.
- Agent-initiated requests are always answered. A dropped request leaves the agent blocked on its own timeout, which presents as a hang far from the cause.
- `sql.go` is optional. It implements `driver.Valuer` on the string enums so they survive a database round trip. Delete it and the package still describes the protocol completely.

## Stability

Pre-1.0. The event and state names are in production and unlikely to move. `driver` is the newest and most likely to change as more backends arrive.
