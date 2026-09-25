package copilot

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
// container capture), moved into Copilot.
var carrySources = []struct {
	name        string
	codec       transcript.Codec
	home, id    string
	summary     string   // a piece of the compaction's summary
	wrapper     string   // a piece of the source agent's own wrapping of it
	retired     []string // prompts the compaction retired
	shownOnly   string   // a prompt the source showed and never sent
	kept, later []string // what the model is given after the summary
}{{
	// qwen's /compress keeps nothing after its summary.
	name: "qwen", codec: qwen.Codec, home: "../qwen/testdata/home", id: "2d4caa81-62a5-40d0-bdd6-82814ff62697",
	summary: "<summary>\nHello from mock server.\n</summary>",
	wrapper: "Resume the prior task using the summary above.",
	retired: []string{"What is the project codename? Reply ONLY the codename.", "What files are in this repository?", "Summarise what you have done so far.", "What was the first thing I asked you?"},
	later:   []string{"user: What was my previous question?", "assistant: Hello from mock server."},
}, {
	// pi's compaction keeps from its last answer (firstKeptEntryId); the
	// session also holds a shell command pi shows and does not send.
	name: "pi", codec: pi.Codec, home: "../pi/testdata/home", id: "01a0d2bd-7043-757c-80b2-3f24970d3f7c",
	summary:   "<summary>\nHello from mock server.\n\n---",
	wrapper:   "compacted into the following summary",
	retired:   []string{"What is the project codename? Reply ONLY the codename.", "Second question.", "Third question."},
	shownOnly: "Ran `echo private`\n```\nprivate\n\n```",
	kept:      []string{"assistant: Hello from mock server."},
	later:     []string{"user: After compaction.", "assistant: Hello from mock server."},
}}

// TestCarriesCompaction writes another agent's compacted session and reads
// it back: session/load replays the retired turns, and the model is given
// Copilot's resume message holding the summary, then the kept turns and
// those after.
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
			home := filepath.Join(t.TempDir(), "home")
			w, err := Writer.Open(home)
			if err != nil {
				t.Fatal(err)
			}
			id, err := w.Write(t.Context(), s)
			if err != nil {
				t.Fatal(err)
			}
			got := read(t, home, id)

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
			if len(ctx) == 0 || !strings.HasPrefix(ctx[0], "user: Some of the conversation history has been summarized") || !strings.Contains(ctx[0], src.summary) {
				t.Fatalf("context starts %q", ctx)
			}
			// The source's own wrapping stays with the source.
			if strings.Contains(ctx[0], src.wrapper) {
				t.Errorf("resume message holds the source's wrapping %q", src.wrapper)
			}
			// The resume message lists the prompts before the compaction:
			// the retired ones, not the kept ones.
			for _, r := range src.retired {
				if !strings.Contains(ctx[0], "<user_message>\n"+r+"\n</user_message>") {
					t.Errorf("resume message does not list %q", r)
				}
			}
			if want := append(slices.Clone(src.kept), src.later...); !slices.Equal(ctx[1:], want) {
				t.Errorf("after the summary:\n  %q\nwant\n  %q", ctx[1:], want)
			}
		})
	}
}
