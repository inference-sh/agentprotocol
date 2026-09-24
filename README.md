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

#### Signing in

`Harness.Auth` records how a user signs each agent in with their own account, measured in Docker (2026-09) on the version in `Auth.MeasuredOn`. Logins were started and killed, never completed, so every logged-in output comes from help or source. `CheckLogin` runs a status check bounded by `StatusTimeout`; only a check marked `Cheap` (no network, no prompt, no writes, measured) is safe to run on every enrollment. Credential paths are for existence checks only.

| agent | subscription login | headless | status check (logged out → exit, marker) | account selector |
|---|---|---|---|---|
| claude | Claude Pro/Max | paste code: `claude auth login`; or `claude setup-token` elsewhere → `CLAUDE_CODE_OAUTH_TOKEN` | `claude auth status --json` → 1, `"loggedIn": false` (cheap) | `CLAUDE_CONFIG_DIR` |
| codex | ChatGPT | device code: `codex login --device-auth` | `codex login status` → 1, `Not logged in` (API-key login prints a masked key; not cheap) | `CODEX_HOME` |
| copilot | GitHub Copilot | device code: `copilot login --device-code` (needs a pty or keyring to save); `COPILOT_GITHUB_TOKEN`/`GH_TOKEN` | none | `COPILOT_HOME` |
| cursor | Cursor | URL opened elsewhere: `NO_OPEN_BROWSER=1 cursor-agent login` | `cursor-agent status --format json` → 0, `"isAuthenticated": false` (writes a log) | `XDG_CONFIG_HOME` |
| droid | Factory account | device code, TUI or ACP `device-pairing` only; `FACTORY_API_KEY` | `droid doctor --auth --json` → any, `not logged in` (network, writes) | `FACTORY_HOME_OVERRIDE` |
| gemini | Google account | paste code in the TUI (needs a TTY) | none | `GEMINI_CLI_HOME` |
| goose | via providers (ChatGPT, Copilot, SuperGrok, other CLIs) | per provider in `goose configure` | none | `GOOSE_PATH_ROOT` |
| grok | SuperGrok / X Premium | device code: `grok login --device-auth` | none (`grok models` first line, writes) | `GROK_HOME` |
| hermes | via providers (ChatGPT, Claude, Nous, SuperGrok) | device code / paste: `hermes auth add <p> --type oauth --no-browser` | per provider, not recorded (writes, may refresh) | `HERMES_HOME` |
| kilo | Kilo account, plus ChatGPT/Copilot/SuperGrok | device code: `kilo auth login -p kilo` | none (`kilo auth list` lists providers, 4.6s, writes) | `XDG_DATA_HOME` |
| kimi | Kimi Code plan | device code: `kimi login` | none | `KIMI_CODE_HOME` |
| kiro | Kiro Free/Pro, Identity Center | device code: `kiro-cli login --license free --use-device-flow` | `kiro-cli whoami --format json` → 1, `"account":null` (writes its db) | `XDG_DATA_HOME` |
| omp | via providers (ChatGPT, Claude, Copilot, Gemini, ...) | device code or paste: `omp login` | none | `OMP_PROFILE` |
| opencode | via providers (ChatGPT, Copilot, SuperGrok) | device code: `opencode auth login -p openai -m "ChatGPT Pro/Plus (headless)"`; `OPENCODE_AUTH_CONTENT` | none (`opencode auth list`, writes) | `XDG_DATA_HOME` |
| pi | via providers (ChatGPT, Claude, Copilot, SuperGrok, Kimi) | device code or paste in the TUI's `/login` | `pi auth check --provider <p> --json --no-refresh` per provider → 1, `not_ready` (cheap) | `PI_CODING_AGENT_DIR` |
| qwen | Alibaba Coding Plan key (Qwen OAuth free tier discontinued) | env: `BAILIAN_CODING_PLAN_API_KEY` | none | `QWEN_HOME` |

Windsurf is IDE-only and has no entry.

`CredentialsPresent(name, env...)` is the cheaper, weaker signal for an agent with no `Cheap` check: it stats (never opens) the agent's `Auth.LoginFiles` for the account `env` selects, following the same selector as the table above (`CLAUDE_CONFIG_DIR`, `XDG_DATA_HOME`, `OMP_PROFILE`, ...; a relative value for a directory variable is ignored, as XDG and goose ignore it). A file that exists may hold an expired or revoked token; a missing one says nothing for an agent that keeps its login in the keyring (`Auth.Keyring`) or reads it from an env var. Copilot, kiro and omp have no login file whose existence proves a login, so it always says no for them. Order for a caller: a `Cheap` status check, then `CredentialsPresent`, then the config directory.

