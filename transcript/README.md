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

A store is not the conversation. Agents keep rows the person never sees
(context they inject, the summary a compaction leaves), rows the model is no
longer given (history a compaction retired, a turn the person undid), and
rows whose meaning depends on others (one message split across rows, a
marker that pins the leaf). A codec maps each agent's own rules onto the
entries, taken from the agent's loader, so three views stay distinct:

| View | What it is |
|---|---|
| `Messages()` | every message row the store holds, in store order |
| `Linearize()` | the conversation the person sees: the active branch, without undone turns or model-only context |
| `Context()` | what the agent gives the model when it resumes: the active branch for the model, every compaction applied |

`Entry.Audience` says who an entry is for (everyone, the model, the person,
or nobody). `Entry.Compaction` marks a row that replaces the history before
it with a summary, keeping the entries from `Keep` on. `Session.Leaf` pins
the entry a tree store resumes from, when the agent's rule is not "the last
linked row". A JSONL codec sets them in `Finish`, which runs after every row
is decoded.

`Session.Events()` projects the conversation the person sees onto the
`agentprotocol` event stream every other consumer already reads. It is a
projection, not a second model.

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

A one-turn sample proves the rows decode; it cannot show what the agent does
with them when a session is compacted, rewound, undone or forked. The rules
for that come from the agent's own session loader, read at the source (or,
for a closed-source agent, from what it replays over ACP `session/load`),
and each codec carries samples of those sessions captured the same way.

## Listing and liveness

`all.ListLive(ctx, home, cwd)` lists every agent's sessions under a home,
newest first, each with its agent and whether a process is using it now. An
agent with no store contributes nothing; a store that fails is reported
beside the results and does not stop the others. `all.List` is the listing
alone, and `all.NewProbe(home)` checks sessions one at a time from one read
of the process table.

Each answer carries the evidence it rests on:

| Evidence | Meaning | Proof? |
|---|---|---|
| `lock-file` | the agent's own record names a running process of it as holding the session: Claude Code's `~/.claude/sessions/<pid>.json`, Copilot's `inuse.<pid>.hold` | yes |
| `open-file` | an agent process holds the session's own file or directory open (codex, grok, qwen, omp, cursor's blob store) | yes |
| `no-process` | no process of the agent is running | yes, idle |
| `held-elsewhere` | every agent process in the session's directory holds another session by the agent's own record | yes, idle |
| `process-in-cwd` | an agent process started in the session's directory, and may be serving another session there | heuristic |
| `recent-write` | the session was written in the last two minutes | heuristic |
| `no-process-in-cwd` | the agent runs, but not in the session's directory | heuristic, idle |

What each agent leaves while a session is live was measured by recording
its processes and open files during a harness-test session. A process is
named by its first argument, or by the script under an interpreter, never
by its executable (Claude Code's search helper runs from Claude's own
binary). The heuristic answers remain for agents that record nothing per
session and keep every session in one database or open and close their
transcript per write (goose, opencode, kilo, hermes, gemini, droid, kimi,
pi), and for processes that predate an agent's registry. Listing a real
home with a dozen Claude Code sessions open proves all but a handful of
answers either way.

A session file copied into place just now reads as `recent-write`, since
its modification time is the copy's. Tests that list a copied sample should
backdate it (`os.Chtimes`) to see the evidence a real idle session gives.

It reads only metadata: process arguments, working directories, HOME, open
file paths, and the agents' registries and markers. The process table is
read from /proc, so off Linux only markers (their pids checked with signal
0) and recent writes apply, and anything else is `unknown`. Treat a
heuristic or unknown answer as a reason to warn before continuing a
session, not as proof.

## Writing back

A session carries the agent it was read from. Written to that agent, its
rows go back as they were. Written to any other, it goes through
`Session.Portable`: the conversation the person sees, re-encoded in the
target's format and linked in order, since one agent's rows mean nothing in
another's store and its links may run through rows that do not survive the
move. `transcripttest.Imported` checks every writer for it.

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
