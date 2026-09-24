# transcript

Read and write the conversations coding agents keep on disk.

Every agent persists its sessions somewhere under `$HOME` in a format of its
own. This package gives those files one shape, so a caller can list an
agent's sessions, read one, or write one the agent will then load. That is
what makes "bring your own chat history" a registry fact rather than a
per-agent hack: an agent's store is described where the agent is described,
next to its launch command and hook format.

## Model

A `Session` is a list of `Entry`, each with a role and content blocks. It is
message-grained because every store on disk is. The vendor's own row travels
along on the entry as `Raw`, so a session read from an agent and written back
to the same agent is byte-for-byte what it was, whatever the mapping did or
did not understand. Only a session that came from somewhere else goes through
the encoder, and that is where any loss lives.

`Session.Events()` projects the entries onto the `agentprotocol` event stream
every other consumer already reads. It is a projection, not a second model.

## Adding an agent

Most stores are a directory of one JSON-lines file per session. For those, a
codec is a `transcript.JSONL` value: a `Layout` that says where the files are
and two functions that say what one row is.

```go
var Codec = transcript.JSONL{
	Layout: transcript.Layout{
		Root:    ".myagent/sessions",
		Project: transcript.MangledCwd, // how the cwd names a subdirectory
		Ext:     ".jsonl",
	},
	Decode: decode, // row -> Entry
	Encode: encode, // Entry -> row, for a session from another agent
}
```

`Decode` returns `ok == false` for a row that is not a message; the engine
keeps it as an opaque entry so a same-agent write loses nothing. A format
with a header row sets `Header`; one that links rows to a parent sets `Tree`;
one that keeps an index or sidecar beside the transcript sets `After`. A
store whose identity is not in the file name sets `Layout.Peek`. Agents that
keep a database implement `Store` directly; those live in the `sqlite`
module so the core needs no driver.

Then name the codec on the harness's registry row:

```go
"myagent": {
	Sessions: myagent.Codec,
	// ...
},
```

Every codec ships one session captured from a real run under `testdata` and
runs the shared conformance check in `transcripttest`: read it, write it,
read it again, require the file is unchanged and the events hold a turn. A
codec cannot exist without a sample.

## Listing and liveness

`all.List(ctx, home, cwd)` lists every agent's sessions under a home in one
call, newest first, each with its agent. An agent with no store contributes
nothing; a store that fails is reported beside the results and does not stop
the others.

`all.Live(home, session)`, or `all.NewProbe(home).Live(session)` to check
many sessions from one read of the process table, says whether a process is
using a session now, and what the answer rests on:

| Evidence | Meaning | Proof? |
|---|---|---|
| `lock-file` | the agent's in-use marker for the session names a running process (Copilot's `inuse.<pid>.hold`) | yes |
| `open-file` | an agent process holds the session's own file or directory open (codex, grok, qwen, omp, cursor's blob store) | yes |
| `no-process` | no process of the agent is running | yes, idle |
| `process-in-cwd` | an agent process runs in the session's directory, and may be serving another session there | heuristic |
| `recent-write` | the session was written in the last two minutes | heuristic |
| `no-process-in-cwd` | the agent runs, but elsewhere | heuristic, idle |

Which of these each agent produces was measured by recording its processes
and open files while a harness-test session was live. Agents that keep every
session in one database (goose, opencode, kilo, hermes) or open and close
their transcript per write (claude, gemini, droid, kimi, pi) give no
per-session proof; for them the answer is the labelled directory heuristic.
It reads only metadata: process arguments, working directories, HOME, open
file paths, and in-use markers. The process table is read from /proc, so
off Linux only in-use markers and recent writes apply, and anything else is
`unknown`. Treat a heuristic or unknown answer as a reason to warn before
continuing a session, not as proof.

## Writing back

A write never disturbs what it read. Entries read from a store carry their
vendor row as `Raw`, and every writer keeps those rows exactly: a JSONL
writer emits them unchanged, and a database writer leaves their rows in
place, removes rows of entries the session no longer holds, and inserts only
new entries. Rewriting a session unchanged leaves the agent's files and
tables byte-for-byte what they were; the `sqlite` module checks this, and an
append into the agent's own database, against every sample.

New entries are written with every field the agent validates on load, taken
from the agent's own source or from a real file it wrote. Where a value has
no source in the session (the model an opencode message ran with, kiro's
agent name), it is copied from the agent's latest session in the same store.

## Coverage

Pure-Go codecs, one package each: claude, codex, copilot (read-only), cursor
(read-only transcript), droid, gemini, grok, kimi, kiro, pi, omp (pi's
format), and qwen. Database-backed codecs in the `sqlite` module: goose,
hermes, opencode, kilo (opencode's schema), cursor's blob store, and copilot
with its index.

Two agents need the `sqlite` module to write. Copilot finds sessions through
its index, `session-store.db`, so a session without a row there does not
load; `transcript/copilot` reads, and the `sqlite` module's Copilot writes the
files and the index. Cursor never loads its readable transcript back;
`transcript/cursor` reads it, and the `sqlite` module's Cursor reads and
writes the blob store, tool results included. `all.Open` prefers the
`sqlite` codec for an agent when that module is imported.

Every codec's `testdata` sample comes from one run of the agent in the
harness-test container, not from any developer's machine, so the conformance
suite reproduces from a clean checkout. The JSONL agents are captured by
copying the session file the run wrote. The SQLite agents write through a
write-ahead log, so the capture waits for the flush after `session/close`
and folds the log into the main file with `PRAGMA wal_checkpoint(TRUNCATE)`
before copying; readers see the log in any case. cursor writes its store only
once its backend checkpoints the conversation, which the harness-test mock
does from d7e0451 on, with its own tool names and ids from b486703.

Some formats hold more than one view of a conversation, and a writer has to
fill the one the agent reads:

- kimi's wire log has context rows, which it rebuilds the model's context
  from, and transcript rows, which it shows. A session written with only the
  transcript rows resumes and the model sees none of it.
- kiro keeps a sidecar beside each transcript; a hand-built session gets a
  complete one.
- opencode validates every message and part against its schema on load.

windsurf is an IDE with no CLI and no local store, so it has no codec.
