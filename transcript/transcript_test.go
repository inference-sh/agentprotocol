package transcript

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func text(id, parent string, role Role, t string) Entry {
	return Entry{ID: id, ParentID: parent, Role: role, Content: []Block{{Kind: BlockText, Text: t}}}
}

func texts(es []Entry) string {
	var out []string
	for _, e := range es {
		out = append(out, e.Text())
	}
	return strings.Join(out, " ")
}

func TestBranchFollowsLeaf(t *testing.T) {
	s := &Session{Entries: []Entry{
		text("1", "", RoleUser, "u1"),
		text("2", "1", RoleAssistant, "a1"),
		text("3", "2", RoleUser, "u2"),
		text("4", "3", RoleAssistant, "a2"),
		text("5", "2", RoleUser, "u2'"),
	}}
	if got := texts(s.Linearize()); got != "u1 a1 u2'" {
		t.Errorf("last row as leaf: %q", got)
	}
	s.Leaf = "4"
	if got := texts(s.Linearize()); got != "u1 a1 u2 a2" {
		t.Errorf("pinned leaf: %q", got)
	}
}

func TestAudience(t *testing.T) {
	s := &Session{Entries: []Entry{
		text("", "", RoleUser, "u1"),
		{Role: RoleUser, Audience: AudienceModel, Content: []Block{{Kind: BlockText, Text: "ctx"}}},
		text("", "", RoleAssistant, "a1"),
		{Role: RoleUser, Audience: AudienceUser, Content: []Block{{Kind: BlockText, Text: "/stats"}}},
		{Role: RoleUser, Audience: AudienceNone, Content: []Block{{Kind: BlockText, Text: "undone"}}},
	}}
	if got := texts(s.Messages()); got != "u1 ctx a1 /stats undone" {
		t.Errorf("messages: %q", got)
	}
	if got := texts(s.Linearize()); got != "u1 a1 /stats" {
		t.Errorf("linearize: %q", got)
	}
	if got := texts(s.Context()); got != "u1 ctx a1" {
		t.Errorf("context: %q", got)
	}
}

func TestContextCompaction(t *testing.T) {
	sum := text("", "", RoleUser, "SUMMARY")
	s := &Session{Entries: []Entry{
		text("1", "", RoleUser, "u1"),
		text("2", "1", RoleAssistant, "a1"),
		text("3", "2", RoleUser, "u2"),
		text("4", "3", RoleAssistant, "a2"),
		{ID: "5", ParentID: "4", Compaction: &Compaction{Summary: []Entry{sum}, Keep: "3"}},
		text("6", "5", RoleUser, "u3"),
		text("7", "6", RoleAssistant, "a3"),
	}}
	if got := texts(s.Context()); got != "SUMMARY u2 a2 u3 a3" {
		t.Errorf("keep from u2: %q", got)
	}
	if got := texts(s.Linearize()); got != "u1 a1 u2 a2 u3 a3" {
		t.Errorf("the person still sees retired history: %q", got)
	}
	s.Entries[4].Compaction.Keep = ""
	if got := texts(s.Context()); got != "SUMMARY u3 a3" {
		t.Errorf("keep nothing: %q", got)
	}
	// A second compaction replaces what the first left.
	s.Entries = append(s.Entries, Entry{ID: "8", ParentID: "7", Compaction: &Compaction{}}, text("9", "8", RoleUser, "u4"))
	if got := texts(s.Context()); got != "u4" {
		t.Errorf("reset: %q", got)
	}
}

func TestPortable(t *testing.T) {
	s := &Session{Agent: "a", Leaf: "2", Vendor: 1, Entries: []Entry{
		{ID: "0", Raw: json.RawMessage(`{"header":1}`)},
		{ID: "1", ParentID: "0", Role: RoleUser, Raw: json.RawMessage(`{}`), Content: []Block{{Kind: BlockText, Text: "u1"}}},
		{ID: "x", ParentID: "1", Role: RoleUser, Audience: AudienceModel, Raw: json.RawMessage(`{}`), Content: []Block{{Kind: BlockText, Text: "ctx"}}},
		{ID: "2", ParentID: "x", Role: RoleAssistant, Raw: json.RawMessage(`{}`), Content: []Block{{Kind: BlockText, Text: "a1"}}},
		{ID: "3", ParentID: "1", Role: RoleAssistant, Raw: json.RawMessage(`{}`), Content: []Block{{Kind: BlockText, Text: "other branch"}}},
	}}
	p := s.Portable()
	if got := texts(p.Entries); got != "u1 a1" {
		t.Fatalf("portable: %q", got)
	}
	for _, e := range p.Entries {
		if e.Raw != nil || e.ParentID != "" {
			t.Errorf("entry %s keeps raw %s or link %q", e.ID, e.Raw, e.ParentID)
		}
	}
	if p.Agent != "" || p.Leaf != "" || p.Vendor != nil {
		t.Errorf("portable keeps agent state: %+v", p)
	}
	if s.Entries[1].Raw == nil {
		t.Error("portable changed the session it copied")
	}
}

