package pi

import (
	"strings"
	"testing"

	"github.com/inference-sh/agentprotocol/transcript"
	"github.com/inference-sh/agentprotocol/transcript/claude"
	"github.com/inference-sh/agentprotocol/transcript/transcripttest"
)

// carry writes a session read from another agent into pi and reads it back.
func carry(t *testing.T, s *transcript.Session) *transcript.Session {
	t.Helper()
	s.Agent = "elsewhere"
	st, err := Codec.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.Write(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	back, err := st.Read(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return back
}

// TestCarriedCompaction moves Claude's compacted sample (claude 2.1.281 in
// the harness-test container: a tool call, a second prompt, /compact
// keeping the last two turns) into pi. pi records the compaction as its own
// compaction entry: the turn it retired stays on the branch for the person,
// and on resume the model gets the summary as pi wraps it, then the turns
// Claude kept and the /compact exchange after them.
func TestCarriedCompaction(t *testing.T) {
	s := read(t, claude.Codec, transcripttest.Sample{Home: "../claude/testdata/home", ID: "6deafb15-b5f3-476d-85d6-0d87ecca9ff4"})
	back := carry(t, s)
	const hello = "assistant: Hello from mock server."
	equal(t, "linearize", summary(back.Linearize()), []string{
		"user: What is the project codename? Reply ONLY",
		"assistant: ",
		"tool: 1\ttest",
		"user: The conversation history before this poi",
		hello,
		"user: Tell me more about the project.",
		hello,
		"user: <command-name>/compact</command-name>\n  ",
		"user: <local-command-stdout>Compacted (ctrl+o ",
	})
	ctx := back.Context()
	equal(t, "context", summary(ctx), []string{
		"user: The conversation history before this poi",
		hello,
		"user: Tell me more about the project.",
		hello,
		"user: <command-name>/compact</command-name>\n  ",
		"user: <local-command-stdout>Compacted (ctrl+o ",
	})
	if !strings.Contains(ctx[0].Text(), "<summary>\nThis session is being continued") {
		t.Errorf("summary = %q, want Claude's summary wrapped as pi wraps one", ctx[0].Text())
	}
}

// TestCarriedCompactionKeep moves omp's compacted rpc sample, whose
// compaction keeps the answer before it, into pi: the entry kept is named
// by the id pi gives it, so the model still gets it after the summary.
func TestCarriedCompactionKeep(t *testing.T) {
	back := carry(t, read(t, OMP, ompCompacted))
	const hello = "assistant: Hello from mock server."
	equal(t, "linearize", summary(back.Linearize()), []string{
		"user: What is the project codename? Reply ONLY",
		hello,
		"user: Ran `ls`\n```\nnotes.md\n\n```",
		"user: Summarize @notes.md please.",
		hello,
		"user: Third question.",
		hello,
		"user: The conversation history before this poi",
		"user: After compaction.",
		hello,
	})
	equal(t, "context", summary(back.Context()), []string{
		"user: The conversation history before this poi",
		hello,
		"user: After compaction.",
		hello,
	})
}
