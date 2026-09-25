package codex_test

import (
	"strings"
	"testing"

	"github.com/inference-sh/agentprotocol/transcript/codex"
	"github.com/inference-sh/agentprotocol/transcript/pi"
)

// TestCarriedCompaction moves pi's compacted rpc sample (a harness-test
// container run: three turns, /compact keeping the last answer, one more
// turn) into Codex. Codex records the compaction as its own compacted row:
// the rows before it stay for the person, and on resume the model is given
// its replacement history, the summary behind Codex's preamble and the
// answer pi kept, then the turn after.
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
	st, err := codex.Codec.Open(t.TempDir())
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
	equal(t, "linearize", texts(back.Linearize()), []string{
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
	equal(t, "context", texts(ctx), []string{
		"user: " + summaryPrefix,
		hello,
		"user: After compaction.",
		hello,
	})
	if !strings.Contains(ctx[0].Text(), "<summary>") {
		t.Errorf("summary = %q, want pi's summary", ctx[0].Text())
	}
}

// TestCarriedAnswerBeforeCompaction moves Codex's own compacted sample (a
// harness-test container run whose mock model answers every request, the
// compaction's included, with the same text) back in as another agent's
// session. The answer right before the compaction reads the same as the
// summary, and is still shown: it answers a prompt, which the compaction's
// own answer never does.
func TestCarriedAnswerBeforeCompaction(t *testing.T) {
	src, err := codex.Codec.Open("testdata/home")
	if err != nil {
		t.Fatal(err)
	}
	s, err := src.Read(t.Context(), "01a0d2bd-6231-7420-8e6e-f0e9d20b29cd")
	if err != nil {
		t.Fatal(err)
	}
	want := texts(s.Linearize())
	s.Agent, s.ID = "elsewhere", ""
	st, err := codex.Codec.Open(t.TempDir())
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
	equal(t, "linearize", texts(back.Linearize()), want)
}
