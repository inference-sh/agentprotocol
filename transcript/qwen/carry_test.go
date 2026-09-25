package qwen

import (
	"strings"
	"testing"

	"github.com/inference-sh/agentprotocol/transcript"
	"github.com/inference-sh/agentprotocol/transcript/claude"
	"github.com/inference-sh/agentprotocol/transcript/grok"
	"github.com/inference-sh/agentprotocol/transcript/pi"
)

// carry reads a session from another agent's store, writes it into Qwen
// and reads it back.
func carry(t *testing.T, from transcript.Codec, home, id string) *transcript.Session {
	t.Helper()
	src, err := from.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	s := read(t, src, id)
	s.Agent = "elsewhere"
	st, err := Codec.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	wrote, err := st.Write(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	return read(t, st, wrote)
}

// firstLines is lines with each text cut at its first newline.
func firstLines(es []transcript.Entry) []string {
	out := lines(es)
	for i, l := range out {
		out[i], _, _ = strings.Cut(l, "\n")
	}
	return out
}

func same(t *testing.T, what string, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("%s:\n  got  %q\n  want %q", what, got, want)
	}
}

// TestCarriedCompaction moves Claude's compacted sample (claude 2.1.281 in
// the harness-test container: a tool call, a second prompt, /compact
// keeping the last two turns) into Qwen. Qwen records the compaction as a
// chat_compression row: the turn it retired stays for the person, and on
// resume the model gets the summary with Qwen's trailer and its
// acknowledgement, then the turns Claude kept and the /compact exchange.
func TestCarriedCompaction(t *testing.T) {
	back := carry(t, claude.Codec, "../claude/testdata/home", "6deafb15-b5f3-476d-85d6-0d87ecca9ff4")
	const hello = "assistant: Hello from mock server."
	same(t, "linearize", firstLines(back.Linearize()), []string{
		"user: What is the project codename? Reply ONLY the codename.",
		"assistant: ",
		"tool: ",
		hello,
		"user: Tell me more about the project.",
		hello,
		"user: <command-name>/compact</command-name>",
		"user: <local-command-stdout>Compacted (ctrl+o to see full summary)",
	})
	ctx := back.Context()
	same(t, "context", firstLines(ctx), []string{
		"user: This session is being continued from a previous conversation that ran out of context. The summary below covers the earlier portion of the conversation.",
		"assistant: Got it. Thanks for the additional context!",
		hello,
		"user: Tell me more about the project.",
		hello,
		"user: <command-name>/compact</command-name>",
		"user: <local-command-stdout>Compacted (ctrl+o to see full summary)",
	})
	if !strings.HasSuffix(ctx[0].Text(), "\n\nResume the prior task using the summary above. Continue from the last in-flight step; do not acknowledge the summary, do not re-introduce, do not greet the user again.") {
		t.Errorf("summary = %q, want Qwen's trailer after it", ctx[0].Text())
	}
}

// TestCarriedKeepAndShownOnly moves pi's compacted rpc sample into Qwen. Its
// compaction keeps the answer before it, which follows the summary in the
// compressed history, in place of Qwen's acknowledgement. Its bash run kept
// out of context is shown and never sent, which a realtime_message row is.
func TestCarriedKeepAndShownOnly(t *testing.T) {
	back := carry(t, pi.Codec, "../pi/testdata/home", "01a0d2bd-7043-757c-80b2-3f24970d3f7c")
	const hello = "assistant: Hello from mock server."
	same(t, "linearize", firstLines(back.Linearize()), []string{
		"user: What is the project codename? Reply ONLY the codename.",
		hello,
		"user: Ran `ls`",
		"user: Ran `echo private`",
		"user: Second question.",
		hello,
		"user: Third question.",
		hello,
		"user: After compaction.",
		hello,
	})
	same(t, "context", firstLines(back.Context()), []string{
		"user: The conversation history before this point was compacted into the following summary:",
		hello,
		"user: After compaction.",
		hello,
	})
	// Moved on again, the turn the compaction kept goes with the summary:
	// it is conversation, not Qwen's context.
	var kept bool
	for _, e := range back.Portable().Entries {
		if c := e.Compaction; c != nil {
			for _, m := range c.Summary {
				kept = kept || m.Text() == "Hello from mock server."
			}
		}
	}
	if !kept {
		t.Error("the kept turn does not travel on with the compaction")
	}
}

// TestCarriedRetiredToolCall moves grok's resumed compacted sample (a
// session grok resumed in the harness-test container) into Qwen. grok shows
// the history its compaction retired and no longer sends it, a tool call
// and its result among it. Qwen keeps that history in the rows before its
// chat_compression, the call and result included, and the model gets
// neither.
func TestCarriedRetiredToolCall(t *testing.T) {
	back := carry(t, grok.Codec, "../grok/testdata/home", "69722182-94aa-4598-a445-42cdcbf44bd9")
	const hello = "assistant: Hello from mock server."
	same(t, "linearize", firstLines(back.Linearize()), []string{
		"user: What is the project codename? Reply ONLY the codename.",
		"assistant: ",
		"tool: ",
		hello,
		"user: What files are in this repository?",
		hello,
		"user: Summarise what you have done so far.",
		hello,
		"user: What was the first thing I asked you?",
		hello,
		"user: /compress",
		"user: What was my previous question?",
		hello,
		"user: What was my previous question?",
		hello,
	})
	for _, e := range back.Context() {
		for _, b := range e.Content {
			if b.Kind == transcript.BlockToolUse || b.Kind == transcript.BlockToolResult {
				t.Errorf("the retired %s reaches the model", b.Kind)
			}
		}
	}
}
