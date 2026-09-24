package grok

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/inference-sh/agentprotocol/transcript"
)

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

// texts renders entries as role: text, a tool call by its name.
func texts(es []transcript.Entry) []string {
	var out []string
	for _, e := range es {
		x := e.Text()
		for _, b := range e.Content {
			switch b.Kind {
			case transcript.BlockToolUse:
				x += "call " + b.Name
			case transcript.BlockToolResult:
				x += "result " + b.Text
			case transcript.BlockImage:
				x += " [image " + b.MediaType + "]"
			}
		}
		out = append(out, string(e.Role)+": "+x)
	}
	return out
}

// The person sees the whole conversation, the history the compaction
// retired included, and none of what grok injected for the model; the model
// is given the history the compaction built, summary and all.
func TestCompactedSession(t *testing.T) {
	s := read(t, "testdata/home", sampleID)
	const answer = "assistant: Hello from mock server."
	want := []string{
		"user: What is the project codename? Reply ONLY the codename.",
		"assistant: call read_file",
		"tool: result 1→test",
		answer,
		"user: What files are in this repository?", answer,
		"user: Summarise what you have done so far.", answer,
		"user: What was the first thing I asked you?", answer,
		"user: /compact",
		"user: What was my previous question?", answer,
		"user: What was my previous question?", answer,
	}
	if got := texts(s.Linearize()); !slices.Equal(got, want) {
		t.Errorf("linearize:\n  %q\nwant\n  %q", got, want)
	}

	ctx := s.Context()
	var roles []transcript.Role
	for _, e := range ctx {
		roles = append(roles, e.Role)
	}
	if !slices.Equal(roles, []transcript.Role{"system", "user", "user", "user", "user", "user", "user", "assistant"}) {
		t.Errorf("context roles %v, want the eight rows of chat_history.jsonl", roles)
	}
	if !strings.HasPrefix(ctx[3].Text(), "This session is being continued") {
		t.Errorf("context[3] = %.60q, want the summary", ctx[3].Text())
	}
	for _, e := range ctx {
		if strings.Contains(e.Text(), "What files are in this repository?") {
			t.Errorf("context holds retired history: %.60q", e.Text())
		}
	}

	var moved []string
	for _, e := range s.Portable().Entries {
		moved = append(moved, e.Text())
	}
	if len(moved) != 3 || !strings.HasPrefix(moved[0], "This session is being continued") || moved[1] != "What was my previous question?" {
		t.Errorf("portable = %.80q, want the summary and the turn after it", moved)
	}
}

// What grok injects is context for the model: the <user_info> prefix, the
// reminders. A prompt reads as typed, without its <user_query> frame.
func TestInjectedContext(t *testing.T) {
	s := read(t, "testdata/home", rewoundID)
	for _, e := range s.Context() {
		injected := e.Role == transcript.RoleSystem || strings.HasPrefix(e.Text(), "<")
		if injected && e.Audience != transcript.AudienceModel {
			t.Errorf("injected %s row %.40q is for audience %d", e.Role, e.Text(), e.Audience)
		}
		if e.Role == transcript.RoleUser && strings.Contains(e.Text(), "user_query>") {
			t.Errorf("prompt kept its frame: %q", e.Text())
		}
	}
	for _, e := range s.Linearize() {
		if strings.HasPrefix(e.Text(), "<") {
			t.Errorf("the person is shown injected context %.40q", e.Text())
		}
	}
}

// A rewind leaves the undone prompts in updates.jsonl behind a
// rewind_marker; they are kept, for no one.
func TestRewind(t *testing.T) {
	s := read(t, "testdata/home", rewoundID)
	const answer = "assistant: Hello from mock server."
	want := []string{"user: first prompt", answer, "user: after rewind", answer}
	if got := texts(s.Linearize()); !slices.Equal(got, want) {
		t.Errorf("linearize %q, want %q", got, want)
	}
	var undone []string
	for _, e := range s.Messages() {
		if e.Audience == transcript.AudienceNone {
			undone = append(undone, e.Text())
		}
	}
	if !slices.Equal(undone, []string{"second prompt", "Hello from mock server.", "third prompt", "Hello from mock server."}) {
		t.Errorf("undone %q", undone)
	}
}

// Both files come back byte for byte, and the summary keeps its counts.
func TestRoundTripBothFiles(t *testing.T) {
	for _, id := range []string{sampleID, rewoundID} {
		s := read(t, "testdata/home", id)
		home := t.TempDir()
		st, _ := Codec.Open(home)
		if _, err := st.Write(t.Context(), s); err != nil {
			t.Fatal(err)
		}
		src := sessionDir(t, "testdata/home", id)
		dst := sessionDir(t, home, id)
		for _, name := range []string{chatFile, updatesFile} {
			want, _ := os.ReadFile(filepath.Join(src, name))
			got, err := os.ReadFile(filepath.Join(dst, name))
			if err != nil || !bytes.Equal(got, want) {
				t.Errorf("%s: %s changed in a round trip (%v)", id, name, err)
			}
		}
		for _, field := range []string{"num_messages", "num_chat_messages"} {
			if got, want := summaryField(t, dst, field), summaryField(t, src, field); got != want {
				t.Errorf("%s: %s = %s, want %s", id, field, got, want)
			}
		}
	}
}

