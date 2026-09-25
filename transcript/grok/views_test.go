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
	"github.com/inference-sh/agentprotocol/transcript/claude"
	"github.com/inference-sh/agentprotocol/transcript/pi"
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
// retired and the echoes of both /compact runs included, as grok replays
// updates.jsonl, and none of what grok injected for the model; the model
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
		// The echo of the /compact that succeeded, which grok logged after
		// its checkpoint and replays ahead of the next prompt.
		"user: /compact",
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

	// Moved on, the prompt the compaction kept travels in the marker's
	// Summary with the summary, and grok's system prompt, user_info and
	// preamble stay behind; applied, that is the conversation the model had.
	if got := keptSummary(s); !slices.Equal(got, []string{"user: What was my previous question?", "user: Summary:\nThe user asked for the projec"}) {
		t.Errorf("marker summary %q, want the kept prompt, then the summary", got)
	}
	var moved []string
	for _, e := range s.Portable().Lower(transcript.Capabilities{}).Entries {
		moved = append(moved, e.Text())
	}
	if len(moved) != 4 || moved[0] != "What was my previous question?" || !strings.HasPrefix(moved[1], "Summary:\nThe user asked") || moved[2] != "What was my previous question?" {
		t.Errorf("portable = %.80q, want the kept prompt, the summary and the turn after it", moved)
	}
}

