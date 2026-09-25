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

// A session moves as what the model knew: the summary in place of retired
// history, without the agent's own injected context or the person's
// slash commands.
func TestPortableCarriesContext(t *testing.T) {
	s := &Session{Agent: "a", Entries: []Entry{
		text("1", "", RoleUser, "u1"),
		text("2", "1", RoleAssistant, "a1"),
		{ID: "3", ParentID: "2", Compaction: &Compaction{Summary: []Entry{text("", "", RoleUser, "SUMMARY")}}},
		{ID: "4", ParentID: "3", Role: RoleUser, Audience: AudienceUser, Content: []Block{{Kind: BlockText, Text: "/stats"}}},
		{ID: "5", ParentID: "4", Role: RoleUser, Audience: AudienceModel, Content: []Block{{Kind: BlockText, Text: "<env>"}}},
		text("6", "5", RoleUser, "u2"),
		text("7", "6", RoleAssistant, "a2"),
	}}
	p := s.Portable()
	// Everything the conversation holds travels, the compaction as a
	// marker; the agent's own context does not.
	if got := texts(p.Entries); got != "u1 a1  /stats u2 a2" {
		t.Errorf("portable: %q", got)
	}
	// A writer that can record neither a compaction nor a shown-only entry
	// gets what the model knew.
	if got := texts(p.Lower(Capabilities{}).Entries); got != "SUMMARY u2 a2" {
		t.Errorf("lowered for a plain writer: %q", got)
	}
	// One that records compactions keeps the retired history.
	if got := texts(p.Lower(Capabilities{Compaction: true}).Entries); got != "u1 a1  u2 a2" {
		t.Errorf("lowered for a writer with compactions: %q", got)
	}
	if got := texts(p.Lower(Capabilities{Compaction: true, UserOnly: true}).Entries); got != "u1 a1  /stats u2 a2" {
		t.Errorf("lowered for a writer with both: %q", got)
	}
	// Written back, the compaction still gives the model the summary.
	full := p.Lower(Capabilities{Compaction: true, UserOnly: true})
	if got := texts(full.Context()); got != "SUMMARY u2 a2" {
		t.Errorf("context of the carried session: %q", got)
	}
}

// A session the agent resumes from nothing has no branch, and an append
// starts a new root instead of continuing the history it left.
func TestRestart(t *testing.T) {
	s := &Session{Restart: true, Entries: []Entry{
		text("1", "", RoleUser, "u1"),
		text("2", "1", RoleAssistant, "a1"),
	}}
	for i := range s.Entries {
		s.Entries[i].Raw = json.RawMessage(`{}`)
	}
	if got := s.Linearize(); len(got) != 0 {
		t.Errorf("linearize: %q", texts(got))
	}
	s.Entries = append(s.Entries, text("", "", RoleUser, "u2"))
	AssignIDs(s, UUIDs, true)
	if p := s.Entries[2].ParentID; p != "" {
		t.Errorf("appended entry linked to %q", p)
	}
}

// A replacement history moves without the agent's own context in it.
func TestPortableSummary(t *testing.T) {
	s := &Session{Entries: []Entry{
		text("1", "", RoleUser, "u1"),
		{ID: "2", ParentID: "1", Compaction: &Compaction{Summary: []Entry{
			{Role: RoleSystem, Audience: AudienceModel, Content: []Block{{Kind: BlockText, Text: "system prompt"}}},
			text("", "", RoleUser, "SUMMARY"),
		}}},
		text("3", "2", RoleUser, "u2"),
	}}
	if got := texts(s.Context()); got != "system prompt SUMMARY u2" {
		t.Errorf("context: %q", got)
	}
	if got := texts(s.Portable().Lower(Capabilities{}).Entries); got != "SUMMARY u2" {
		t.Errorf("portable: %q", got)
	}
	c := s.Portable().Entries[1].Compaction
	if c == nil || len(c.Summary) != 1 || c.Summary[0].Text() != "SUMMARY" {
		t.Errorf("the marker's summary keeps only conversation: %+v", c)
	}
}

// The model is given what the agent sent, and another agent what the
// person wrote.
func TestModelContent(t *testing.T) {
	e := text("1", "", RoleUser, "question")
	e.ModelContent = []Block{{Kind: BlockText, Text: "question + hook"}}
	s := &Session{Entries: []Entry{e}}
	if got := texts(s.Context()); got != "question + hook" {
		t.Errorf("context: %q", got)
	}
	if got := texts(s.Linearize()); got != "question" {
		t.Errorf("linearize: %q", got)
	}
	p := s.Portable().Entries
	if texts(p) != "question" || p[0].ModelContent != nil {
		t.Errorf("portable: %+v", p)
	}
}

