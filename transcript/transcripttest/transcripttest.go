// Package transcripttest is the conformance test every codec runs. A codec
// ships a captured sample from a real run of its agent; the test reads it,
// checks it holds a conversation, writes it into a fresh home, and checks
// the file the agent would load is the file it wrote.
package transcripttest

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/inference-sh/agentprotocol"
	"github.com/inference-sh/agentprotocol/transcript"
)

// TurnStarted is the event type a turn opens with, exported so codec tests
// can count turns without importing the agentprotocol package directly.
const TurnStarted = "turn.started"

// Sample is one captured session: a home directory tree under testdata that
// holds the agent's files exactly as the agent laid them out.
type Sample struct {
	// Home is the testdata directory that stands in for $HOME.
	Home string
	// CWD is the working directory the session was recorded in, when the
	// layout depends on it.
	CWD string
	// ID is the session id the agent gave it.
	ID string
}

// RoundTrip runs the conformance checks for a codec against a sample.
func RoundTrip(t *testing.T, codec transcript.Codec, sample Sample) *transcript.Session {
	t.Helper()
	ctx := context.Background()

	store, err := codec.Open(sample.Home)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	infos, err := store.List(ctx, sample.CWD)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var found *transcript.Info
	for i := range infos {
		if infos[i].ID == sample.ID {
			found = &infos[i]
		}
	}
	if found == nil {
		t.Fatalf("list did not return session %s; got %d sessions", sample.ID, len(infos))
	}

	s, err := store.Read(ctx, sample.ID)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if s.ID != sample.ID {
		t.Errorf("read: id = %q, want %q", s.ID, sample.ID)
	}
	msgs := s.Messages()
	if len(msgs) == 0 {
		t.Fatalf("read: no messages decoded from %d rows", len(s.Entries))
	}
	var users, assistants int
	for _, e := range msgs {
		switch e.Role {
		case transcript.RoleUser:
			users++
		case transcript.RoleAssistant:
			assistants++
		}
	}
	if users == 0 {
		t.Errorf("read: no user turn among %d messages", len(msgs))
	}
	if assistants == 0 {
		t.Errorf("read: no assistant message among %d messages", len(msgs))
	}

	events := s.Events()
	var turns int
	for _, ev := range events {
		if ev.Type == agentprotocol.AgentEventTurnStarted {
			turns++
		}
	}
	if turns == 0 {
		t.Errorf("events: no turn started among %d events", len(events))
	}

	// Same-agent round trip: the file written must be the file read.
	home := t.TempDir()
	out, err := codec.Open(home)
	if err != nil {
		t.Fatalf("open temp: %v", err)
	}
	id, err := out.Write(ctx, s)
	if err == transcript.ErrReadOnly {
		return s
	}
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if id != sample.ID {
		t.Errorf("write: id = %q, want %q", id, sample.ID)
	}
	back, err := out.List(ctx, sample.CWD)
	if err != nil {
		t.Fatalf("list written: %v", err)
	}
	if len(back) != 1 || back[0].ID != sample.ID {
		t.Fatalf("list written: got %+v", back)
	}
	if found.Path != "" && back[0].Path != "" {
		want, err := os.ReadFile(found.Path)
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(back[0].Path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(normalize(want), normalize(got)) {
			t.Errorf("round trip changed the file\n--- read from %s\n+++ written to %s", found.Path, back[0].Path)
		}
		rel, _ := filepath.Rel(sample.Home, found.Path)
		if got, _ := filepath.Rel(home, back[0].Path); got != rel {
			t.Errorf("written to %s, agent keeps it at %s", got, rel)
		}
	}

	s2, err := out.Read(ctx, sample.ID)
	if err != nil {
		t.Fatalf("read written: %v", err)
	}
	if len(s2.Messages()) != len(msgs) {
		t.Errorf("read written: %d messages, want %d", len(s2.Messages()), len(msgs))
	}
	return s
}

// Foreign checks that a session built by hand, as one imported from another
// agent would be, can be written and read back with its messages intact.
func Foreign(t *testing.T, codec transcript.Codec, cwd string) {
	t.Helper()
	ctx := context.Background()
	home := t.TempDir()
	store, err := codec.Open(home)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	s := &transcript.Session{
		Agent: "test",
		CWD:   cwd,
		Entries: []transcript.Entry{
			{Role: transcript.RoleUser, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "What is the project codename?"}}},
			{Role: transcript.RoleAssistant, Content: []transcript.Block{
				{Kind: transcript.BlockToolUse, ToolID: "call_1", Name: "read", Input: []byte(`{"path":"README.md"}`)},
			}},
			{Role: transcript.RoleTool, Content: []transcript.Block{
				{Kind: transcript.BlockToolResult, ToolID: "call_1", Name: "read", Text: "codename: HERON", Status: transcript.StatusOK},
			}},
			{Role: transcript.RoleAssistant, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "HERON"}}},
		},
	}
	id, err := store.Write(ctx, s)
	if err == transcript.ErrReadOnly {
		t.Skip("store is read-only")
	}
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if id == "" {
		t.Fatal("write: empty id")
	}
	back, err := store.Read(ctx, id)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	msgs := back.Messages()
	if len(msgs) < 3 {
		t.Fatalf("read: %d messages, want at least 3", len(msgs))
	}
	if msgs[0].Role != transcript.RoleUser || msgs[0].Text() != "What is the project codename?" {
		t.Errorf("first message = %+v", msgs[0])
	}
	last := msgs[len(msgs)-1]
	if last.Role != transcript.RoleAssistant || last.Text() != "HERON" {
		t.Errorf("last message = %+v", last)
	}
}

