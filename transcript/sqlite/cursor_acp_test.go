package sqlite

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/inference-sh/agentprotocol/transcript"
)

// The cursor-acp sample is one run of `cursor-agent acp` (2026.09.23) in the
// harness-test container: a prompt, a Read tool call, its result, an answer.
// Over ACP Cursor keeps the session in ~/.cursor/acp-sessions/<id>/, a blob
// store like the CLI's beside a meta.json holding only the cwd.
const (
	cursorACPCWD = "/tmp/harness-test-cursor-3714372819/test-repo"
	cursorACPID  = "7ed6c6d1-4af7-452c-a375-7df7215dcbe4"
)

func TestCursorACPRead(t *testing.T) {
	st, err := Cursor.Open("testdata/cursor-acp")
	if err != nil {
		t.Fatal(err)
	}
	infos, err := st.List(t.Context(), cursorACPCWD)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].ID != cursorACPID || infos[0].CWD != cursorACPCWD {
		t.Fatalf("list = %+v", infos)
	}
	if want := filepath.Join("testdata/cursor-acp", cursorACP, cursorACPID); infos[0].Root != want || infos[0].Path != filepath.Join(want, "store.db") {
		t.Errorf("root = %q, path = %q", infos[0].Root, infos[0].Path)
	}
	if other, _ := st.List(t.Context(), "/elsewhere"); len(other) != 0 {
		t.Errorf("list for another cwd = %+v", other)
	}
	s, err := st.Read(t.Context(), cursorACPID)
	if err != nil {
		t.Fatal(err)
	}
	if s.CWD != cursorACPCWD || s.Created.IsZero() || s.Updated.IsZero() {
		t.Errorf("cwd %q, created %v, updated %v", s.CWD, s.Created, s.Updated)
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
	if r := msgs[2].Content[0]; r.Kind != transcript.BlockToolResult || r.Text != "test" {
		t.Errorf("tool result = %+v", r)
	}
}

// TestCursorListsBoth lists a home holding a CLI session and an ACP session:
// the two stores are separate, and a caller sees both.
func TestCursorListsBoth(t *testing.T) {
	home := copyHome(t, "testdata/cursor")
	acp := filepath.Join(home, cursorACP, cursorACPID)
	if err := os.MkdirAll(acp, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"store.db", "meta.json"} {
		if err := copyFile(filepath.Join("testdata/cursor-acp", cursorACP, cursorACPID, f), filepath.Join(acp, f)); err != nil {
			t.Fatal(err)
		}
	}
	st, err := Cursor.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	all, err := st.List(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	for _, in := range all {
		ids[in.ID] = in.CWD
	}
	if len(all) != 2 || ids[cursorID] != cursorCWD || ids[cursorACPID] != cursorACPCWD {
		t.Errorf("list = %+v", all)
	}
	for _, id := range []string{cursorID, cursorACPID} {
		if s, err := st.Read(t.Context(), id); err != nil || len(s.Messages()) != 4 {
			t.Errorf("read %s: %v", id, err)
		}
	}
}

// TestCursorACPUnlisted: cursor-agent's session/list skips an ACP session
// whose meta.json is missing or has no cwd (src/acp/session-list.ts), yet
// session/load needs only the store (src/acp/agent-store.ts), so the
// store does the same: not listed, still readable.
func TestCursorACPUnlisted(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, cursorACP, cursorACPID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(filepath.Join("testdata/cursor-acp", cursorACP, cursorACPID, "store.db"), filepath.Join(dir, "store.db")); err != nil {
		t.Fatal(err)
	}
	st, err := Cursor.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if all, err := st.List(t.Context(), ""); err != nil || len(all) != 0 {
		t.Errorf("list = %+v, %v; want nothing", all, err)
	}
	if s, err := st.Read(t.Context(), cursorACPID); err != nil || len(s.Messages()) != 4 {
		t.Errorf("read: %v", err)
	}
}

// TestCursorACPSameAgent writes an ACP session back to an empty home: it
// goes back to acp-sessions, where session/load looks, with the same blobs
// and meta.json.
func TestCursorACPSameAgent(t *testing.T) {
	src, err := Cursor.Open("testdata/cursor-acp")
	if err != nil {
		t.Fatal(err)
	}
	s, err := src.Read(t.Context(), cursorACPID)
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	dst, err := CursorCodec{New: CursorCLI}.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dst.Write(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	from := filepath.Join("testdata/cursor-acp", cursorACP, cursorACPID)
	to := filepath.Join(home, cursorACP, cursorACPID)
	orig, got := blobsOf(t, filepath.Join(from, "store.db")), blobsOf(t, filepath.Join(to, "store.db"))
	if len(orig) != len(got) {
		t.Fatalf("%d blobs written, %d read", len(got), len(orig))
	}
	for id, data := range orig {
		if !bytes.Equal(got[id], data) {
			t.Errorf("blob %s differs", id)
		}
	}
	a, _ := os.ReadFile(filepath.Join(from, "meta.json"))
	b, err := os.ReadFile(filepath.Join(to, "meta.json"))
	if err != nil || !bytes.Equal(a, b) {
		t.Errorf("meta.json = %s, %v; want %s", b, err, a)
	}
	if _, err := os.Stat(filepath.Join(home, cursorChats)); !os.IsNotExist(err) {
		t.Error("an ACP session was also written to chats")
	}
}

// TestCursorNewSession writes a session Cursor never had. The registered
// codec files it under acp-sessions, the only place `cursor-agent acp`
// session/load opens; a codec opened for the CLI files it under chats, where
// `agent --resume` looks.
func TestCursorNewSession(t *testing.T) {
	built := func() *transcript.Session {
		return &transcript.Session{CWD: "/tmp/p", Title: "seeded", Entries: []transcript.Entry{
			{Role: transcript.RoleUser, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "hi"}}},
			{Role: transcript.RoleAssistant, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "hello"}}},
		}}
	}
	home := t.TempDir()
	st, err := Cursor.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.Write(t.Context(), built())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, cursorACP, id)
	if _, err := os.Stat(filepath.Join(dir, "store.db")); err != nil {
		t.Fatalf("no ACP store: %v", err)
	}
	// The sidecar as cursor-agent writes it (acp-storage.ts): schemaVersion,
	// cwd, and the title when there is one.
	if meta, _ := os.ReadFile(filepath.Join(dir, "meta.json")); string(meta) != `{"schemaVersion":1,"cwd":"/tmp/p","title":"seeded"}` {
		t.Errorf("meta.json = %s", meta)
	}
	if infos, err := st.List(t.Context(), "/tmp/p"); err != nil || len(infos) != 1 || infos[0].ID != id || infos[0].Title != "seeded" {
		t.Errorf("list = %+v, %v", infos, err)
	}

	home = t.TempDir()
	st, err = CursorCodec{New: CursorCLI}.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	id, err = st.Write(t.Context(), built())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, cursorChats, cursorDir("/tmp/p"), id, "store.db")); err != nil {
		t.Errorf("no CLI store: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, cursorACP)); !os.IsNotExist(err) {
		t.Error("a CLI session was written to acp-sessions")
	}
}
