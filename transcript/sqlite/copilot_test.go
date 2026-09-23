package sqlite

import (
	"path/filepath"
	"testing"

	"github.com/inference-sh/agentprotocol/transcript"
)

// TestCopilotIndex writes a hand-built session and requires the index rows
// Copilot looks sessions up by: without the sessions row Copilot reports the
// session "not found or could not be loaded".
func TestCopilotIndex(t *testing.T) {
	home := t.TempDir()
	st, err := Copilot.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.Write(t.Context(), &transcript.Session{CWD: "/tmp/p", Entries: []transcript.Entry{
		{Role: transcript.RoleUser, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "The codename is HERON."}}},
		{Role: transcript.RoleAssistant, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "Noted."}}},
		{Role: transcript.RoleUser, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "Again?"}}},
		{Role: transcript.RoleAssistant, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "HERON."}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	db, done, err := openRO(filepath.Join(home, ".copilot", "session-store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	defer db.Close()
	var cwd, summary string
	if err := db.QueryRow(`SELECT cwd, summary FROM sessions WHERE id = ?`, id).Scan(&cwd, &summary); err != nil {
		t.Fatalf("sessions row: %v", err)
	}
	if cwd != "/tmp/p" || summary != "The codename is HERON." {
		t.Errorf("sessions row = %q %q", cwd, summary)
	}
	var turns int
	if err := db.QueryRow(`SELECT count(*) FROM turns WHERE session_id = ?`, id).Scan(&turns); err != nil {
		t.Fatal(err)
	}
	if turns != 2 {
		t.Errorf("%d turns, want 2", turns)
	}
	var answer string
	if err := db.QueryRow(`SELECT assistant_response FROM turns WHERE session_id = ? AND turn_index = 1`, id).Scan(&answer); err != nil || answer != "HERON." {
		t.Errorf("turn 1 answer = %q, %v", answer, err)
	}
	var version int
	if err := db.QueryRow(`SELECT version FROM schema_version`).Scan(&version); err != nil || version != copilotSchemaVersion {
		t.Errorf("schema_version = %d, %v", version, err)
	}
	// Writing again must update, not duplicate.
	if _, err := st.Write(t.Context(), &transcript.Session{ID: id, CWD: "/tmp/p", Entries: []transcript.Entry{
		{Role: transcript.RoleUser, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "Once."}}},
	}}); err != nil {
		t.Fatal(err)
	}
	db2, done2, err := openRO(filepath.Join(home, ".copilot", "session-store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer done2()
	defer db2.Close()
	if err := db2.QueryRow(`SELECT count(*) FROM turns WHERE session_id = ?`, id).Scan(&turns); err != nil || turns != 1 {
		t.Errorf("after rewrite: %d turns, %v", turns, err)
	}
}

func TestCopilotRoundTrip(t *testing.T) { roundTrip(t, Copilot, "copilot") }

func TestCopilotRegistered(t *testing.T) {
	if _, ok := transcript.Registered("copilot"); !ok {
		t.Error("copilot not registered")
	}
}
