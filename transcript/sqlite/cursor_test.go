package sqlite

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/inference-sh/agentprotocol/transcript"
)

// The cursor sample is one headless run of cursor-agent in the harness-test
// container, whose mock checkpoints the conversation as Cursor's backend does
// (harness-test b486703): a prompt, a Read tool call, its result, an
// answer.
const (
	cursorCWD = "/tmp/harness-test-cursor-956164664/test-repo"
	cursorID  = "287974f9-6a0e-4655-9cbc-f236ee648317"
)

func TestCursorRead(t *testing.T) {
	st, err := Cursor.Open("testdata/cursor")
	if err != nil {
		t.Fatal(err)
	}
	infos, err := st.List(t.Context(), cursorCWD)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].ID != cursorID || infos[0].CWD != cursorCWD {
		t.Fatalf("list = %+v", infos)
	}
	s, err := st.Read(t.Context(), cursorID)
	if err != nil {
		t.Fatal(err)
	}
	if s.CWD != cursorCWD {
		t.Errorf("cwd = %q", s.CWD)
	}
	msgs := s.Messages()
	want := []transcript.Role{transcript.RoleUser, transcript.RoleAssistant, transcript.RoleTool, transcript.RoleAssistant}
	if len(msgs) != len(want) {
		t.Fatalf("%d messages, want %d: %+v", len(msgs), len(want), msgs)
	}
	for i, r := range want {
		if msgs[i].Role != r {
			t.Errorf("message %d role = %q, want %q", i, msgs[i].Role, r)
		}
	}
	call, result := msgs[1].Content[0], msgs[2].Content[0]
	if call.Kind != transcript.BlockToolUse || call.ToolID != "tool_046f39ab-4162-4276-913e-98899cb0b966" || call.Name != "Read" {
		t.Errorf("tool call = %+v", call)
	}
	// The result is the one thing the readable transcript drops.
	if result.Kind != transcript.BlockToolResult || result.ToolID != "tool_046f39ab-4162-4276-913e-98899cb0b966" || result.Text != "test" {
		t.Errorf("tool result = %+v", result)
	}
	if msgs[3].Text() != "Hello from mock server." {
		t.Errorf("answer = %q", msgs[3].Text())
	}
}

// TestCursorSameAgent writes a session read from Cursor back out and requires
// the same blobs and root: the store is content-addressed, so equal bytes
// mean equal ids.
func TestCursorSameAgent(t *testing.T) {
	src, err := Cursor.Open("testdata/cursor")
	if err != nil {
		t.Fatal(err)
	}
	s, err := src.Read(t.Context(), cursorID)
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	dst, err := Cursor.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	id, err := dst.Write(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	if id != cursorID {
		t.Errorf("id = %q", id)
	}
	orig := blobsOf(t, filepath.Join("testdata/cursor", cursorChats, cursorDir(cursorCWD), cursorID, "store.db"))
	got := blobsOf(t, filepath.Join(home, cursorChats, cursorDir(cursorCWD), cursorID, "store.db"))
	if len(orig) != len(got) {
		t.Fatalf("%d blobs written, %d read", len(got), len(orig))
	}
	for id, data := range orig {
		if !bytes.Equal(got[id], data) {
			t.Errorf("blob %s differs", id)
		}
	}
	back, err := dst.Read(t.Context(), cursorID)
	if err != nil {
		t.Fatal(err)
	}
	if len(back.Messages()) != 4 {
		t.Errorf("read back %d messages", len(back.Messages()))
	}
}

func TestCursorRoundTrip(t *testing.T) { roundTrip(t, Cursor, "cursor") }

func TestCursorRegistered(t *testing.T) {
	if _, ok := transcript.Registered("cursor"); !ok {
		t.Error("cursor not registered")
	}
}

func blobsOf(t *testing.T, path string) map[string][]byte {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	db, err := openRO(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT id, data FROM blobs`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string][]byte{}
	for rows.Next() {
		var id string
		var data []byte
		if err := rows.Scan(&id, &data); err != nil {
			t.Fatal(err)
		}
		out[id] = data
	}
	return out
}
