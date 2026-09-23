package sqlite

import (
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

// TestReadSeesWriteAheadLog builds the state a headless goose run leaves: a
// main database file with no tables and every row in the -wal, the agent not
// having checkpointed. The main file alone is shown to hold nothing, which is
// what an immutable open reads; the store must still list the session.
func TestReadSeesWriteAheadLog(t *testing.T) {
	live := t.TempDir()
	path := filepath.Join(live, "sessions.db")
	db, err := sql.Open("sqlite", "file:"+url.PathEscape(path)+"?_pragma=journal_mode(WAL)&_pragma=wal_autocheckpoint(0)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if err := gooseSchema(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO sessions (id, working_dir) VALUES ('20260923_1', '/w')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO messages (session_id, role, content_json, created_timestamp) VALUES ('20260923_1', 'user', '[{"type":"text","text":"hi"}]', 1)`); err != nil {
		t.Fatal(err)
	}

	// Copy the files while the writer still holds them open, as a reader on
	// another process would find them.
	home := t.TempDir()
	dst := filepath.Join(home, goosePath)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "-wal"} {
		if err := copyFile(path+suffix, dst+suffix); err != nil {
			t.Fatal(err)
		}
	}

	imm, err := sql.Open("sqlite", "file:"+url.PathEscape(dst)+"?mode=ro&immutable=1")
	if err != nil {
		t.Fatal(err)
	}
	var tables int
	if err := imm.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name = 'sessions'`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	imm.Close()
	if tables != 0 {
		t.Fatalf("setup: the main file already has the sessions table, so this does not test the log")
	}

	st, err := Goose.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	infos, err := st.List(t.Context(), "/w")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(infos) != 1 || infos[0].ID != "20260923_1" {
		t.Fatalf("list = %+v, want the session that lives only in the log", infos)
	}
	s, err := st.Read(t.Context(), "20260923_1")
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Messages()) != 1 {
		t.Errorf("read %d messages", len(s.Messages()))
	}
}
