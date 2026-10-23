package claude

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/inference-sh/agentprotocol/transcript"
)

func TestWriteHosted(t *testing.T) {
	home := t.TempDir()
	attachments := []json.RawMessage{
		json.RawMessage(`{"type":"environment","snapshot":{"workingDirectory":"/w"}}`),
		json.RawMessage(`{"type":"model","identity":{"modelId":"m"}}`),
		json.RawMessage(`{"type":"session_context","context":{}}`),
		json.RawMessage(`{"type":"date","date":"2026-10-23"}`),
	}
	s := &transcript.Session{Agent: "llm", CWD: "/w", Entries: []transcript.Entry{
		{Role: transcript.RoleUser, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "q"}}},
		{Role: transcript.RoleAssistant, Content: []transcript.Block{{Kind: transcript.BlockToolUse, ToolID: "t1", Name: "add", Input: json.RawMessage(`{}`)}}},
		{Role: transcript.RoleTool, Content: []transcript.Block{{Kind: transcript.BlockToolResult, ToolID: "t1", Text: "42", Status: transcript.StatusOK}}},
		{Role: transcript.RoleAssistant, Content: []transcript.Block{{Kind: transcript.BlockToolUse, ToolID: "t2", Name: "add", Input: json.RawMessage(`{}`)}}},
		{Role: transcript.RoleTool, Content: []transcript.Block{{Kind: transcript.BlockToolResult, ToolID: "t2", Status: transcript.StatusOK}}},
	}}
	id, err := WriteHosted(context.Background(), home, s, attachments)
	if err != nil {
		t.Fatal(err)
	}
	path := Codec.Layout.Dir(home, "/w") + "/" + id + ".jsonl"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]json.RawMessage
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var r map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, r)
	}
	var types []string
	for _, r := range rows {
		var typ string
		_ = json.Unmarshal(r["type"], &typ)
		types = append(types, typ)
	}
	if got, want := strings.Join(types, ","), "user,attachment,attachment,attachment,attachment,assistant,user,assistant,user"; got != want {
		t.Fatalf("rows %s, want %s", got, want)
	}
	// One chain, in order.
	for i := 1; i < len(rows); i++ {
		if string(rows[i]["parentUuid"]) != string(rows[i-1]["uuid"]) {
			t.Errorf("row %d's parent is %s, want %s", i, rows[i]["parentUuid"], rows[i-1]["uuid"])
		}
	}
	for i, a := range attachments {
		if got := string(rows[1+i]["attachment"]); got != string(a) {
			t.Errorf("attachment %d is %s, want %s", i, got, a)
		}
	}
	// Tool results are an MCP tool's: blocks, even with no text.
	for i, want := range map[int]string{6: `[{"type":"text","text":"42"}]`, 8: `[{"type":"text","text":""}]`} {
		var m struct {
			Content []struct {
				Content json.RawMessage `json:"content"`
			} `json:"content"`
		}
		_ = json.Unmarshal(rows[i]["message"], &m)
		if len(m.Content) != 1 || string(m.Content[0].Content) != want {
			t.Errorf("row %d: tool result %s, want %s", i, rows[i]["message"], want)
		}
	}

	// What was written reads back as the context attachments.
	got, err := ContextAttachments(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(attachments) {
		t.Fatalf("read %d attachments, want %d", len(got), len(attachments))
	}
}

func TestWriteHostedLeavesOtherWritersAlone(t *testing.T) {
	home := t.TempDir()
	st, err := Codec.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	s := &transcript.Session{Agent: "codex", CWD: "/w", Entries: []transcript.Entry{
		{Role: transcript.RoleUser, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "q"}}},
		{Role: transcript.RoleAssistant, Content: []transcript.Block{{Kind: transcript.BlockToolUse, ToolID: "t1", Name: "Bash", Input: json.RawMessage(`{}`)}}},
		{Role: transcript.RoleTool, Content: []transcript.Block{{Kind: transcript.BlockToolResult, ToolID: "t1", Text: "ok", Status: transcript.StatusOK}}},
	}}
	id, err := st.Write(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(Codec.Layout.Dir(home, "/w") + "/" + id + ".jsonl")
	if !strings.Contains(string(raw), `"content":"ok"`) {
		t.Errorf("a handoff's tool result should stay a string, as Claude Code's own tools write it:\n%s", raw)
	}
}
