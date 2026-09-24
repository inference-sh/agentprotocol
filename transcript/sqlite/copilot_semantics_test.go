package sqlite

import (
	"path/filepath"
	"testing"
)

// TestCopilotIndexSkipsSubagent rewrites a captured session whose second
// turn delegates to a sub-agent. The sub-agent's answer is in the log, and
// Copilot's index does not count it as the turn's answer.
func TestCopilotIndexSkipsSubagent(t *testing.T) {
	const id = "92371bbf-c936-4cea-ae4b-32090b3e29f8"
	src, err := Copilot.Open("../copilot/testdata/home")
	if err != nil {
		t.Fatal(err)
	}
	s, err := src.Read(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	dst, err := Copilot.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dst.Write(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	db, done, err := openRO(filepath.Join(home, ".copilot", "session-store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	defer db.Close()
	var answer string
	if err := db.QueryRow(`SELECT assistant_response FROM turns WHERE session_id = ? AND turn_index = 1`, id).Scan(&answer); err != nil {
		t.Fatal(err)
	}
	if answer != "ANSWER-TWO" {
		t.Errorf("turn 1 answer = %q, want the main agent's alone", answer)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM turns WHERE session_id = ?`, id).Scan(&n); err != nil || n != 4 {
		t.Errorf("%d turns, %v; want 4", n, err)
	}
}