// keptSummary lists the Summary of a session's portable compaction marker
// as role: text, each text cut to its first 38 bytes.
func keptSummary(s *transcript.Session) []string {
	var out []string
	for _, e := range s.Portable().Entries {
		if e.Compaction == nil {
			continue
		}
		for _, m := range e.Compaction.Summary {
			t := m.Text()
			if len(t) > 38 {
				t = t[:38]
			}
			out = append(out, string(m.Role)+": "+t)
		}
	}
	return out
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
	for _, id := range []string{sampleID, rewoundID, resumedID} {
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

// A compacted session from another agent, pi's compaction probe in the
// harness-test container (four prompts, one run as a private shell command,
// a compaction keeping the last answer, a prompt after it), is recorded the
// way grok compacts: updates.jsonl holds the whole history with a
// compaction_checkpoint after the retired part, whose checkpoint file holds
// the compacted history, and chat_history.jsonl holds the kept answer, then
// the summary as grok words its own, then the turn after. The private
// command's output is in updates.jsonl alone.
func TestWriteCompacted(t *testing.T) {
	src, err := pi.Codec.Open("../pi/testdata/home")
	if err != nil {
		t.Fatal(err)
	}
	in, err := src.Read(t.Context(), "01a0d2bd-7043-757c-80b2-3f24970d3f7c")
	if err != nil {
		t.Fatal(err)
	}
	in.Agent = "elsewhere"
	home := t.TempDir()
	st, _ := Codec.Open(home)
	id, err := st.Write(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	s := read(t, home, id)
	const answer = "assistant: Hello from mock server."
	want := []string{
		"user: What is the project codename? Reply ONLY the codename.", answer,
		"user: Ran `ls`\n```\nnotes.md\n\n```", "user: Ran `echo private`\n```\nprivate\n\n```",
		"user: Second question.", answer,
		"user: Third question.", answer,
		"user: After compaction.", answer,
	}
	if got := texts(s.Linearize()); !slices.Equal(got, want) {
		t.Errorf("linearize:\n  %q\nwant\n  %q", got, want)
	}
	ctx := s.Context()
	// The summary is a user row behind grok's preamble, as grok stores its
	// own (format_compact_summary_content), pi's text after it without pi's
	// own wrapping.
	if len(ctx) != 4 || ctx[1].Role != transcript.RoleUser || !strings.HasPrefix(ctx[1].Text(), summaryPreamble+"Hello from mock server.") {
		t.Fatalf("context = %.60q, want the kept answer, then the summary", texts(ctx))
	}
	if got := texts([]transcript.Entry{ctx[0], ctx[2], ctx[3]}); !slices.Equal(got, []string{answer, "user: After compaction.", answer}) {
		t.Errorf("context around the summary = %q", got)
	}
	// Moved on again, the answer the compaction kept goes with the summary.
	if got := keptSummary(s); !slices.Equal(got, []string{answer, "user: Hello from mock server.\n\n---\n\n**Turn C"}) {
		t.Errorf("marker summary %q, want the kept answer, then the summary", got)
	}

	dir := sessionDir(t, home, id)
	var checkpoint struct {
		Update struct {
			SessionUpdate  string `json:"sessionUpdate"`
			CheckpointID   string `json:"checkpoint_id"`
			PromptIndex    int    `json:"prompt_index_at_compaction"`
			CheckpointFile string `json:"checkpoint_file"`
		} `json:"update"`
	}
	for _, row := range lines(t, filepath.Join(dir, updatesFile)) {
		var line updateLine
		if json.Unmarshal(row, &line) == nil && line.Method == xaiMethod && json.Unmarshal(line.Params, &checkpoint) == nil {
			break
		}
	}
	if checkpoint.Update.SessionUpdate != "compaction_checkpoint" || checkpoint.Update.PromptIndex != 4 {
		t.Fatalf("checkpoint update %+v, want one before prompt 4", checkpoint.Update)
	}
	raw, err := os.ReadFile(filepath.Join(dir, checkpoint.Update.CheckpointFile))
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		CheckpointID string            `json:"checkpoint_id"`
		History      []json.RawMessage `json:"compacted_history"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	if len(file.History) != 2 || file.CheckpointID != checkpoint.Update.CheckpointID {
		t.Errorf("checkpoint file %s, want the kept answer and the summary", raw)
	}
}

// resumedID is qwen's compacted sample (four turns, /compress, one turn
// after) written into grok's home in the harness-test container, then
// resumed over ACP with one more prompt. grok rewrote chat_history.jsonl
// with its system prompt ahead of the summary the codec wrote, and nothing
// kept between them.
const resumedID = "69722182-94aa-4598-a445-42cdcbf44bd9"

// The summary is found after grok's system prompt, so the model's context
// is the summary and the turns since, the person sees every turn, and the
// summary moves on with the session.
func TestResumedForeignCompaction(t *testing.T) {
	s := read(t, "testdata/home", resumedID)
	ctx := s.Context()
	if len(ctx) != 6 || ctx[0].Role != transcript.RoleSystem || !strings.HasPrefix(ctx[1].Text(), "Hello from mock server.\n\nResume the prior task") {
		t.Fatalf("context = %.60q, want the system prompt, the summary, the turns since", texts(ctx))
	}
	lin := texts(s.Linearize())
	if len(lin) != 15 || lin[4] != "user: What files are in this repository?" || slices.Contains(lin, "user: "+ctx[1].Text()) {
		t.Errorf("linearize = %.60q, want every turn and no summary", lin)
	}
	var carried bool
	for _, e := range s.Portable().Entries {
		if c := e.Compaction; c != nil && len(c.Summary) == 1 && c.Summary[0].Text() == ctx[1].Text() {
			carried = true
		}
	}
	if !carried {
		t.Error("the summary does not move with the session")
	}
}

// An entry shown and never sent that follows a compaction, a command's
// echo, is written to updates.jsonl as a host turn after the checkpoint,
// and reads back shown in its place and kept from the model.
func TestEchoAfterCompaction(t *testing.T) {
	text := func(id string, role transcript.Role, s string) transcript.Entry {
		return transcript.Entry{ID: id, Role: role, Content: []transcript.Block{{Kind: transcript.BlockText, Text: s}}}
	}
	echo := text("e", transcript.RoleUser, "/stats")
	echo.Audience = transcript.AudienceUser
	in := &transcript.Session{Agent: "elsewhere", CWD: "/tmp/p", Entries: []transcript.Entry{
		text("1", transcript.RoleUser, "u1"), text("2", transcript.RoleAssistant, "a1"),
		{ID: "c", Compaction: &transcript.Compaction{Summary: []transcript.Entry{text("", transcript.RoleUser, "SUMMARY")}}},
		text("3", transcript.RoleUser, "u2"), text("4", transcript.RoleAssistant, "a2"),
		echo,
		text("5", transcript.RoleUser, "u3"), text("6", transcript.RoleAssistant, "a3"),
	}}
	home := t.TempDir()
	st, _ := Codec.Open(home)
	id, err := st.Write(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	s := read(t, home, id)
	want := []string{"user: u1", "assistant: a1", "user: u2", "assistant: a2", "user: /stats", "user: u3", "assistant: a3"}
	if got := texts(s.Linearize()); !slices.Equal(got, want) {
		t.Errorf("linearize %q, want %q", got, want)
	}
	for _, e := range s.Context() {
		if e.Text() == "/stats" {
			t.Error("the echo reaches the model")
		}
	}
}

// Claude Code's compacted sample (claude 2.1.281 in the harness-test
// container, /compact keeping the last two turns) carries a summary that
// already opens with the words grok's own does. Written into grok, it is
// one compaction_meta user row with the preamble once, which the reader
// finds as the summary again.
func TestWriteClaudeSummary(t *testing.T) {
	src, err := claude.Codec.Open("../claude/testdata/home")
	if err != nil {
		t.Fatal(err)
	}
	in, err := src.Read(t.Context(), "6deafb15-b5f3-476d-85d6-0d87ecca9ff4")
	if err != nil {
		t.Fatal(err)
	}
	in.Agent = "elsewhere"
	home := t.TempDir()
	st, _ := Codec.Open(home)
	id, err := st.Write(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	var metas []string
	for _, row := range lines(t, filepath.Join(sessionDir(t, home, id), chatFile)) {
		var r chatRow
		if json.Unmarshal(row, &r) == nil && r.SyntheticReason == compactionMeta {
			metas = append(metas, contentText(r.Content))
		}
	}
	if len(metas) != 1 || !strings.HasPrefix(metas[0], summaryPreamble) || strings.Count(metas[0], summaryPreamble) != 1 {
		t.Fatalf("compaction_meta rows %.120q, want one summary with grok's preamble once", metas)
	}
	s := read(t, home, id)
	var summary int
	for _, e := range s.Context() {
		if e.Text() == metas[0] {
			summary++
			if e.Role != transcript.RoleUser {
				t.Errorf("summary role %s", e.Role)
			}
		}
	}
	if summary != 1 {
		t.Errorf("the summary is %d times in the context", summary)
	}
}
