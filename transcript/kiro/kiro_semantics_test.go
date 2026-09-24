package kiro

import (
	"slices"
	"strings"
	"testing"

	"github.com/inference-sh/agentprotocol/transcript"
	"github.com/inference-sh/agentprotocol/transcript/transcripttest"
)

// Two ACP runs of kiro-cli 2.24 (its v2 engine) in the harness-test
// container against the mock model, each loaded back over ACP session/load
// and then resumed with one more prompt, whose request to the mock is what
// the model is given on resume.
const (
	compactCWD = "/tmp/harness-test-kiro-compact/test-repo"
	// Five turns with long answers, the second calling read, /compact, a
	// turn, and a turn after resuming. The compaction kept the last two
	// turns.
	compactID = "efad6832-1cd2-44b2-94c2-04f8e466e78b"
	// Two turns, /clear, a turn, and a turn after resuming.
	clearID = "dcce2ace-938a-4799-a666-20d485cc9a42"
)

// lines renders entries as role and the start of their text, a tool call as
// its name and a tool result as its output.
func lines(es []transcript.Entry) []string {
	var out []string
	for _, e := range es {
		for _, b := range e.Content {
			switch b.Kind {
			case transcript.BlockText:
				t := b.Text
				if i := strings.Index(t, " word word"); i > 0 {
					t = t[:i] + " …"
				}
				out = append(out, string(e.Role)+": "+t)
			case transcript.BlockToolUse:
				out = append(out, "call: "+b.Name)
			case transcript.BlockToolResult:
				out = append(out, "result: "+b.Text)
			}
		}
	}
	return out
}

func read(t *testing.T, id string) *transcript.Session {
	t.Helper()
	st, err := Codec.Open("testdata/home")
	if err != nil {
		t.Fatal(err)
	}
	s, err := st.Read(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// summary is the context entry kiro opens the resumed request with, as the
// mock received it.
const summary = "--- CONTEXT ENTRY BEGIN ---\nThis summary contains ALL relevant information from our previous conversation including tool uses, results, code analysis, and file operations. YOU MUST reference this information when answering questions and explicitly acknowledge specific details from the summary when they're relevant to the current question.\n\nSUMMARY CONTENT:\nSUMMARY-TEXT-XYZ\n\n## FACTUAL RECORD\n\n### Files Modified and Read\n_(Most frequently and recently accessed first. Indented items show summaries for the last 5 modifications.)_\n* README.md\n\n\n--- CONTEXT ENTRY END ---"

// TestCompactedContext: on resume kiro gives the model the summary, then the
// turns the compaction kept, then what followed; session/load replays the
// whole conversation to the person.
func TestCompactedContext(t *testing.T) {
	s := read(t, compactID)
	wantModel := []string{
		"user: " + summary,
		"user: fourth question delta",
		"assistant: ANSWER-FOUR …",
		"user: fifth question epsilon",
		"assistant: ANSWER-FIVE …",
		"user: sixth question zeta",
		"assistant: ANSWER-SIX",
		"user: seventh question eta",
		"assistant: ANSWER-SEVEN",
	}
	if got := lines(s.Context()); !slices.Equal(got, wantModel) {
		t.Errorf("context:\n  %q\nwant\n  %q", got, wantModel)
	}
	shown := lines(s.Linearize())
	if len(shown) != 16 || shown[0] != "user: first question alpha" || shown[3] != "call: read" || shown[15] != "assistant: ANSWER-SEVEN" {
		t.Errorf("linearize: %q", shown)
	}
	// Another agent gets the summary in place of the turns it retired.
	if got := lines(s.Portable().Entries); !slices.Equal(got, wantModel) {
		t.Errorf("portable:\n  %q\nwant\n  %q", got, wantModel)
	}
}

// TestClearedContext: /clear leaves the conversation on screen and gives the
// model nothing from before it.
func TestClearedContext(t *testing.T) {
	s := read(t, clearID)
	wantModel := []string{
		"user: third question gamma",
		"assistant: ANSWER-THREE",
		"user: fourth question delta",
		"assistant: ANSWER-FOUR",
	}
	if got := lines(s.Context()); !slices.Equal(got, wantModel) {
		t.Errorf("context:\n  %q\nwant\n  %q", got, wantModel)
	}
	if got := lines(s.Linearize()); len(got) != 8 || got[0] != "user: first question alpha" {
		t.Errorf("linearize: %q", got)
	}
}

func TestCompactedRoundTrip(t *testing.T) {
	for _, id := range []string{compactID, clearID} {
		smp := transcripttest.Sample{Home: "testdata/home", CWD: compactCWD, ID: id}
		transcripttest.RoundTrip(t, Codec, smp)
		transcripttest.Append(t, Codec, smp)
		transcripttest.Imported(t, Codec, smp)
	}
}
