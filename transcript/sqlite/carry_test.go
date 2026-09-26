package sqlite

import (
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/inference-sh/agentprotocol/transcript"
	"github.com/inference-sh/agentprotocol/transcript/pi"
	"github.com/inference-sh/agentprotocol/transcript/qwen"
)

// A carried session is a compacted session another agent wrote, moved into
// one of the stores here. Each source is a container capture from that
// agent's testdata.
type carrySource struct {
	name    string
	read    func(t *testing.T) *transcript.Session
	summary string // a piece of the compaction's summary
	// wrapper is a piece of the source agent's own wrapping of the
	// summary, which stays with the source.
	wrapper string
	retired []string // prompts the compaction retired
	// shownOnly is a prompt the source showed and never sent, which a
	// target that stores such prompts must show and not send.
	shownOnly string
	// kept is what the model is given after the summary, as role: text
	// lines of the messages with text: the turns the compaction kept, then
	// the ones after it.
	kept []string
}

var carrySources = []carrySource{{
	// qwen's /compress keeps nothing after its summary.
	name: "qwen",
	read: func(t *testing.T) *transcript.Session {
		st, err := qwen.Codec.Open("../qwen/testdata/home")
		if err != nil {
			t.Fatal(err)
		}
		s, err := st.Read(t.Context(), "2d4caa81-62a5-40d0-bdd6-82814ff62697")
		if err != nil {
			t.Fatal(err)
		}
		return s
	},
	summary: "Hello from mock server.",
	wrapper: "Resume the prior task using the summary above.",
	retired: []string{"What is the project codename? Reply ONLY the codename.", "What files are in this repository?", "Summarise what you have done so far.", "What was the first thing I asked you?"},
	kept:    []string{"user: What was my previous question?", "assistant: Hello from mock server."},
}, {
	// pi's compaction keeps from its last answer (firstKeptEntryId), and
	// the session holds a shell command run with !!, which pi shows and
	// leaves out of the context.
	name: "pi",
	read: func(t *testing.T) *transcript.Session {
		st, err := pi.Codec.Open("../pi/testdata/home")
		if err != nil {
			t.Fatal(err)
		}
		s, err := st.Read(t.Context(), "01a0d2bd-7043-757c-80b2-3f24970d3f7c")
		if err != nil {
			t.Fatal(err)
		}
		return s
	},
	summary:   "Hello from mock server.\n\n---\n\n**Turn Context (split turn):**",
	wrapper:   "compacted into the following summary",
	retired:   []string{"What is the project codename? Reply ONLY the codename.", "Second question.", "Third question."},
	shownOnly: "Ran `echo private`\n```\nprivate\n\n```",
	kept:      []string{"assistant: Hello from mock server.", "user: After compaction.", "assistant: Hello from mock server."},
}, {
	// opencode's compaction keeps its last turn (tail_start_id).
	name: "opencode",
	read: func(t *testing.T) *transcript.Session {
		c := compactSamples[0]
		return readSample(t, c.codec, c.home, c.id)
	},
	summary: "The codename is BLUEBIRD.",
	retired: []string{"What is the project codename?", "Say it again."},
	kept: []string{
		"user: And once more.", "assistant: The codename is BLUEBIRD.",
		"assistant: The codename is BLUEBIRD.",
		"user: After compaction: codename?", "assistant: The codename is BLUEBIRD.",
	},
}}

