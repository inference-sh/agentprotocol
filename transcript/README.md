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

## Coverage

Pure-Go codecs, one package each: claude, codex, copilot, cursor, droid,
gemini, grok, kimi, kiro, pi, plus qwen (gemini's format) and omp (pi's
format). Database-backed codecs in the `sqlite` module: goose, hermes,
opencode, and kilo (kilo shares opencode's schema).

cursor has two codecs. Its store of record is a content-addressed blob
database, ~/.cursor/chats/<md5 of cwd>/<id>/store.db: each message is a JSON
blob keyed by its sha256, and a root blob lists the ids in order. The
`sqlite` module's `Cursor` reads and writes that store, tool results
included. From it Cursor derives a readable transcript whose writer drops
tool results; `transcript/cursor` reads that file for callers that must stay
driver-free, and is read-only, as is any JSONL codec that sets no `Encode`.
`all.Open` prefers the store-of-record codec when the sqlite module is
imported.

Every codec's `testdata` sample comes from one run of the agent in the
harness-test container, not from any developer's machine, so the conformance
suite reproduces from a clean checkout. The JSONL agents are captured by
copying the session file the run wrote. The SQLite agents write through a
write-ahead log, so the capture waits for the flush after `session/close`
and folds the log into the main file with `PRAGMA wal_checkpoint(TRUNCATE)`
before copying, or the copied file is empty. cursor writes its store only
once its backend checkpoints the conversation, which the harness-test mock
does from d7e0451 on, tool turn included from 4b2d12c.

windsurf is an IDE with no CLI and no local store, so it has no codec.
