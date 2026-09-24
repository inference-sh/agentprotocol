package grok

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/inference-sh/agentprotocol/transcript"
)

// chatSession writes a session directory holding rows as its
// chat_history.jsonl, for row shapes no run against the mock server
// produces, and reads it back.
func chatSession(t *testing.T, rows ...string) *transcript.Session {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, root, transcript.EscapedCwd.Name("/tmp/p"), "s1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	sum := `{"info":{"id":"s1","cwd":"/tmp/p"},"session_summary":"","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z","num_messages":0,"current_model_id":"m"}`
	if err := os.WriteFile(filepath.Join(dir, "summary.json"), []byte(sum), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, chatFile), []byte(strings.Join(rows, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return read(t, home, "s1")
}

// blocks renders an entry's blocks as kind:text, a tool block by its id and
// name.
func blocks(e transcript.Entry) []string {
	var out []string
	for _, b := range e.Content {
		switch b.Kind {
		case transcript.BlockToolUse:
			out = append(out, "tool_use:"+b.ToolID+" "+b.Name)
		case transcript.BlockToolResult:
			out = append(out, "tool_result:"+b.ToolID+" "+b.Text+" "+string(b.Status))
		case transcript.BlockImage:
			out = append(out, "image:"+b.MediaType)
		default:
			out = append(out, string(b.Kind)+":"+b.Text)
		}
	}
	return out
}

// Reasoning items and calls the backend ran are siblings of the assistant
// item (ConversationItem::Reasoning and ::BackendToolCall in
// xai-grok-sampling-types conversation.rs); the mock server streams neither.
// The rows are the shapes grok's own storage tests write
// (storage/jsonl/tests.rs).
func TestReasoningAndBackendRows(t *testing.T) {
	s := chatSession(t,
		`{"type":"user","content":[{"type":"text","text":"<user_query>\nfind cats\n</user_query>"}],"prompt_index":0}`,
		`{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"Let me think..."}]}`,
		`{"type":"backend_tool_call","kind":{"tool_type":"web_search","id":"ws_1","status":"completed","action":{"type":"search","query":"cats","sources":[{"type":"url","url":"https://cats.example"}]}}}`,
		`{"type":"reasoning","id":"rs_2","summary":[],"encrypted_content":"e1"}`,
		`{"type":"assistant","content":"Cats.","model_id":"m"}`,
	)
	var got [][]string
	for _, e := range s.Context() {
		got = append(got, blocks(e))
	}
	want := [][]string{
		{"text:find cats"},
		{"reasoning:Let me think..."},
		{"tool_use:ws_1 web_search", "tool_result:ws_1 https://cats.example ok"},
		nil,
		{"text:Cats."},
	}
	if !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("context blocks\n  %q\nwant\n  %q", got, want)
	}
	for _, e := range s.Context() {
		if e.Role != transcript.RoleAssistant && e.Role != transcript.RoleUser {
			t.Errorf("row decoded as %s", e.Role)
		}
	}
}

// Sessions from before chat_format_version 1 hold chat-completions rows,
// which grok still reads (read_chat_history_sync_bounded in
// storage/jsonl/mod.rs) and no longer writes.
func TestLegacyRows(t *testing.T) {
	s := chatSession(t,
		`{"role":"system","content":"You are Grok."}`,
		`{"role":"user","content":[{"type":"text","text":"<user_query>\nread it\n</user_query>"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AA=="}}]}`,
		`{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"read_file","arguments":"{\"target_file\":\"a\"}"}}],"reasoning_content":"thinking"}`,
		`{"role":"tool","content":"body","tool_call_id":"c1"}`,
		`{"role":"assistant","content":"done"}`,
	)
	var got [][]string
	for _, e := range s.Context() {
		got = append(got, append([]string{string(e.Role)}, blocks(e)...))
	}
	want := [][]string{
		{"system", "text:You are Grok."},
		{"user", "text:read it", "image:image/png"},
		{"assistant", "reasoning:thinking", "tool_use:c1 read_file"},
		{"tool", "tool_result:c1 body ok"},
		{"assistant", "text:done"},
	}
	if !slices.EqualFunc(got, want, slices.Equal) {
		t.Fatalf("context\n  %q\nwant\n  %q", got, want)
	}
	if in := s.Context()[2].Content[1].Input; string(in) != `{"target_file":"a"}` {
		t.Errorf("tool input %s", in)
	}
}

// Written for grok from another agent, reasoning is a reasoning item before
// the assistant item, which grok sends back to the model; the updates view
// shows it as a thought.
func TestWriteReasoning(t *testing.T) {
	in := &transcript.Session{Agent: "elsewhere", CWD: "/tmp/p", Entries: []transcript.Entry{
		{Role: transcript.RoleUser, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "go"}}},
		{Role: transcript.RoleAssistant, Content: []transcript.Block{
			{Kind: transcript.BlockReasoning, Text: "weighing it"},
			{Kind: transcript.BlockText, Text: "reading"},
			{Kind: transcript.BlockToolUse, ToolID: "c", Name: "read_file", Input: []byte(`{}`)},
		}},
		{Role: transcript.RoleTool, Content: []transcript.Block{{Kind: transcript.BlockToolResult, ToolID: "c", Text: "body", Status: transcript.StatusOK}}},
		{Role: transcript.RoleAssistant, Content: []transcript.Block{{Kind: transcript.BlockReasoning, Text: "only thought"}}},
	}}
	home := t.TempDir()
	st, err := Codec.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.Write(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	s := read(t, home, id)
	var got [][]string
	for _, e := range s.Context() {
		got = append(got, append([]string{string(e.Role)}, blocks(e)...))
	}
	want := [][]string{
		{"user", "text:go"},
		{"assistant", "reasoning:weighing it"},
		{"assistant", "text:reading", "tool_use:c read_file"},
		{"tool", "tool_result:c body ok"},
		{"assistant", "reasoning:only thought"},
	}
	if !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("context\n  %q\nwant\n  %q", got, want)
	}
	chat, err := os.ReadFile(filepath.Join(home, root, transcript.EscapedCwd.Name("/tmp/p"), id, chatFile))
	if err != nil {
		t.Fatal(err)
	}
	if row := `{"type":"reasoning","id":"","summary":[{"type":"summary_text","text":"weighing it"}]}`; !strings.Contains(string(chat), row) {
		t.Errorf("chat history has no %s:\n%s", row, chat)
	}
}
