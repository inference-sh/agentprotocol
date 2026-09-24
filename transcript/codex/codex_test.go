package codex_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inference-sh/agentprotocol/transcript"
	"github.com/inference-sh/agentprotocol/transcript/codex"
	"github.com/inference-sh/agentprotocol/transcript/transcripttest"
)

// The samples are runs of codex 0.156 in the harness-test container against
// its mock server. The first is a headless run: instructions, a prompt, a
// mock exec_command call, an answer. The others were driven with codex exec
// and codex exec resume: a thread of three turns; a thread of four turns
// whose third resumed with model_auto_compact_token_limit=5, so Codex
// compacted before it; and a thread of three turns reverted to before its
// third through codex app-server's thread/revert, resumed for a fourth, then
// forked through thread/fork and resumed once in the fork. The reverted
// thread is two files and the fork a third.
const (
	sampleCWD = "/tmp/harness-test-codex-643346103/test-repo"
	sampleID  = "01a0cad9-6894-7800-b1c5-44db37a059d9"

	containerCWD = "/tmp/harness-test-codex-1895962888/test-repo"
	multiID      = "01a0d2bd-5cba-73b0-9685-afccf9a35373"
	compactID    = "01a0d2bd-6231-7420-8e6e-f0e9d20b29cd"

	revertCWD  = "/tmp/harness-test-codex-2939195279/test-repo"
	revertID   = "01a0d2d7-b2e6-7263-bb14-1e70e00407a4"
	forkID     = "01a0d2d7-bffa-7df0-9ddd-0f00431640bb"
	sessionDir = "testdata/home/.codex/sessions/2026/09/24/"
	revertBase = sessionDir + "rollout-2026-09-24T09-55-46-01a0d2d7-b2e6-7263-bb14-1e70e00407a4.jsonl"
	revertHead = sessionDir + "rollout-2026-09-24T09-55-48-01a0d2d7-b2e6-7263-bb14-1e70e00407a4_01a0d2d7-b98c-78d0-8cd2-f58020e2215e.jsonl"
	forkHead   = sessionDir + "rollout-2026-09-24T09-55-49-01a0d2d7-bffa-7df0-9ddd-0f00431640bb.jsonl"
)

var samples = []transcripttest.Sample{
	{Home: "testdata/home", CWD: sampleCWD, ID: sampleID},
	{Home: "testdata/home", CWD: containerCWD, ID: multiID},
	{Home: "testdata/home", CWD: containerCWD, ID: compactID},
	{Home: "testdata/home", CWD: revertCWD, ID: revertID},
}

func TestRoundTrip(t *testing.T) {
	for _, sample := range samples {
		t.Run(sample.ID, func(t *testing.T) {
			s := transcripttest.RoundTrip(t, codex.Codec, sample)
			if s.CWD != sample.CWD {
				t.Errorf("cwd = %q", s.CWD)
			}
			if _, ok := s.Vendor.(*codex.Vendor); !ok {
				t.Errorf("vendor = %T", s.Vendor)
			}
		})
	}
}

func TestToolCall(t *testing.T) {
	s := read(t, "testdata/home", sampleID)
	var calls, results int
	for _, e := range s.Messages() {
		for _, b := range e.Content {
			switch b.Kind {
			case transcript.BlockToolUse:
				calls++
				if b.Name != "exec_command" || string(b.Input) != `{"cmd":"cat README.md"}` {
					t.Errorf("tool_use = %+v", b)
				}
			case transcript.BlockToolResult:
				results++
			}
		}
	}
	if calls != 1 || results != 1 {
		t.Errorf("tool calls %d, results %d", calls, results)
	}
}

func TestForeign(t *testing.T) {
	transcripttest.Foreign(t, codex.Codec, "/tmp/some/project")
}

func TestAppend(t *testing.T) {
	for _, sample := range samples {
		t.Run(sample.ID, func(t *testing.T) { transcripttest.Append(t, codex.Codec, sample) })
	}
}

