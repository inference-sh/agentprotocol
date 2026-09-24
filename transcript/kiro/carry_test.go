package kiro

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/inference-sh/agentprotocol/transcript"
	"github.com/inference-sh/agentprotocol/transcript/pi"
	"github.com/inference-sh/agentprotocol/transcript/qwen"
)

// A compacted session another agent wrote, from that agent's testdata (a
// container capture), moved into kiro.
var carrySources = []struct {
	name        string
	codec       transcript.Codec
	home, id    string
	summary     string   // a piece of the compaction's summary
	retired     []string // prompts the compaction retired
	shownOnly   string   // a prompt the source showed and never sent
	kept, later []string // what the model is given after the summary
}{{
	// qwen's /compress keeps nothing after its summary.
	name: "qwen", codec: qwen.Codec, home: "../qwen/testdata/home", id: "2d4caa81-62a5-40d0-bdd6-82814ff62697",
	summary: "Resume the prior task using the summary above.",
	retired: []string{"What is the project codename? Reply ONLY the codename.", "What files are in this repository?", "Summarise what you have done so far.", "What was the first thing I asked you?"},
	later:   []string{"user: What was my previous question?", "assistant: Hello from mock server."},
}, {
	// pi's compaction keeps from its last answer (firstKeptEntryId); the
	// session also holds a shell command pi shows and does not send.
	name: "pi", codec: pi.Codec, home: "../pi/testdata/home", id: "01a0d2bd-7043-757c-80b2-3f24970d3f7c",
	// kiro keeps whole turns, so the answer pi kept brings the prompt
	// it answers along.
	summary:   "compacted into the following summary",
	retired:   []string{"What is the project codename? Reply ONLY the codename.", "Second question."},
	shownOnly: "Ran `echo private`\n```\nprivate\n\n```",
	kept:      []string{"user: Third question.", "assistant: Hello from mock server."},
	later:     []string{"user: After compaction.", "assistant: Hello from mock server."},
}}

// TestCarriesCompaction writes another agent's compacted session and reads
// it back: session/load replays the retired turns, and the model is given
// the summary in kiro's context entry, then the turns the Compaction row
// kept and those after.
func TestCarriesCompaction(t *testing.T) {
	for _, src := range carrySources {
		t.Run(src.name, func(t *testing.T) {
			st, err := src.codec.Open(src.home)
			if err != nil {
				t.Fatal(err)
			}
			s, err := st.Read(t.Context(), src.id)
			if err != nil {
				t.Fatal(err)
			}
			s.Agent, s.ID = "elsewhere", ""
			dst, err := Codec.Open(filepath.Join(t.TempDir(), "home"))
			if err != nil {
				t.Fatal(err)
			}
			id, err := dst.Write(t.Context(), s)
			if err != nil {
				t.Fatal(err)
			}
			got, err := dst.Read(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}

			shown := lines(got.Linearize())
			for _, r := range src.retired {
				if !slices.Contains(shown, "user: "+r) {
					t.Errorf("retired prompt %q is not shown; shown %q", r, shown)
				}
			}
			ctx := lines(got.Context())
			// The shown-only prompt is before the compaction, so it
			// travels as history the compaction retired: shown, not sent.
			if src.shownOnly != "" && (!slices.Contains(shown, "user: "+src.shownOnly) || slices.Contains(ctx, "user: "+src.shownOnly)) {
				t.Errorf("shown-only prompt: shown %q, sent %q", shown, ctx)
			}
			if len(ctx) == 0 || !strings.HasPrefix(ctx[0], "user: --- CONTEXT ENTRY BEGIN ---") || !strings.Contains(ctx[0], src.summary) {
				t.Fatalf("context starts %q", ctx)
			}
			if want := append(slices.Clone(src.kept), src.later...); !slices.Equal(ctx[1:], want) {
				t.Errorf("after the summary:\n  %q\nwant\n  %q", ctx[1:], want)
			}
		})
	}
}

// TestCompactionKeepsInPlace: the turns kiro's Compaction row copies are
// the rows before it, so the compaction keeps them and retires the rest,
// and another agent is given them as kept history.
func TestCompactionKeepsInPlace(t *testing.T) {
	s := read(t, compactID)
	var c *transcript.Compaction
	for _, e := range s.Entries {
		if e.Compaction != nil {
			c = e.Compaction
		}
	}
	if c == nil || c.Keep != "1cda7af0-e381-4a7c-8cff-00565c2a6d2e" || len(c.Summary) != 1 {
		t.Fatalf("compaction = %+v", c)
	}
	p := s.Portable()
	at := slices.IndexFunc(p.Entries, func(e transcript.Entry) bool { return e.Compaction != nil })
	if k := p.Entries[at].Compaction.Keep; k != "1cda7af0-e381-4a7c-8cff-00565c2a6d2e" {
		t.Errorf("portable keeps from %q", k)
	}
}
