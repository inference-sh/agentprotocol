package claude

import (
	"strings"
	"testing"

	"github.com/inference-sh/agentprotocol/transcript"
	"github.com/inference-sh/agentprotocol/transcript/pi"
)

// lines renders entries as role: text, with the first line of the text.
func lines(es []transcript.Entry) []string {
	out := make([]string, len(es))
	for i, e := range es {
		text, _, _ := strings.Cut(e.Text(), "\n")
		out[i] = string(e.Role) + ": " + text
	}
	return out
}

func same(t *testing.T, what string, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("%s:\n  got  %q\n  want %q", what, got, want)
	}
}

// TestCarriedCompaction moves pi's compacted rpc sample (pi 0.x in the
// harness-test container: three turns, /compact keeping the last answer, one
// more turn) into Claude. Claude records the compaction as its own: the
// history before it stays in the file for the person, and on resume the
// model gets the summary as Claude wraps one, the answer pi kept, and the
// turn after.
func TestCarriedCompaction(t *testing.T) {
	src, err := pi.Codec.Open("../pi/testdata/home")
	if err != nil {
		t.Fatal(err)
	}
	s, err := src.Read(t.Context(), "01a0d2bd-7043-757c-80b2-3f24970d3f7c")
	if err != nil {
		t.Fatal(err)
	}
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
	const hello = "assistant: Hello from mock server."
	same(t, "linearize", lines(back.Linearize()), []string{
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
	ctx := back.Context()
	same(t, "context", lines(ctx), []string{
		"user: This session is being continued from a previous conversation that ran out of context. The summary below covers the earlier portion of the conversation.",
		hello,
		"user: After compaction.",
		hello,
	})
	// pi's own wrapping stays with pi: the model gets the summary inside
	// Claude's wrapping alone.
	if sum := ctx[0].Text(); strings.Contains(sum, "<summary>") || !strings.HasSuffix(sum, "Hello from mock server.\n\nRecent messages are preserved verbatim.") {
		t.Errorf("summary = %q, want pi's summary wrapped as Claude wraps one, once", sum)
	}
}