func normalize(b []byte) []byte {
	return bytes.TrimRight(b, "\n")
}

// Append checks the other way a session reaches an agent: read one the agent
// wrote, add a user turn and an answer, write it, and read it back. The new
// messages must be the last two on the active branch, which for a store that
// keeps a tree means they were linked to the conversation they continue.
func Append(t *testing.T, codec transcript.Codec, sample Sample) {
	t.Helper()
	ctx := context.Background()
	src, err := codec.Open(sample.Home)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	s, err := src.Read(ctx, sample.ID)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	before := len(s.Linearize())
	s.Entries = append(s.Entries,
		transcript.Entry{Role: transcript.RoleUser, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "The codename is HERON. Remember it."}}},
		transcript.Entry{Role: transcript.RoleAssistant, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "Noted: HERON."}}},
	)
	dst, err := codec.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open temp: %v", err)
	}
	id, err := dst.Write(ctx, s)
	if err == transcript.ErrReadOnly {
		t.Skip("store is read-only")
	}
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	back, err := dst.Read(ctx, id)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	lin := back.Linearize()
	if len(lin) < 2 {
		t.Fatalf("active branch has %d messages", len(lin))
	}
	if got := lin[len(lin)-2].Text(); got != "The codename is HERON. Remember it." {
		t.Errorf("second to last on the active branch = %q", got)
	}
	if got := lin[len(lin)-1].Text(); got != "Noted: HERON." {
		t.Errorf("last on the active branch = %q", got)
	}
	if len(lin) != before+2 {
		t.Errorf("active branch has %d messages, want %d: the appended turn did not continue the conversation", len(lin), before+2)
	}
}

// ForeignIDs writes a session whose entry ids come from somewhere else, as a
// session imported from another agent's store does: they are not in this
// agent's scheme, and the second names the first as its parent. The agent
// may reject ids of the wrong shape (Copilot rejects any event id that is
// not a UUID), so the writer must give the entries ids valid names and keep
// the link. valid is the agent's id scheme.
func ForeignIDs(t *testing.T, codec transcript.Codec, cwd string, valid func(string) bool) {
	t.Helper()
	ctx := context.Background()
	store, err := codec.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	s := &transcript.Session{
		Agent: "test",
		CWD:   cwd,
		Entries: []transcript.Entry{
			{ID: "seed-1", Role: transcript.RoleUser, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "The codename is HERON."}}},
			{ID: "seed-2", ParentID: "seed-1", Role: transcript.RoleAssistant, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "Noted: HERON."}}},
		},
	}
	id, err := store.Write(ctx, s)
	if err == transcript.ErrReadOnly {
		t.Skip("store is read-only")
	}
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	back, err := store.Read(ctx, id)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	msgs := back.Linearize()
	if len(msgs) != 2 || msgs[0].Text() != "The codename is HERON." || msgs[1].Text() != "Noted: HERON." {
		t.Fatalf("read back %+v, want the two messages in order", msgs)
	}
	for _, e := range msgs {
		if e.ID != "" && !valid(e.ID) {
			t.Errorf("entry written with id %q, which is not in the agent's scheme", e.ID)
		}
	}
	if msgs[1].ParentID != "" && msgs[1].ParentID != msgs[0].ID {
		t.Errorf("second entry's parent %q is not the first entry %q", msgs[1].ParentID, msgs[0].ID)
	}
}