// carry reads src as another agent's session and writes it into codec under
// a fresh home, then reads it back.
func carry(t *testing.T, codec transcript.Codec, src carrySource) *transcript.Session {
	t.Helper()
	s := src.read(t)
	s.Agent, s.ID = "elsewhere", ""
	st := mustOpen(t, codec, filepath.Join(t.TempDir(), "home"))
	id, err := st.Write(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.Read(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// checkCarried checks a carried session read back: the person is shown the
// retired prompts; the model is given the summary, then the kept turns and
// those after, and no retired prompt. own reports a message the target
// agent adds around a summary of its own accord.
func checkCarried(t *testing.T, got *transcript.Session, src carrySource, userOnly bool, own func(transcript.Entry) bool) {
	t.Helper()
	var shown []string
	for _, e := range got.Linearize() {
		shown = append(shown, e.Text())
	}
	if src.shownOnly != "" && slices.Contains(shown, src.shownOnly) != userOnly {
		t.Errorf("shown-only prompt shown: %v, want %v; shown %q", !userOnly, userOnly, shown)
	}
	for _, r := range src.retired {
		if !slices.Contains(shown, r) {
			t.Errorf("retired prompt %q is not shown; shown %q", r, shown)
		}
	}
	ctx := got.Context()
	at := slices.IndexFunc(ctx, func(e transcript.Entry) bool { return strings.Contains(e.Text(), src.summary) })
	if at < 0 {
		t.Fatalf("no summary in the context %q", said(ctx))
	}
	if src.wrapper != "" {
		for _, e := range ctx {
			if strings.Contains(e.Text(), src.wrapper) {
				t.Errorf("the model is given the source's wrapping %q in %q", src.wrapper, e.Text())
			}
		}
	}
	var rest []string
	for i, e := range ctx {
		if e.Role == transcript.RoleUser && (slices.Contains(src.retired, e.Text()) || src.shownOnly != "" && e.Text() == src.shownOnly) {
			t.Errorf("prompt %q reaches the model", e.Text())
		}
		if i > at && e.Text() != "" && !own(e) {
			rest = append(rest, string(e.Role)+": "+e.Text())
		}
	}
	sameLines(t, "after the summary", rest, src.kept)
}

func TestOpencodeCarriesCompaction(t *testing.T) {
	for _, target := range []struct {
		name  string
		codec transcript.Codec
	}{{"opencode", Opencode}, {"kilo", Kilo}} {
		for _, src := range carrySources {
			t.Run(target.name+"/"+src.name, func(t *testing.T) {
				got := carry(t, target.codec, src)
				checkCarried(t, got, src, true, func(e transcript.Entry) bool { return e.Text() == "What did we do so far?" })
			})
		}
	}
}

func TestGooseCarriesCompaction(t *testing.T) {
	for _, src := range carrySources {
		t.Run(src.name, func(t *testing.T) {
			got := carry(t, Goose, src)
			checkCarried(t, got, src, true, func(e transcript.Entry) bool { return strings.HasPrefix(e.Text(), "Your context was compacted") })
		})
	}
}

func TestHermesCarriesCompaction(t *testing.T) {
	for _, src := range carrySources {
		t.Run(src.name, func(t *testing.T) {
			got := carry(t, Hermes, src)
			checkCarried(t, got, src, true, func(transcript.Entry) bool { return false })
			if ctx := got.Context(); !strings.HasPrefix(ctx[0].Text(), "[CONTEXT COMPACTION — REFERENCE ONLY]") {
				t.Errorf("the model is given the summary as %q", ctx[0].Text())
			}
		})
	}
}

// A hermes from before the active and compacted columns sends every row it
// has, so a carried compaction reaches it applied: the summary in place of
// the history it retired.
func TestHermesOlderSchemaCarriesSummary(t *testing.T) {
	ddl, err := os.ReadFile("testdata/hermes-old/schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	path := filepath.Join(home, hermesPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+url.PathEscape(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(ddl)); err != nil {
		t.Fatal(err)
	}
	db.Close()
	src := carrySources[0]
	s := src.read(t)
	s.Agent, s.ID = "elsewhere", ""
	st := mustOpen(t, Hermes, home)
	id, err := st.Write(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.Read(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	ctx := said(got.Context())
	if len(ctx) == 0 || !strings.Contains(got.Context()[0].Text(), src.summary) || strings.Contains(got.Context()[0].Text(), src.wrapper) {
		t.Fatalf("context = %q", ctx)
	}
	sameLines(t, "after the summary", ctx[1:], src.kept)
}

// Two sessions opening with the same prompt get distinct titles, numbered
// the way hermes numbers a repeat; the second write failed on hermes'
// unique title index.
func TestHermesRepeatedTitle(t *testing.T) {
	st := mustOpen(t, Hermes, t.TempDir())
	var titles []string
	for range 3 {
		s := &transcript.Session{CWD: "/work", Entries: []transcript.Entry{
			{Role: transcript.RoleUser, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "same prompt"}}},
		}}
		id, err := st.Write(t.Context(), s)
		if err != nil {
			t.Fatal(err)
		}
		got, err := st.Read(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		titles = append(titles, got.Title)
	}
	sameLines(t, "titles", titles, []string{"same prompt", "same prompt #2", "same prompt #3"})
}
