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

## Design rules

- Nothing here imports anything of ours. This package sits at the bottom.
- State machines expose their transitions (`CanTransitionTo`) rather than leaving callers to rediscover them.
- `sql.go` is optional. It implements `driver.Valuer` on the string enums so they survive a database round trip. Delete it and the package still describes the protocol completely.

Transport mappings for A2A and ACP, and a driver interface for running an agent over either, belong in subpackages here. They are not written yet, and will land when a second consumer needs them.

## Stability

Pre-1.0. The event and state names are in production and unlikely to move. Anything added later may change shape before 1.0.