func TestForeignIDs(t *testing.T) {
	transcripttest.ForeignIDs(t, codex.Codec, "/tmp/some/project", func(id string) bool { return transcript.IsUUID(strings.TrimPrefix(id, "msg_")) })
}

func TestListsCWD(t *testing.T) {
	for _, sample := range samples {
		t.Run(sample.ID, func(t *testing.T) { transcripttest.ListsCWD(t, codex.Codec, sample) })
	}
}

func TestImported(t *testing.T) {
	for _, sample := range samples {
		t.Run(sample.ID, func(t *testing.T) { transcripttest.Imported(t, codex.Codec, sample) })
	}
}

func read(t *testing.T, home, id string) *transcript.Session {
	t.Helper()
	st, err := codex.Codec.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	s, err := st.Read(context.Background(), id)
	if err != nil {
		t.Fatalf("read %s: %v", id, err)
	}
	return s
}

// texts renders entries as role: text, eliding injected context to its
// opening tag.
func texts(es []transcript.Entry) []string {
	var out []string
	for _, e := range es {
		x := e.Text()
		if i := strings.IndexAny(x, "\n"); i >= 0 {
			x = x[:i]
		}
		out = append(out, string(e.Role)+": "+x)
	}
	return out
}

func equal(t *testing.T, what string, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("%s:\n  got  %q\n  want %q", what, got, want)
	}
}

// Codex injects its skills list as a developer message and the environment
// as a user message; the person sees neither, and the model is given both.
func TestInjectedContext(t *testing.T) {
	s := read(t, "testdata/home", multiID)
	equal(t, "shown", texts(s.Linearize()), []string{
		"user: Turn one: what is the project codename?",
		"assistant: Hello from mock server.",
		"user: Turn two: say it again.",
		"assistant: Hello from mock server.",
		"user: Turn three: once more.",
		"assistant: Hello from mock server.",
	})
	// What codex sent the mock server on the third turn, and its answer.
	equal(t, "context", texts(s.Context()), []string{
		"system: <skills_instructions>",
		"user: <environment_context>",
		"user: Turn one: what is the project codename?",
		"assistant: Hello from mock server.",
		"user: Turn two: say it again.",
		"assistant: Hello from mock server.",
		"user: Turn three: once more.",
		"assistant: Hello from mock server.",
	})

	s = read(t, "testdata/home", sampleID)
	for _, e := range s.Messages() {
		if strings.HasPrefix(e.Text(), "# AGENTS.md instructions") && e.Audience != transcript.AudienceModel {
			t.Errorf("AGENTS.md instructions have audience %d", e.Audience)
		}
	}
}

// The compacted row replaces the history with its replacement_history; the
// model's answer to the compaction request is not shown.
func TestCompaction(t *testing.T) {
	s := read(t, "testdata/home", compactID)
	// What codex sent the mock server on the fourth turn, and its answer.
	equal(t, "context", texts(s.Context()), []string{
		"user: Compact one: what is the codename?",
		"user: Compact two: again.",
		"user: " + summaryPrefix,
		"system: <skills_instructions>",
		"user: <environment_context>",
		"user: Compact three: after the limit.",
		"assistant: Hello from mock server.",
		"user: Compact four: after compaction.",
		"assistant: Hello from mock server.",
	})
	equal(t, "shown", texts(s.Linearize()), []string{
		"user: Compact one: what is the codename?",
		"assistant: Hello from mock server.",
		"user: Compact two: again.",
		"assistant: Hello from mock server.",
		"user: Compact three: after the limit.",
		"assistant: Hello from mock server.",
		"user: Compact four: after compaction.",
		"assistant: Hello from mock server.",
	})
	// Another agent's writer is given the summary with the turns after it.
	equal(t, "portable", texts(s.Portable().Entries), []string{
		"user: Compact one: what is the codename?",
		"user: Compact two: again.",
		"user: " + summaryPrefix,
		"user: Compact three: after the limit.",
		"assistant: Hello from mock server.",
		"user: Compact four: after compaction.",
		"assistant: Hello from mock server.",
	})
}