// row is the test format: one JSON object per entry.
type row struct {
	ID     string `json:"id"`
	Parent string `json:"parent,omitempty"`
	Role   Role   `json:"role"`
	Text   string `json:"text"`
}

var testCodec = JSONL{
	Agent:  "test",
	Layout: Layout{Root: "s", Ext: ".jsonl"},
	Tree:   true,
	Decode: func(raw json.RawMessage, s *Session) (Entry, bool, error) {
		var r row
		if err := json.Unmarshal(raw, &r); err != nil {
			return Entry{}, false, err
		}
		return text(r.ID, r.Parent, r.Role, r.Text), true, nil
	},
	Encode: func(e Entry, s *Session) (json.RawMessage, error) {
		if e.Role == RoleSystem {
			return nil, nil // the format has no system rows
		}
		return json.Marshal(row{ID: e.ID, Parent: e.ParentID, Role: e.Role, Text: e.Text()})
	},
}

func writeRead(t *testing.T, s *Session) (*Session, []row) {
	t.Helper()
	st, err := testCodec.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	id, err := st.Write(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	back, err := st.Read(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	var rows []row
	for _, e := range back.Entries {
		var r row
		if err := json.Unmarshal(e.Raw, &r); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, r)
	}
	return back, rows
}

// Another agent's rows never reach the file: its session is re-encoded.
func TestWriteForeignAgentReencodes(t *testing.T) {
	s := &Session{Agent: "other", Entries: []Entry{
		{ID: "o1", Role: RoleUser, Raw: json.RawMessage(`{"other":"format"}`), Content: []Block{{Kind: BlockText, Text: "u1"}}},
		{ID: "o2", ParentID: "o1", Role: RoleAssistant, Raw: json.RawMessage(`{"other":"format"}`), Content: []Block{{Kind: BlockText, Text: "a1"}}},
	}}
	back, rows := writeRead(t, s)
	if back.Agent != "test" {
		t.Errorf("read stamps agent %q", back.Agent)
	}
	if len(rows) != 2 || rows[0].Text != "u1" || rows[1].Text != "a1" || rows[1].Parent != rows[0].ID {
		t.Errorf("rows = %+v", rows)
	}
}

// An entry the encoder has no row for does not leave a link to nothing.
func TestWriteRelinksPastDroppedEntry(t *testing.T) {
	s := &Session{Entries: []Entry{
		text("", "", RoleUser, "u1"),
		text("", "", RoleSystem, "sys"),
		text("", "", RoleAssistant, "a1"),
	}}
	_, rows := writeRead(t, s)
	if len(rows) != 2 || rows[1].Parent != rows[0].ID {
		t.Errorf("rows = %+v", rows)
	}
}

// An append continues from the pinned leaf, not the last row in the file.
func TestAppendFollowsLeaf(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "s")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	file := `{"id":"1","role":"user","text":"u1"}
{"id":"2","parent":"1","role":"assistant","text":"a1"}
{"id":"3","parent":"2","role":"user","text":"rewound"}
`
	if err := os.WriteFile(filepath.Join(dir, "x.jsonl"), []byte(file), 0o644); err != nil {
		t.Fatal(err)
	}
	st, _ := testCodec.Open(home)
	ctx := context.Background()
	s, err := st.Read(ctx, "x")
	if err != nil {
		t.Fatal(err)
	}
	s.Leaf = "2"
	s.Entries = append(s.Entries, text("", "", RoleUser, "u2"))
	if _, err := st.Write(ctx, s); err != nil {
		t.Fatal(err)
	}
	back, _ := st.Read(ctx, "x")
	if got := texts(back.Linearize()); got != "u1 a1 u2" {
		t.Errorf("after append: %q", got)
	}
}

// A line cut short by a crash does not make the session unreadable.
func TestReadSkipsTornLine(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "s")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	file := `{"id":"1","role":"user","text":"u1"}
{"id":"2","parent":"1","role":"assistant","text":"a1"}
{"id":"3","parent":"2","role":"us`
	if err := os.WriteFile(filepath.Join(dir, "x.jsonl"), []byte(file), 0o644); err != nil {
		t.Fatal(err)
	}
	st, _ := testCodec.Open(home)
	s, err := st.Read(context.Background(), "x")
	if err != nil {
		t.Fatal(err)
	}
	if got := texts(s.Linearize()); got != "u1 a1" {
		t.Errorf("read %q", got)
	}
}