// A compaction whose first kept entry does not travel keeps from the next
// one that does.
func TestPortableKeepRemapped(t *testing.T) {
	s := &Session{Entries: []Entry{
		text("1", "", RoleUser, "u1"),
		{ID: "2", ParentID: "1", Role: RoleUser, Audience: AudienceModel, Content: []Block{{Kind: BlockText, Text: "<env>"}}},
		text("3", "2", RoleAssistant, "a1"),
		{ID: "4", ParentID: "3", Compaction: &Compaction{Summary: []Entry{text("", "", RoleUser, "S")}, Keep: "2"}},
		text("5", "4", RoleUser, "u2"),
	}}
	p := s.Portable()
	if k := p.Entries[2].Compaction.Keep; k != "3" {
		t.Errorf("keep = %q, want 3", k)
	}
	if got := texts(p.Lower(Capabilities{}).Entries); got != "S a1 u2" {
		t.Errorf("lowered: %q", got)
	}
}

// History a compaction retired, which its agent still showed, travels to a
// writer that records compactions but not shown-only entries; a shown-only
// entry the compaction does not retire does not.
func TestLowerKeepsRetiredHistory(t *testing.T) {
	retired := text("1", "", RoleUser, "u1")
	retired.Audience = AudienceUser
	stats := text("4", "", RoleUser, "/stats")
	stats.Audience = AudienceUser
	s := &Session{Entries: []Entry{
		retired,
		text("2", "", RoleAssistant, "a1"),
		{ID: "3", Compaction: &Compaction{Summary: []Entry{text("", "", RoleUser, "S")}}},
		stats,
		text("5", "", RoleUser, "u2"),
	}}
	got := s.Lower(Capabilities{Compaction: true})
	if texts(got.Entries) != "u1 a1  u2" || got.Entries[0].Audience != AudienceAll {
		t.Errorf("lowered: %q, first audience %d", texts(got.Entries), got.Entries[0].Audience)
	}
}

// A written compaction still names the entry it keeps after ids are given
// in the writer's scheme.
func TestAssignIDsFollowsKeep(t *testing.T) {
	s := &Session{Entries: []Entry{
		text("foreign-1", "", RoleUser, "u1"),
		text("foreign-2", "", RoleAssistant, "a1"),
		{Compaction: &Compaction{Keep: "foreign-2"}},
	}}
	AssignIDs(s, UUIDs, true)
	if k := s.Entries[2].Compaction.Keep; k != s.Entries[1].ID || !IsUUID(k) {
		t.Errorf("keep = %q, want %q", k, s.Entries[1].ID)
	}
}

// A compaction marker a tree writer records sits in the chain: it gets an
// id and a parent, and the entry after it names it.
func TestAssignIDsLinksMarker(t *testing.T) {
	s := &Session{Entries: []Entry{
		text("", "", RoleUser, "u1"),
		{Compaction: &Compaction{}},
		text("", "", RoleUser, "u2"),
	}}
	AssignIDs(s, UUIDs, true)
	m := s.Entries[1]
	if m.ID == "" || m.ParentID != s.Entries[0].ID || s.Entries[2].ParentID != m.ID {
		t.Errorf("chain: %q <- %q (%q) <- %q", s.Entries[0].ID, m.ID, m.ParentID, s.Entries[2].ParentID)
	}
}

// A result filed inside the assistant message that made the call travels
// in a tool entry of its own, the shape every writer takes.
func TestPortableSplitsResults(t *testing.T) {
	s := &Session{Entries: []Entry{
		text("1", "", RoleUser, "u1"),
		{ID: "2", ParentID: "1", Role: RoleAssistant, Content: []Block{
			{Kind: BlockText, Text: "reading"},
			{Kind: BlockToolUse, ToolID: "c1", Name: "read"},
			{Kind: BlockToolResult, ToolID: "c1", Text: "data"},
			{Kind: BlockImage, ToolID: "c1", MediaType: "image/png", Data: []byte{1}},
		}},
	}}
	p := s.Portable().Entries
	if len(p) != 3 || p[1].Role != RoleAssistant || len(p[1].Content) != 2 || p[2].Role != RoleTool || len(p[2].Content) != 2 {
		t.Fatalf("portable: %+v", p)
	}
}

// Parallel calls that share an ID each travel with their own, and each
// result with its call's.
func TestPortableUniqueToolIDs(t *testing.T) {
	s := &Session{Entries: []Entry{
		{ID: "1", Role: RoleAssistant, Content: []Block{
			{Kind: BlockToolUse, ToolID: "read", Name: "read"},
			{Kind: BlockToolUse, ToolID: "read", Name: "read"},
			{Kind: BlockToolUse, Name: "ls"},
		}},
		{ID: "2", ParentID: "1", Role: RoleTool, Content: []Block{
			{Kind: BlockToolResult, ToolID: "read", Text: "a"},
			{Kind: BlockToolResult, ToolID: "read", Text: "b"},
			{Kind: BlockToolResult, ToolID: "", Text: "c"},
		}},
	}}
	p := s.Portable().Entries
	calls := p[0].Content
	var results []Block
	for _, e := range p[1:] {
		results = append(results, e.Content...)
	}
	ids := map[string]bool{}
	for i := range calls {
		if ids[calls[i].ToolID] || calls[i].ToolID == "" {
			t.Fatalf("call ids not unique: %+v", calls)
		}
		ids[calls[i].ToolID] = true
		if results[i].ToolID != calls[i].ToolID {
			t.Errorf("result %d id %q, call id %q", i, results[i].ToolID, calls[i].ToolID)
		}
	}
	if s.Entries[0].Content[1].ToolID != "read" {
		t.Error("portable changed the source session's blocks")
	}
}