// A paginated rollout numbers every row; a written row continues the
// numbering, or Codex refuses to append to the file and its history
// projection skips the row.
func TestAppendOrdinals(t *testing.T) {
	s := read(t, "testdata/home", compactID)
	s.Entries = append(s.Entries,
		transcript.Entry{Role: transcript.RoleUser, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "Five."}}},
		transcript.Entry{Role: transcript.RoleAssistant, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "Noted."}}},
	)
	home := t.TempDir()
	st, _ := codex.Codec.Open(home)
	if _, err := st.Write(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	infos, err := st.List(context.Background(), "")
	if err != nil || len(infos) != 1 {
		t.Fatalf("list = %+v, %v", infos, err)
	}
	var ordinals []uint64
	eachRow(t, infos[0].Path, func(r row) {
		if r.Ordinal == nil {
			t.Fatalf("row %s has no ordinal", r.Type)
		}
		ordinals = append(ordinals, *r.Ordinal)
	})
	for i, o := range ordinals {
		if o != uint64(i) {
			t.Fatalf("ordinals %v, want 0..%d", ordinals, len(ordinals)-1)
		}
	}
}

// A session from another agent is written as a legacy rollout, which has no
// ordinals.
func TestForeignLegacy(t *testing.T) {
	home := t.TempDir()
	st, _ := codex.Codec.Open(home)
	s := &transcript.Session{Agent: "other", CWD: "/p", Entries: []transcript.Entry{
		{Role: transcript.RoleUser, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "hi"}}},
	}}
	id, err := st.Write(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	infos, _ := st.List(context.Background(), "")
	if len(infos) != 1 || infos[0].ID != id {
		t.Fatalf("list = %+v", infos)
	}
	eachRow(t, infos[0].Path, func(r row) {
		if r.Ordinal != nil {
			t.Errorf("legacy row %s has ordinal %d", r.Type, *r.Ordinal)
		}
		if r.Type != "session_meta" {
			return
		}
		// SessionMeta requires originator and cli_version
		// (protocol/src/protocol.rs); without them Codex reads the file as
		// not starting with session metadata.
		var m map[string]json.RawMessage
		if err := json.Unmarshal(r.Payload, &m); err != nil {
			t.Fatal(err)
		}
		for _, k := range []string{"id", "session_id", "timestamp", "cwd", "originator", "cli_version"} {
			if _, ok := m[k]; !ok {
				t.Errorf("session_meta has no %s: %s", k, r.Payload)
			}
		}
		if _, ok := m["history_mode"]; ok {
			t.Errorf("session_meta = %s", r.Payload)
		}
	})
}

