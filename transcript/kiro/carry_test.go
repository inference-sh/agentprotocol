package kiro

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/inference-sh/agentprotocol/transcript"
	"github.com/inference-sh/agentprotocol/transcript/claude"
	"github.com/inference-sh/agentprotocol/transcript/copilot"
	"github.com/inference-sh/agentprotocol/transcript/gemini"
	"github.com/inference-sh/agentprotocol/transcript/kimi"
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
	wrapper     string   // a piece of the source agent's own wrapping of it
	retired     []string // prompts the compaction retired
	shownOnly   string   // a prompt the source showed and never sent
	kept, later []string // what the model is given after the summary
}{{
	// qwen's /compress keeps nothing after its summary.
	name: "qwen", codec: qwen.Codec, home: "../qwen/testdata/home", id: "2d4caa81-62a5-40d0-bdd6-82814ff62697",
	summary: "SUMMARY CONTENT:\nHello from mock server.\n--- CONTEXT ENTRY END ---",
	wrapper: "Resume the prior task using the summary above.",
	retired: []string{"What is the project codename? Reply ONLY the codename.", "What files are in this repository?", "Summarise what you have done so far.", "What was the first thing I asked you?"},
	later:   []string{"user: What was my previous question?", "assistant: Hello from mock server."},
}, {
	// pi's compaction keeps from its last answer (firstKeptEntryId); the
	// session also holds a shell command pi shows and does not send.
	name: "pi", codec: pi.Codec, home: "../pi/testdata/home", id: "01a0d2bd-7043-757c-80b2-3f24970d3f7c",
	// kiro keeps whole turns, so the answer pi kept brings the prompt
	// it answers along.
	summary:   "SUMMARY CONTENT:\nHello from mock server.\n\n---",
	wrapper:   "compacted into the following summary",
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
			// The source's own wrapping stays with the source.
			if strings.Contains(ctx[0], src.wrapper) {
				t.Errorf("context entry holds the source's wrapping %q", src.wrapper)
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

// TestAlternates moves sessions whose messages do not alternate into kiro:
// gemini's (two answers in a row), copilot's (the same, then a compaction),
// pi's (a prompt and two bash runs in a row, then a compaction) and kimi's
// (two prompts in a row). kiro fails the first request of a history holding
// two messages in a row from the same side, so each run of them is written
// as one message, and none of their text is lost.
func TestAlternates(t *testing.T) {
	for _, src := range []struct {
		name     string
		codec    transcript.Codec
		home, id string
	}{
		{"gemini", gemini.Codec, "../gemini/testdata/home", "170f5754-4e70-4ac2-8bd8-22e062f69964"},
		{"copilot", copilot.Codec, "../copilot/testdata/home", "92371bbf-c936-4cea-ae4b-32090b3e29f8"},
		{"pi", pi.Codec, "../pi/testdata/home", "01a0d2bd-7043-757c-80b2-3f24970d3f7c"},
		{"kimi", kimi.Codec, "../kimi/testdata/home", "session_9ef8feda-5261-4097-892f-94cf87e40a76"},
	} {
		t.Run(src.name, func(t *testing.T) {
			st, err := src.codec.Open(src.home)
			if err != nil {
				t.Fatal(err)
			}
			s, err := st.Read(t.Context(), src.id)
			if err != nil {
				t.Fatal(err)
			}
			var texts []string
			for _, e := range s.Portable().Lower(files.Caps).Entries {
				for _, b := range e.Content {
					if b.Kind == transcript.BlockText && b.Text != "" {
						texts = append(texts, b.Text)
					}
				}
			}
			s.ID = ""
			home := t.TempDir()
			dst, err := Codec.Open(home)
			if err != nil {
				t.Fatal(err)
			}
			id, err := dst.Write(t.Context(), s)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(filepath.Join(home, ".kiro", "sessions", "cli", id+".jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			side := map[string]string{"Prompt": "user", "ToolResults": "user", "AssistantMessage": "assistant", "user": "user", "assistant": "assistant"}
			alternating := func(what string, kinds []string) {
				for i := 1; i < len(kinds); i++ {
					if side[kinds[i]] == side[kinds[i-1]] {
						t.Errorf("%s: %s after %s: %v", what, kinds[i], kinds[i-1], kinds)
						return
					}
				}
			}
			var run []string
			for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
				var r struct {
					Kind string `json:"kind"`
					Data struct {
						MessagesSnapshot []struct {
							Role string `json:"role"`
						} `json:"messages_snapshot"`
					} `json:"data"`
				}
				if err := json.Unmarshal([]byte(line), &r); err != nil {
					t.Fatal(err)
				}
				switch r.Kind {
				case "Prompt", "ToolResults", "AssistantMessage":
					run = append(run, r.Kind)
				case "Compaction", "Clear":
					alternating("rows", run)
					run = nil
					for _, m := range r.Data.MessagesSnapshot {
						run = append(run, m.Role)
					}
					alternating("snapshot", run)
				}
			}
			alternating("rows", run)
			for _, text := range texts {
				quoted, _ := json.Marshal(text)
				if !strings.Contains(string(raw), string(quoted)) {
					t.Errorf("text lost: %.60q", text)
				}
			}
		})
	}
}

// TestTrailingPrompt moves Claude's compacted sample, which ends on the
// /compact exchange Claude records as user messages, into kiro. kiro fails
// the next request of a history ending on a user message, so the prompt is
// followed by a CancelledPrompt row, kiro's own for a prompt it got no answer
// to: the prompt stays in the session for the person and is not given to
// the model.
func TestTrailingPrompt(t *testing.T) {
	src, err := claude.Codec.Open("../claude/testdata/home")
	if err != nil {
		t.Fatal(err)
	}
	s, err := src.Read(t.Context(), "6deafb15-b5f3-476d-85d6-0d87ecca9ff4")
	if err != nil {
		t.Fatal(err)
	}
	s.ID = ""
	home := t.TempDir()
	dst, err := Codec.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	id, err := dst.Write(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(home, ".kiro", "sessions", "cli", id+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	rows := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if n := len(rows); n < 2 || rows[n-1] != `{"version":"v1","kind":"CancelledPrompt"}` || !strings.Contains(rows[n-2], `"kind":"Prompt"`) {
		t.Fatalf("last rows = %.200q", rows[max(0, len(rows)-2):])
	}
	back, err := dst.Read(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	ctx, lin := back.Context(), back.Linearize()
	if last := ctx[len(ctx)-1]; last.Role != transcript.RoleAssistant {
		t.Errorf("the model is given %s %.60q last", last.Role, last.Text())
	}
	if last := lin[len(lin)-1]; last.Role != transcript.RoleUser || !strings.Contains(last.Text(), "/compact") {
		t.Errorf("the person is shown %s %.60q last", last.Role, last.Text())
	}
}

// TestCompactionOpensWithPrompt moves Claude's compacted sample into kiro.
// Claude's compaction keeps nothing, and its model's answer comes right
// after the boundary; a kiro history opening with that answer fails its
// first request. The Compaction row keeps the answer's turn from its prompt
// on, so the model is given the summary, that prompt, its tool call and
// result, and the answer, and nothing is lost or repeated.
func TestCompactionOpensWithPrompt(t *testing.T) {
	src, err := claude.Codec.Open("../claude/testdata/home")
	if err != nil {
		t.Fatal(err)
	}
	s, err := src.Read(t.Context(), "6deafb15-b5f3-476d-85d6-0d87ecca9ff4")
	if err != nil {
		t.Fatal(err)
	}
	s.ID = ""
	dst, err := Codec.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id, err := dst.Write(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	back, err := dst.Read(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range back.Context() {
		line := string(e.Role) + ": " + e.Text()
		for _, b := range e.Content {
			if b.Kind == transcript.BlockToolUse || b.Kind == transcript.BlockToolResult {
				line += string(b.Kind)
			}
		}
		if strings.HasPrefix(e.Text(), summaryPrefix) {
			line = "summary"
		}
		got = append(got, line)
	}
	want := []string{
		"summary",
		"user: What is the project codename? Reply ONLY the codename.",
		"assistant: tool_use",
		"tool: tool_result",
		"assistant: Hello from mock server.",
		"user: Tell me more about the project.",
		"assistant: Hello from mock server.",
	}
	if !slices.Equal(got, want) {
		t.Errorf("context =\n  %q\nwant\n  %q", got, want)
	}
	// kiro's context entry wraps the summary once, and Claude's own
	// wrapping stays with Claude.
	if sum := back.Context()[0].Text(); strings.Count(sum, summaryPrefix) != 1 || strings.Contains(sum, "This session is being continued") {
		t.Errorf("summary = %q, want it inside kiro's context entry alone", sum)
	}
	for _, e := range back.Linearize() {
		if strings.HasPrefix(e.Text(), summaryPrefix) {
			t.Error("the summary is shown as a message")
		}
	}
}

// TestTrailingToolResult moves a session ending on a tool result into kiro:
// kiro's own tool-call capture e53ccf60 with its last answer cut, as a
// session another agent stopped mid-turn holds it. kiro refuses a history
// ending on a user message, so the result is followed by a CancelledPrompt
// row: the session loads, and the result is shown and not given to the
// model.
func TestTrailingToolResult(t *testing.T) {
	src, err := Codec.Open("testdata/home")
	if err != nil {
		t.Fatal(err)
	}
	s, err := src.Read(t.Context(), "e53ccf60-b53a-4fdd-a00f-05547225a99d")
	if err != nil {
		t.Fatal(err)
	}
	last := len(s.Entries) - 1
	for s.Entries[last].Role != transcript.RoleAssistant {
		last--
	}
	s.Entries = s.Entries[:last]
	s.Agent, s.ID, s.Vendor = "elsewhere", "", nil
	home := t.TempDir()
	dst, err := Codec.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	id, err := dst.Write(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(home, ".kiro", "sessions", "cli", id+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	rows := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if n := len(rows); n < 2 || rows[n-1] != `{"version":"v1","kind":"CancelledPrompt"}` || !strings.Contains(rows[n-2], `"kind":"ToolResults"`) {
		t.Fatalf("last rows = %.200q", rows[max(0, len(rows)-2):])
	}
	back, err := dst.Read(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	ctx, lin := back.Context(), back.Linearize()
	if e := ctx[len(ctx)-1]; e.Role != transcript.RoleAssistant {
		t.Errorf("the model is given %s last", e.Role)
	}
	if e := lin[len(lin)-1]; e.Role != transcript.RoleTool {
		t.Errorf("the person is shown %s last, want the tool result", e.Role)
	}
}
