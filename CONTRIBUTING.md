# Contributing

## Agents run in harness-test's container, never on your machine

Nothing that executes an agent binary runs on a developer's machine: not a
test, not a measurement, not a quick timing loop. That covers everything in
this module that shells out — `harness.DetectInstalled`/`DetectAll`/
`DetectOne` (each runs the agent's `--version`), `harness.CheckLogin`/
`CheckLoginVersion`, and every `driver` backend — and every tool built on
them.

Why it is a rule and not a preference:

- A command that looks harmless is not, across versions. pi 0.80.3 predates
  `pi auth check` and reads "auth check" as a prompt: on a signed-in machine
  the "status check" starts a billed model turn. Only the container, with a
  mock model, makes that a finding instead of a bill.
- The machine you are on has real logins, real session stores
  (`~/.claude/projects`, `~/.codex/sessions`, ...) and real config. An agent
  started there can write into them, resume a real conversation, or act with
  a real account.
- Results from one developer's machine are not reproducible. The container
  pins the agent versions, uses a mock model, and is what the compatibility
  matrix is measured against.

How to work instead:

- **Unit tests** use fixtures: build `DetectResult`s by hand, fake a
  `driver.Backend`/`Session`, point `HOME` and `PATH` at temp dirs so nothing
  real is found. A test that would find and run an installed agent is a bug.
- **Anything that needs a real agent** — a new driver, a status check, a
  version probe, a timing — runs in `inference-sh/harness-test`'s container,
  against its mock model.
- **Callers of this module** (belt's daemon, the api) follow the same rule
  for their own tests.