// row is the envelope of a rollout line.
type row struct {
	Ordinal *uint64         `json:"ordinal"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// summaryPrefix opens the summary a compaction leaves
// (prompts/templates/compact/summary_prefix.md).
const summaryPrefix = "Another language model started to solve this problem and produced a summary of its thinking process. You also have access to the state of the tools that were used by that language model. Use this to build on the work that has already been done and avoid duplicating work. Here is the summary produced by the other language model, use the information in this summary to assist with your own analysis:"

func eachRow(t *testing.T, path string, fn func(row)) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var r row
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("row %q: %v", line, err)
		}
		fn(r)
	}
}

// A reverted thread resumes from its newest file, which continues a prefix
// of the one before it; the reverted turn is in neither.
func TestRevert(t *testing.T) {
	st, _ := codex.Codec.Open("testdata/home")
	infos, err := st.List(context.Background(), revertCWD)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, in := range infos {
		if in.ID == revertID {
			paths = append(paths, in.Path)
		}
	}
	if len(paths) != 1 || paths[0] != filepath.Join("testdata/home", strings.TrimPrefix(revertHead, "testdata/home/")) {
		t.Errorf("list gives the thread as %q, want its newest file only", paths)
	}
	s := read(t, "testdata/home", revertID)
	shown := []string{
		"user: Revert one: what is the codename?",
		"assistant: Hello from mock server.",
		"user: Revert two: again.",
		"assistant: Hello from mock server.",
		"user: Revert four: after the revert.",
		"assistant: Hello from mock server.",
	}
	equal(t, "shown", texts(s.Linearize()), shown)
	// What codex sent the mock server on the fourth turn, and its answer.
	equal(t, "context", texts(s.Context()), append([]string{
		"system: <skills_instructions>",
		"user: <environment_context>",
	}, shown...))

	// The fork continues the reverted thread's newest file, which continues
	// the first.
	s = read(t, "testdata/home", forkID)
	equal(t, "fork", texts(s.Linearize()), append(shown,
		"user: Fork one: in the fork.",
		"assistant: Hello from mock server.",
	))
}

// Writing a thread that spans files writes its newest file and leaves the
// files it inherits from as they are; into a home that lacks them, it
// writes the inherited prefixes, which Codex reads the same way.
func TestRevertWrite(t *testing.T) {
	ctx := context.Background()
	s := read(t, "testdata/home", forkID)
	want := texts(s.Linearize())
	home := t.TempDir()
	st, _ := codex.Codec.Open(home)
	if _, err := st.Write(ctx, s); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{forkHead, revertHead} {
		got, err := os.ReadFile(filepath.Join(home, strings.TrimPrefix(f, "testdata/home/")))
		if err != nil {
			t.Fatal(err)
		}
		orig, _ := os.ReadFile(f)
		if f == forkHead && string(got) != string(orig) {
			t.Errorf("%s changed", f)
		}
		// The fork inherits the revert's rows up to its history_base.
		if f == revertHead && !strings.HasPrefix(string(orig), string(got)) {
			t.Errorf("%s is not a prefix of the file it came from", f)
		}
	}
	back := read(t, home, forkID)
	equal(t, "written", texts(back.Linearize()), want)

	// An appended turn goes into the newest file only, numbered after it.
	home = t.TempDir()
	for _, f := range []string{revertBase, revertHead} {
		data, _ := os.ReadFile(f)
		p := filepath.Join(home, strings.TrimPrefix(f, "testdata/home/"))
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	st, _ = codex.Codec.Open(home)
	s, err := st.Read(ctx, revertID)
	if err != nil {
		t.Fatal(err)
	}
	s.Entries = append(s.Entries, transcript.Entry{Role: transcript.RoleUser, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "Five."}}})
	if _, err := st.Write(ctx, s); err != nil {
		t.Fatal(err)
	}
	base, _ := os.ReadFile(filepath.Join(home, strings.TrimPrefix(revertBase, "testdata/home/")))
	orig, _ := os.ReadFile(revertBase)
	if string(base) != string(orig) {
		t.Error("the write changed the file the thread inherits from")
	}
	var ordinals []uint64
	eachRow(t, filepath.Join(home, strings.TrimPrefix(revertHead, "testdata/home/")), func(r row) {
		if r.Ordinal == nil {
			t.Fatalf("row %s has no ordinal", r.Type)
		}
		ordinals = append(ordinals, *r.Ordinal)
	})
	// The head's session_meta is ordinal 26, the history_base's
	// end_ordinal_exclusive; its last row read is 39.
	if len(ordinals) < 2 || ordinals[0] != 26 || ordinals[len(ordinals)-1] != 40 || ordinals[len(ordinals)-2] != 39 {
		t.Errorf("head ordinals %v, want 26..40", ordinals)
	}
	back = read(t, home, revertID)
	if lin := back.Linearize(); lin[len(lin)-1].Text() != "Five." || len(lin) != 7 {
		t.Errorf("read back %q", texts(lin))
	}
}
