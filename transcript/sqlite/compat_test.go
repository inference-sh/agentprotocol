package sqlite

import (
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/inference-sh/agentprotocol/transcript"
)

// TestHermesOlderSchema lists, reads and writes a hermes database from
// before the cwd column: the session's directory comes from model_config,
// and a write names only the columns the table has. Listing a real older
// database failed with "no such column: cwd".
func TestHermesOlderSchema(t *testing.T) {
	ddl, err := os.ReadFile("testdata/hermes-old/schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	path := filepath.Join(home, hermesPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+url.PathEscape(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(ddl)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO sessions (id, source, started_at, model_config, title) VALUES ('old-1', 'cli', 1790000000.5, '{"cwd": "/work/old"}', 'an old session')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO messages (session_id, role, content, timestamp) VALUES ('old-1', 'user', 'hello', 1790000001.0)`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	st, err := Hermes.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	infos, err := st.List(t.Context(), "/work/old")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(infos) != 1 || infos[0].ID != "old-1" || infos[0].CWD != "/work/old" {
		t.Fatalf("list = %+v", infos)
	}
	s, err := st.Read(t.Context(), "old-1")
	if err != nil {
		t.Fatal(err)
	}
	if s.CWD != "/work/old" || len(s.Messages()) != 1 {
		t.Errorf("read cwd %q, %d messages", s.CWD, len(s.Messages()))
	}
	id, err := st.Write(t.Context(), &transcript.Session{CWD: "/work/new", Entries: []transcript.Entry{
		{Role: transcript.RoleUser, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "hi"}}},
	}})
	if err != nil {
		t.Fatalf("write into the older schema: %v", err)
	}
	back, err := st.Read(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if back.CWD != "/work/new" {
		t.Errorf("written session reads back cwd %q", back.CWD)
	}
}

// TestCursorWithoutMeta lists and reads a Cursor session kept without
// meta.json, as older Cursor versions left them, and rewrites it unchanged
// without adding one. Listing a real one failed the whole store.
func TestCursorWithoutMeta(t *testing.T) {
	home := copyHome(t, "testdata/cursor")
	dir := filepath.Join(home, cursorChats, cursorDir(cursorCWD), cursorID)
	if err := os.Remove(filepath.Join(dir, "meta.json")); err != nil {
		t.Fatal(err)
	}
	st, err := Cursor.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	all, err := st.List(t.Context(), "")
	if err != nil {
		t.Fatalf("unfiltered list: %v", err)
	}
	if len(all) != 1 || all[0].ID != cursorID || all[0].CWD != "" {
		t.Errorf("unfiltered list = %+v, want the session with an unknown cwd", all)
	}
	mine, err := st.List(t.Context(), cursorCWD)
	if err != nil {
		t.Fatal(err)
	}
	if len(mine) != 1 || mine[0].CWD != cursorCWD {
		t.Errorf("filtered list = %+v, want the cwd the directory hashes", mine)
	}
	s, err := st.Read(t.Context(), cursorID)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Messages()) != 4 || s.Created.IsZero() {
		t.Errorf("read %d messages, created %v", len(s.Messages()), s.Created)
	}
	before := dump(t, filepath.Join(dir, "store.db"))
	if _, err := st.Write(t.Context(), s); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "meta.json")); !os.IsNotExist(err) {
		t.Error("an unchanged rewrite added meta.json")
	}
	after := dump(t, filepath.Join(dir, "store.db"))
	for table, rows := range before {
		if len(rows) != len(after[table]) {
			t.Errorf("table %s changed on an unchanged rewrite", table)
		}
	}
}