#### Supported versions

`Harness.Tested` is the range of each agent's versions inference has verified, with the evidence. `Harness.Requires` is a separate hard floor: the version a capability the session driver cannot work without appeared in, set only where that capability is known to be missing below it. `harness.Support(name, version)` compares an installed version against both and runs nothing; `DetectResult.Support()` does the same with the version detection read. The verdict's `Level` is one of:

| level | meaning | caller |
|---|---|---|
| `supported` | inside the tested range | drive it |
| `newer-than-tested` | above `Tested.Max` | drive it, warn |
| `older-than-tested` | below `Tested.Min`, not below `Requires` | drive it, warn: it may work |
| `older-than-supported` | below `Requires`: a capability the driver needs is missing | do not drive; show `Reason` and `UpgradeCmd` |
| `unknown` | version unreadable, agent unknown or untested | drive it, warn |

`Reason` is a sentence for end users ("Pi Coding Agent 0.80.3 has no RPC agent_settled event, which the pi driver ends a turn on, added in 0.80.4"; "Claude Code 2.1.280 is older than the oldest version inference has tested (2.1.281); it may work"). `TestedMin`, `TestedMax`, `Requires` and `UpgradeCmd` (the `UpgradeCmd` field where `InstallCmd` does not upgrade, as with pip, else `InstallCmd`) are data to render.

`driver.ForHarness` stays version-agnostic. `driver.ForHarnessVersion(h, version, env)` returns the backend and the verdict, refuses only `older-than-supported` (with an `*UnsupportedVersionError` carrying the verdict), and tells the backend the version.

| agent | tested min | tested max | how verified | floor (`Requires`) | floor evidence |
|---|---|---|---|---|---|
| claude | 2.1.281 | 2.1.282 | harness-test CI; agentprotocol CI pins 2.1.281 | — | |
| codex | 0.156.1 | 0.156.1 | harness-test CI; agentprotocol CI pins 0.156.1; codexapp generated from it | 0.56.0 | app-server `thread/start`, `thread/resume`, `turn/start`, `turn/interrupt`, `turn/completed`, `item/*` are in `app-server-protocol/src/protocol/common.rs` at `rust-v0.56.0` and absent at `rust-v0.55.0` |
| copilot | 1.0.86 | 1.0.88 | harness-test CI | — | |
| cursor | 2026.07.23 | 2026.09.23 | harness-test CI; `--agent-version` runs for 2026.07.23 | — | `cursor-agent acp` is in every bundle read, 2025.12.17 through 2026.09.23; no version without it found |
| droid | 0.223.0 | 0.226.2 | harness-test CI | — | |
| gemini | 0.60.0 | 0.61.0 | harness-test CI | — | |
| goose | 1.51.0 | 1.52.0 | harness-test CI | — | |
| grok | 1.0.34 | 1.0.41 | harness-test CI | — | |
| hermes | 0.13.0 | 0.19.0 | harness-test CI; `--agent-version` runs for 0.13.0 and 0.14.0 | — | |
| kilo | 7.7.5 | 7.7.9 | harness-test CI | — | |
| kimi | 2.0.2 | 2.1.1 | harness-test CI | — | |
| kiro | 2.22.1 | 2.24.0 | harness-test CI | — | |
| omp | 18.2.6 | 18.3.0 | harness-test CI | — | |
| opencode | 1.18.31 | 1.18.32 | harness-test CI | — | |
| pi | 0.87.1 | 0.87.1 | harness-test CI; agentprotocol CI pins 0.87.1 | 0.80.4 | CHANGELOG 0.80.4 adds the RPC `agent_settled` event; the pi driver closes a turn only on it |
| qwen | 0.24.1 | 0.24.5 | harness-test CI | — | |

A floor of — refuses no version.

"`--agent-version` runs" means every job of that workflow passed in harness-test's container with the version pinned (2026-09-25). Cursor before 2026.07.23 does not read `CURSOR_API_ENDPOINT` (its `--endpoint` defaults to api2.cursor.sh), so harness-test's mock cannot reach it; that is a limit of the test setup, not of the driver. "harness-test CI" means the agent's workflow in belt-sh/harness-test finished green, every job including the session job that drives the agent through its `DriverKind` backend, with that version installed; versions come from the runs' `→ version:` log lines, 2026-09-20 to 2026-09-24. Claude, codex, cursor and pi have had the session job only since 2026-09-24, so older versions that passed their other modes are not counted (`Tested.Evidence` lists them). Nothing between two tested versions is claimed beyond that.