// Parallel results filed in one entry travel as one entry each, every
// image with its own result.
func TestPortableOneResultPerEntry(t *testing.T) {
	s := &Session{Entries: []Entry{
		{ID: "1", Role: RoleAssistant, Content: []Block{
			{Kind: BlockToolUse, ToolID: "a", Name: "read"},
			{Kind: BlockToolUse, ToolID: "b", Name: "read"},
		}},
		{ID: "2", ParentID: "1", Role: RoleTool, Content: []Block{
			{Kind: BlockToolResult, ToolID: "a", Text: "ra"},
			{Kind: BlockImage, ToolID: "a", MediaType: "image/png", Data: []byte{1}},
			{Kind: BlockToolResult, ToolID: "b", Text: "rb"},
		}},
	}}
	p := s.Portable().Entries
	if len(p) != 3 || p[1].ID != "2" || len(p[1].Content) != 2 || p[2].Content[0].Text != "rb" || p[2].ID == "2" {
		t.Fatalf("portable: %+v", p)
	}
}

// An image known only by its local path moves with its bytes; one whose
// file is gone stays a reference.
func TestPortableInlinesLocalFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pic.png")
	if err := os.WriteFile(path, []byte{0x89, 'P', 'N', 'G'}, 0o644); err != nil {
		t.Fatal(err)
	}
	s := &Session{Entries: []Entry{{ID: "1", Role: RoleUser, Content: []Block{
		{Kind: BlockText, Text: "look"},
		{Kind: BlockImage, URI: path},
		{Kind: BlockImage, URI: "file://" + path},
		{Kind: BlockImage, URI: filepath.Join(dir, "gone.png")},
	}}}}
	c := s.Portable().Entries[0].Content
	for _, k := range []int{1, 2} {
		if string(c[k].Data) != "\x89PNG" || c[k].MediaType != "image/png" {
			t.Errorf("block %d: %q %q", k, c[k].Data, c[k].MediaType)
		}
	}
	if c[3].Data != nil || c[3].URI == "" {
		t.Errorf("missing file: %+v", c[3])
	}
	if s.Entries[0].Content[1].Data != nil {
		t.Error("portable changed the source block")
	}
}

// A text file moves as text, named, to a writer that records no files; an
// image and a PDF stay what they are.
func TestPortableTextFileAsText(t *testing.T) {
	s := &Session{Entries: []Entry{{ID: "1", Role: RoleUser, Content: []Block{
		{Kind: BlockText, Text: "read this"},
		{Kind: BlockFile, MediaType: "text/plain", Name: "notes.md", Data: []byte("the codename is HERON")},
		{Kind: BlockFile, MediaType: "application/pdf", Name: "doc.pdf", Data: []byte("%PDF")},
		{Kind: BlockImage, MediaType: "image/png", Data: []byte{1}},
	}}}}
	if c := s.Portable().Lower(Capabilities{Files: true}).Entries[0].Content; c[1].Kind != BlockFile {
		t.Errorf("a writer with files got %+v", c[1])
	}
	c := s.Portable().Lower(Capabilities{}).Entries[0].Content
	if c[1].Kind != BlockText || c[1].Text != "<file name=\"notes.md\">\nthe codename is HERON\n</file>" {
		t.Errorf("text file: %+v", c[1])
	}
	if c[2].Kind != BlockFile || c[3].Kind != BlockImage {
		t.Errorf("other files changed: %+v %+v", c[2], c[3])
	}
}

func TestCompactionText(t *testing.T) {
	c := &Compaction{Summary: []Entry{text("1", "", RoleUser, "<w>one</w>"), {}, text("2", "", RoleUser, "two")}}
	if got := c.Text(nil); got != "<w>one</w>\n\ntwo" {
		t.Errorf("Text(nil) = %q", got)
	}
	unwrap := func(s string) string { return strings.TrimSuffix(strings.TrimPrefix(s, "<w>"), "</w>") }
	if got := c.Text(unwrap); got != "one\n\ntwo" {
		t.Errorf("Text(unwrap) = %q", got)
	}
}

func TestUnkept(t *testing.T) {
	es := []Entry{
		{ID: "a", Role: RoleUser},
		{ID: "b", Role: RoleAssistant},
		{ID: "c", Role: RoleOpaque, Compaction: &Compaction{Keep: "b"}},
		{ID: "d", Role: RoleUser},
	}
	got := Unkept(es)
	var ids []string
	for _, e := range got {
		ids = append(ids, e.ID)
	}
	if strings.Join(ids, ",") != "a,c,b,d" || got[1].Compaction.Keep != "" {
		t.Errorf("Unkept order = %v, keep %q", ids, got[1].Compaction.Keep)
	}
	if es[2].Compaction.Keep != "b" {
		t.Error("Unkept changed its input")
	}
}

func TestWriteFileAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a", "b.json")
	if err := WriteFileAtomic(path, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "x" {
		t.Errorf("read %q, %v", data, err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("temporary file left behind: %v", err)
	}
}
