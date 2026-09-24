package sqlite

import (
	"strings"
	"testing"

	"github.com/inference-sh/agentprotocol/transcript"
)

// The kiro v1 compaction sample is headless runs of kiro-cli 2.24 (chat
// --no-interactive --agent-engine v1 --resume) in the harness-test
// container against the mock model: three turns, /compact, which wrote the
// conversation again under a new id, and two more turns there.
const (
	kiroV1CompactedID = "833db471-0a1f-4d61-a77d-5a1e7a029070"
	kiroV1BeforeID    = "b1cef8f5-9915-4c8f-ae64-79c9557e7f14"
)

func kiroTexts(es []transcript.Entry) string {
	var out []string
	for _, e := range es {
		t := e.Text()
		if i := strings.Index(t, "SUMMARY CONTENT:\n"); i >= 0 {
			t = "summary: " + strings.TrimSuffix(t[i+len("SUMMARY CONTENT:\n"):], "\n--- CONTEXT ENTRY END ---")
		}
		out = append(out, string(e.Role)+": "+t)
	}
	return strings.Join(out, " | ")
}

// TestKiroV1Compacted: the model is given the compaction's summary, then the
// history the compacted conversation kept, as kiro sent them when the
// conversation went on.
func TestKiroV1Compacted(t *testing.T) {
	st, err := Kiro.Open("testdata/kiro-compact")
	if err != nil {
		t.Fatal(err)
	}
	s, err := st.Read(t.Context(), kiroV1CompactedID)
	if err != nil {
		t.Fatal(err)
	}
	const history = "user: first question alpha | assistant: ANSWER-ONE | user: second question beta | assistant: ANSWER-TWO | user: third question gamma | assistant: ANSWER-THREE | user: fourth question delta | assistant: ANSWER-FOUR | user: sixth question zeta | assistant: ANSWER-SIX"
	if got, want := kiroTexts(s.Context()), "user: summary: SUMMARY-TEXT-XYZ | "+history; got != want {
		t.Errorf("context:\n  %s\nwant\n  %s", got, want)
	}
	if got := kiroTexts(s.Linearize()); got != history {
		t.Errorf("linearize:\n  %s", got)
	}
	if got := kiroTexts(s.Portable().Entries); !strings.HasPrefix(got, "user: summary: SUMMARY-TEXT-XYZ | ") {
		t.Errorf("portable: %s", got)
	}
	before, err := st.Read(t.Context(), kiroV1BeforeID)
	if err != nil {
		t.Fatal(err)
	}
	if got := kiroTexts(before.Context()); got != "user: first question alpha | assistant: ANSWER-ONE | user: second question beta | assistant: ANSWER-TWO | user: third question gamma | assistant: ANSWER-THREE" {
		t.Errorf("the conversation before /compact: %s", got)
	}
}