**Raising `Tested.Max`.** harness-test installs each agent with its `InstallCmd`, which takes the latest release, on every push and in the nightly; agentprotocol's `Agents` workflow runs a `latest` entry nightly next to its pins. When those are green on a newer release, set `Max` to it and add it to the evidence.

**Extending `Tested.Min`.** It needs a green harness-test run with the older version installed: every CI job (each mode from `--modes-for`, mock and belt hooks) with `harness-test --agent-version <v>`, which installs that exact version (npm `pkg@v`, pip `pkg==v`, or the agent's pinned installer for cursor, grok, kiro and goose), or the `Pinned agent version` workflow in belt-sh/harness-test, which runs the same jobs. Add the version and the run to the evidence.

#### Version-specific behaviour

When a flag, subcommand or protocol field exists only on some versions, the registry records it once as a `VersionRange` (`From` inclusive, `Before` exclusive) under a feature name in `Harness.Features`, and the driver asks `h.HasFeature(name, installedVersion)` before using it. The entry is not forked per version. Recorded so far: codex `turn/steer` (in the app-server schema from `rust-v0.99.0`), so a `CodexBackend` with an older `Version` reports `Steer: false` and returns an error for a prompt sent during a running turn; and pi `--session-id` (CHANGELOG: added in 0.76.0), so a `PiBackend` below that reports `Resume: false` and refuses a resume before starting pi. A feature only some versions have is a `Features` entry; a capability without which the driver cannot run at all is `Requires`. `StatusCheck.MinVersion` is the same rule for the login check. Versions are read with `ParseVersion`, which takes the first dotted number from `--version` output (`codex-cli 0.156.1`, `2.1.282 (Claude Code)`, `2026.09.23-86fc751`), and compared numerically per component by `CompareVersions`.

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

**Agent stderr.** `ACPBackend`, `ClaudeBackend`, `CodexBackend` and `PiBackend` each have `Stderr io.Writer`, with one meaning: it receives the agent's stderr as it is written, and nil keeps only a tail. The last 4 KiB are kept either way and quoted (`; agent stderr: ...`) in the error when `Open` fails and in the `process_exited` error when the agent dies; `ACPBackend` also quotes them in a `no_response` error.

**No turn waits forever on a silent ACP agent.** `initialize` and `session/new` are bounded by `acp.DefaultCallTimeout` (60s) and a resume by `ACPBackend.LoadTimeout`. A prompt is bounded only up to the agent's first sign of work (a message, thought, tool call or plan update, a permission request, or the prompt's response) by `ACPBackend.FirstEventTimeout`, default `DefaultFirstEventTimeout` (2 minutes), negative to disable. Past it the turn fails with an error event of code `no_response` and `session/cancel` is sent; the session stays open for the caller to close or kill. Session bookkeeping updates (available commands, mode, usage) and echoes of the user's prompt do not count as work. After the first sign the turn runs until the agent answers, up to `acp.DefaultPromptTimeout` (10 minutes) for the whole prompt. Against harness-test's mock the 13 ACP agents sent their first sign 0.01s to 2.07s after `session/prompt` and answered `session/new` within 2.53s (2026-09-25, two runs), and a real model adds its time to first token. A turn that ends with no message, thought, tool call or plan at all is reported through `OnDiagnostic` with the stderr tail: hermes before 0.18.0 ends a turn whose model call failed that way, the provider's error on stderr only.

## Notes on the design

- **The core depends on nothing.** Not on a database, a web framework, a transport, or any other module. That is what lets a server, a test suite and a command line tool share it.
- **Mappings between vocabularies are total and written out in full**, even where both sides currently use identical strings. When only one of two specifications is yours to change, the day they diverge should break a test rather than quietly produce a wrong status on the wire.
- **Every request from an agent gets an answer**, including methods this library does not know, which receive a JSON-RPC method-not-found. Dropping a request silently leaves the agent blocked on its own timeout, which shows up as a hang a long way from the cause.
- **`sql.go` is optional.** It implements `driver.Valuer` on the string enums so they survive a round trip through a database, because named string types are not encoded by some drivers otherwise. Nothing else in the package imports it, and deleting the file leaves the protocol fully described.

## Status

Pre-1.0, and versioned accordingly.

The event names, run states and tool contracts are in production at [inference.sh](https://inference.sh), which is where this came from, and are unlikely to move. `driver` is the newest part and the most likely to change as more backends arrive.

Bug reports are useful, particularly agents whose ACP behaviour differs from what this client expects. That corner of the ecosystem is less uniform than the specification suggests. Open an issue or write to hello@inference.sh.
