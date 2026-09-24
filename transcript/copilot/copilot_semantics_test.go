package copilot

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"testing"

	"github.com/inference-sh/agentprotocol/transcript"
	"github.com/inference-sh/agentprotocol/transcript/transcripttest"
)

// Two ACP runs of Copilot CLI 1.0.88 in the harness-test container against
// the mock model, each loaded back over ACP session/load and then resumed
// with one more prompt, whose request to the mock is what the model is given
// on resume.
const (
	// A turn, a turn that delegates to an explore sub-agent through the task
	// tool, /compact, a turn, and a turn after resuming.
	subagentCWD = "/tmp/harness-test-copilot-compact/test-repo"
	subagentID  = "92371bbf-c936-4cea-ae4b-32090b3e29f8"
	// Two turns, /compact, a turn that runs bash, /compact again, a turn,
	// and a turn after resuming.
	twiceCWD = "/tmp/harness-test-copilot-cap2/test-repo"
	twiceID  = "ec590b04-5e2b-4203-a931-5ba7ebbd85d8"
)

// stamp is the time Copilot puts before each prompt it gives the model.
var datetime = regexp.MustCompile(`^<current_datetime>[^<]*</current_datetime>\n\n`)

// lines renders entries as role and text, a tool call as its name and a tool
// result as its output, skipping content-free rows the way the agent's own
// replay skips an assistant message with no text. The time before a prompt
// is left out; TestModelContent checks it.
func lines(es []transcript.Entry) []string {
	var out []string
	for _, e := range es {
		for _, b := range e.Content {
			switch b.Kind {
			case transcript.BlockText:
				if b.Text != "" {
					out = append(out, string(e.Role)+": "+datetime.ReplaceAllString(b.Text, ""))
				}
			case transcript.BlockToolUse:
				out = append(out, "call: "+b.Name)
			case transcript.BlockToolResult:
				out = append(out, "result: "+b.Text)
			}
		}
	}
	return out
}

func read(t *testing.T, home, id string) *transcript.Session {
	t.Helper()
	st, err := Codec.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	s, err := st.Read(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// resumed is the message Copilot gives the model in place of compacted
// history when it resumes a session, as the mock received it.
func resumed(summary string, users ...string) string {
	s := "Some of the conversation history has been summarized to free up context.\n\n" +
		"You were originally given instructions from a user over one or more turns. Here were the user messages:\n"
	for _, u := range users {
		s += "<user_message>\n" + u + "\n</user_message>\n"
	}
	return s + "\nHere is a summary of the prior context:\n<summary>\n" + summary + "\n</summary>\n"
}

// TestCompactedContext: on resume Copilot gives the model one user message
// built from the newest compaction's summary and every earlier user prompt,
// then what followed the compaction; session/load still replays the whole
// conversation to the person.
func TestCompactedContext(t *testing.T) {
	s := read(t, "testdata/home", subagentID)
	wantShown := []string{
		"user: first question alpha",
		"assistant: ANSWER-ONE",
		"user: second question beta delegates",
		"call: task",
		"result: ANSWER-TWO",
		"assistant: ANSWER-TWO", // the sub-agent's answer, which session/load replays
		"assistant: ANSWER-TWO",
		"user: third question gamma",
		"assistant: ANSWER-THREE",
		"user: fourth question delta",
		"assistant: ANSWER-FOUR",
	}
	if got := lines(s.Linearize()); !slices.Equal(got, wantShown) {
		t.Errorf("linearize:\n  %q\nwant\n  %q", got, wantShown)
	}
	wantModel := []string{
		"user: " + resumed("SUMMARY-TEXT-XYZ", "first question alpha", "second question beta delegates"),
		"user: third question gamma",
		"assistant: ANSWER-THREE",
		"user: fourth question delta",
		"assistant: ANSWER-FOUR",
	}
	if got := lines(s.Context()); !slices.Equal(got, wantModel) {
		t.Errorf("context:\n  %q\nwant\n  %q", got, wantModel)
	}
	// Another agent gets the summary in place of the turns it retired.
	if got := lines(s.Portable().Entries); !slices.Equal(got, wantModel) {
		t.Errorf("portable:\n  %q\nwant\n  %q", got, wantModel)
	}
}

// TestCompactedTwice: the summary of a second compaction lists the user
// prompts from before the first one too.
func TestCompactedTwice(t *testing.T) {
	s := read(t, "testdata/home", twiceID)
	wantModel := []string{
		"user: " + resumed("SUMMARY-TWO", "first question alpha", "second question beta", "third question gamma runs a tool"),
		"user: fourth question delta",
		"assistant: ANSWER-FOUR",
		"user: fifth question epsilon",
		"assistant: ANSWER-FIVE",
	}
	if got := lines(s.Context()); !slices.Equal(got, wantModel) {
		t.Errorf("context:\n  %q\nwant\n  %q", got, wantModel)
	}
	if got := lines(s.Linearize()); len(got) != 12 || got[0] != "user: first question alpha" || got[5] != "call: bash" {
		t.Errorf("linearize: %q", got)
	}
}

// TestSubagentRows: a sub-agent's events share the session's log and chain
// but carry its agentId. The model is never given them; session/load
// replays the sub-agent's answer and not its prompt. The sample is cut
// before its compaction, as the session stood then, so the context still
// holds the turn that delegated.
func TestSubagentRows(t *testing.T) {
	home := t.TempDir()
	src := filepath.Join("testdata/home/.copilot/session-state", subagentID, "events.jsonl")
	dir := filepath.Join(home, ".copilot/session-state", subagentID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	var cut bytes.Buffer
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(nil, 1<<24)
	for sc.Scan() {
		var ev struct{ Type string }
		if json.Unmarshal(sc.Bytes(), &ev) == nil && ev.Type == "session.compaction_start" {
			break
		}
		cut.Write(sc.Bytes())
		cut.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), cut.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	s := read(t, home, subagentID)
	wantModel := []string{
		"user: first question alpha",
		"assistant: ANSWER-ONE",
		"user: second question beta delegates",
		"call: task",
		"result: ANSWER-TWO",
		"assistant: ANSWER-TWO",
	}
	if got := lines(s.Context()); !slices.Equal(got, wantModel) {
		t.Errorf("context:\n  %q\nwant\n  %q", got, wantModel)
	}
	for _, e := range s.Linearize() {
		if e.Text() == "subagent question" {
			t.Error("the sub-agent's prompt is shown")
		}
	}
}

func TestCompactedRoundTrip(t *testing.T) {
	for _, smp := range []transcripttest.Sample{
		{Home: "testdata/home", CWD: subagentCWD, ID: subagentID},
		{Home: "testdata/home", CWD: twiceCWD, ID: twiceID},
	} {
		transcripttest.RoundTrip(t, Codec, smp)
		transcripttest.Append(t, Writer, smp)
		transcripttest.Imported(t, Writer, smp)
	}
}