func sessionDir(t *testing.T, home, id string) string {
	t.Helper()
	dirs, _ := filepath.Glob(filepath.Join(home, root, "*", id))
	if len(dirs) != 1 {
		t.Fatalf("session %s: %d directories", id, len(dirs))
	}
	return dirs[0]
}

func summaryField(t *testing.T, dir, field string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "summary.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	return string(fields[field])
}

func lines(t *testing.T, path string) []json.RawMessage {
	t.Helper()
	var out []json.RawMessage
	if err := transcript.EachLine(path, func(row json.RawMessage) (bool, error) {
		out = append(out, append(json.RawMessage(nil), row...))
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// A turn appended to a session grok wrote goes to both files, so grok lists
// it among its prompts, replays it to the client and gives it to the model,
// and numbers the next prompt after it. The summary counts both files.
func TestAppendWritesBothFiles(t *testing.T) {
	s := read(t, "testdata/home", sampleID)
	s.Entries = append(s.Entries,
		transcript.Entry{Role: transcript.RoleUser, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "The codename is HERON."}}},
		transcript.Entry{Role: transcript.RoleAssistant, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "Noted: HERON."}}},
	)
	home := t.TempDir()
	st, _ := Codec.Open(home)
	if _, err := st.Write(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	src, dst := sessionDir(t, "testdata/home", sampleID), sessionDir(t, home, sampleID)

	before := lines(t, filepath.Join(src, updatesFile))
	after := lines(t, filepath.Join(dst, updatesFile))
	if len(after) != len(before)+2 {
		t.Fatalf("updates.jsonl has %d rows, want %d", len(after), len(before)+2)
	}
	for i := range before {
		if !bytes.Equal(after[i], before[i]) {
			t.Fatalf("updates.jsonl row %d changed", i)
		}
	}
	got := prompts(after)
	if len(got) != 7 || got[6] != "The codename is HERON." {
		t.Errorf("grok's prompt list is %q, want the six before and the new one", got)
	}
	var user updateLine
	var params updateParams
	if json.Unmarshal(after[len(before)], &user) != nil || json.Unmarshal(user.Params, &params) != nil {
		t.Fatal("new row does not parse")
	}
	if user.Method != acpMethod || params.Update.SessionUpdate != "user_message_chunk" || params.Update.Meta == nil || params.Update.Meta.PromptIndex == nil || *params.Update.Meta.PromptIndex != 6 {
		t.Errorf("new user row %s", after[len(before)])
	}
	if last := eventSeq(before[len(before)-1]); eventSeq(after[len(before)]) <= last {
		t.Errorf("new event id %s does not follow %d", params.Meta.EventID, last)
	}

	chatRows := lines(t, filepath.Join(dst, chatFile))
	var prompt struct {
		Content     []textPart `json:"content"`
		PromptIndex *int       `json:"prompt_index"`
	}
	if err := json.Unmarshal(chatRows[len(chatRows)-2], &prompt); err != nil || prompt.PromptIndex == nil || *prompt.PromptIndex != 6 ||
		prompt.Content[0].Text != "<user_query>\nThe codename is HERON.\n</user_query>" {
		t.Errorf("chat prompt row %s", chatRows[len(chatRows)-2])
	}
	if got := summaryField(t, dst, "num_messages"); got != "45" {
		t.Errorf("num_messages = %s, want the 45 rows of updates.jsonl", got)
	}
	if got := summaryField(t, dst, "num_chat_messages"); got != "10" {
		t.Errorf("num_chat_messages = %s, want the 10 rows of chat_history.jsonl", got)
	}
}

// A session built by hand gets an updates.jsonl whose prompts grok lists.
func TestForeignWritesUpdates(t *testing.T) {
	home := t.TempDir()
	st, _ := Codec.Open(home)
	id, err := st.Write(t.Context(), &transcript.Session{Agent: "test", CWD: "/tmp/p", Entries: []transcript.Entry{
		{Role: transcript.RoleUser, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "hi"}}},
		{Role: transcript.RoleAssistant, Content: []transcript.Block{{Kind: transcript.BlockToolUse, ToolID: "c1", Name: "read_file", Input: json.RawMessage(`{}`)}}},
		{Role: transcript.RoleTool, Content: []transcript.Block{{Kind: transcript.BlockToolResult, ToolID: "c1", Text: "out", Status: transcript.StatusError}}},
		{Role: transcript.RoleUser, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "again"}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	rows := lines(t, filepath.Join(sessionDir(t, home, id), updatesFile))
	if got := prompts(rows); !slices.Equal(got, []string{"hi", "again"}) {
		t.Errorf("prompts %q", got)
	}
	if got := summaryField(t, sessionDir(t, home, id), "num_messages"); got != "4" {
		t.Errorf("num_messages = %s, want 4", got)
	}
	back := read(t, home, id)
	want := []string{"user: hi", "assistant: call read_file", "tool: result out", "user: again"}
	if got := texts(back.Linearize()); !slices.Equal(got, want) {
		t.Errorf("read back %q", got)
	}
}
