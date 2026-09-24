package gemini

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/inference-sh/agentprotocol/transcript"
	"github.com/inference-sh/agentprotocol/transcript/transcripttest"
)

// The sample is one ACP run of gemini-cli 0.60 in the harness-test
// container: session context, a prompt, a mock write_file call, an answer.
// Its file holds $set snapshots and repeated message records, which the
// reader must replay as gemini does.
const (
	sampleCWD = "/tmp/harness-test-gemini-332863879/test-repo"
	sampleID  = "170f5754-4e70-4ac2-8bd8-22e062f69964"
)

func TestRoundTrip(t *testing.T) {
	s := transcripttest.RoundTrip(t, Codec, transcripttest.Sample{Home: "testdata/home", CWD: sampleCWD, ID: sampleID})
	if s.CWD != sampleCWD {
		t.Errorf("cwd = %q, want the project root from .project_root", s.CWD)
	}
	seen := map[string]bool{}
	var calls, results int
	for _, e := range s.Messages() {
		if seen[e.ID] {
			t.Errorf("message %s read twice; a repeated record replaces the first", e.ID)
		}
		seen[e.ID] = true
		for _, b := range e.Content {
			switch b.Kind {
			case transcript.BlockToolUse:
				calls++
			case transcript.BlockToolResult:
				results++
			}
		}
	}
	if calls == 0 || results == 0 {
		t.Errorf("tool calls %d, results %d", calls, results)
	}
}

// TestReplay reads a file built from every record kind gemini writes and
// requires the list gemini's loader would produce.
func TestReplay(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".gemini", "tmp", "p", "chats")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".gemini", "tmp", "p", ".project_root"), []byte("/w/p"), 0o644); err != nil {
		t.Fatal(err)
	}
	msg := func(id, text string) string {
		return `{"id":"` + id + `","timestamp":"2026-09-23T00:00:00.000Z","type":"user","content":[{"text":"` + text + `"}]}`
	}
	lines := []string{
		`{"sessionId":"s1","projectHash":"` + ProjectHash("/w/p") + `","startTime":"2026-09-23T00:00:00.000Z","lastUpdated":"2026-09-23T00:00:00.000Z","kind":"main"}`,
		msg("a", "first"),
		msg("b", "second"),
		msg("a", "first, edited"),
		`{"$set":{"messages":[` + msg("x", "snapshot one") + `,` + msg("y", "snapshot two") + `]}}`,
		msg("c", "after the snapshot"),
		`{"$rewindTo":"y"}`,
		msg("d", "after the rewind"),
		`{"$set":{"lastUpdated":"2026-09-23T00:01:00.000Z"}}`,
	}
	if err := os.WriteFile(filepath.Join(dir, "session-x.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := Codec.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	s, err := st.Read(t.Context(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range s.Messages() {
		got = append(got, e.ID+"="+e.Text())
	}
	want := "x=snapshot one,d=after the rewind"
	if strings.Join(got, ",") != want {
		t.Errorf("replay = %s, want %s", strings.Join(got, ","), want)
	}
	if s.CWD != "/w/p" {
		t.Errorf("cwd = %q", s.CWD)
	}
}

// TestHandBuilt writes a session gemini did not write into a home where
// another root already holds the project name. The file must carry the
// projectHash gemini requires (without it gemini reads the file as a legacy
// record and deletes it at startup), and the project must be registered
// under the next free name with its markers.
func TestHandBuilt(t *testing.T) {
	home := t.TempDir()
	gdir := filepath.Join(home, ".gemini")
	if err := os.MkdirAll(filepath.Join(gdir, "tmp", "p"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gdir, "projects.json"), []byte(`{"projects":{"/other/p":"p"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gdir, "tmp", "p", ".project_root"), []byte("/other/p"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := Codec.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.Write(t.Context(), &transcript.Session{CWD: "/work/p", Entries: []transcript.Entry{
		{Role: transcript.RoleUser, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "The codename is HERON."}}},
		{Role: transcript.RoleAssistant, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "Noted."}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	var reg struct {
		Projects map[string]string `json:"projects"`
	}
	raw, err := os.ReadFile(filepath.Join(gdir, "projects.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &reg); err != nil {
		t.Fatal(err)
	}
	if reg.Projects["/work/p"] != "p-1" || reg.Projects["/other/p"] != "p" {
		t.Errorf("projects.json = %+v, want /work/p as p-1 beside /other/p", reg.Projects)
	}
	for _, base := range []string{"tmp", "history"} {
		if got, _ := os.ReadFile(filepath.Join(gdir, base, "p-1", ".project_root")); string(got) != "/work/p" {
			t.Errorf("%s/p-1/.project_root = %q", base, got)
		}
	}
	files, _ := filepath.Glob(filepath.Join(gdir, "tmp", "p-1", "chats", "*.jsonl"))
	if len(files) != 1 {
		t.Fatalf("files = %v", files)
	}
	var head conversationHeader
	if _, err := transcript.PeekFirstLine(files[0], func(row json.RawMessage) (transcript.Info, error) {
		return transcript.Info{}, json.Unmarshal(row, &head)
	}); err != nil {
		t.Fatal(err)
	}
	if head.SessionID != id || head.ProjectHash != ProjectHash("/work/p") {
		t.Errorf("metadata = %+v, want sessionId %s and projectHash %s", head, id, ProjectHash("/work/p"))
	}
	s, err := st.Read(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if s.CWD != "/work/p" || len(s.Messages()) != 2 {
		t.Errorf("read back cwd %q, %d messages", s.CWD, len(s.Messages()))
	}
}

func TestProjectHash(t *testing.T) {
	// The value gemini itself wrote for this root, from the sample.
	if got := ProjectHash(sampleCWD); got != "b636427f49c84517b731deb3afb4e20d104d92b55f86cf99db34d7ac94b580dc" {
		t.Errorf("ProjectHash = %s", got)
	}
}

func TestForeign(t *testing.T) { transcripttest.Foreign(t, Codec, "/tmp/some/project") }

func TestAppend(t *testing.T) {
	transcripttest.Append(t, Codec, transcripttest.Sample{Home: "testdata/home", CWD: sampleCWD, ID: sampleID})
}

func TestForeignIDs(t *testing.T) {
	transcripttest.ForeignIDs(t, Codec, "/tmp/some/project", IDs.Valid)
}

func TestListsCWD(t *testing.T) {
	transcripttest.ListsCWD(t, Codec, transcripttest.Sample{Home: "testdata/home", CWD: sampleCWD, ID: sampleID})
}

// TestFileMinute: a session created in the current minute is named for the
// minute before, which gemini 0.61's load cannot overwrite; an older one
// keeps its own minute.
func TestFileMinute(t *testing.T) {
	now := time.Date(2026, 9, 24, 10, 30, 45, 0, time.UTC)
	if got := fileMinute(now.Add(-5*time.Second), now); !got.Equal(time.Date(2026, 9, 24, 10, 29, 0, 0, time.UTC)) {
		t.Errorf("created this minute: named %v", got)
	}
	old := time.Date(2026, 9, 24, 9, 12, 30, 0, time.UTC)
	if got := fileMinute(old, now); !got.Equal(time.Date(2026, 9, 24, 9, 12, 0, 0, time.UTC)) {
		t.Errorf("created earlier: named %v", got)
	}
}

func TestImported(t *testing.T) {
	transcripttest.Imported(t, Codec, transcripttest.Sample{Home: "testdata/home", CWD: sampleCWD, ID: sampleID})
}

// The compact sample is the harness-test container's compaction probe
// against gemini-cli 0.61 over ACP (scratchpad compact_30913): two sessions,
// each sent "/compress" as a prompt. gemini's ACP agent has no /compress
// command (cli acp/commands), so the model answered it like any prompt and
// nothing was compacted. The probe then resumed 1508c8b8 with session/load,
// which synced the history gemini rebuilt back into the file as a $set of
// part-shaped records and left a second file under the same id holding
// only the session context.
const (
	compactHome    = "testdata/compact"
	compactCWD     = "/tmp/harness-test-gemini-3332324892/test-repo"
	compactResumed = "1508c8b8-b29d-4784-81c8-0f16a2b12190"
	compactSlash   = "9860df61-20c4-4b7f-bea1-c52072f79fbe"
)

func TestCompactSampleRoundTrip(t *testing.T) {
	for _, id := range []string{compactResumed, compactSlash} {
		transcripttest.RoundTrip(t, Codec, transcripttest.Sample{Home: compactHome, CWD: compactCWD, ID: id})
	}
}

// TestListsResumable: gemini lists a session once, from the file it would
// load, and never a file with nothing to resume. The probe's load of
// 1508c8b8 left session-…09-11-1508c8b8.jsonl beside the real one, and a
// startup session 83e62242 that never got a prompt.
func TestListsResumable(t *testing.T) {
	st, err := Codec.Open(compactHome)
	if err != nil {
		t.Fatal(err)
	}
	infos, err := st.List(t.Context(), compactCWD)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, in := range infos {
		if _, dup := got[in.ID]; dup {
			t.Errorf("session %s listed twice", in.ID)
		}
		got[in.ID] = filepath.Base(in.Path)
	}
	want := map[string]string{
		compactResumed: "session-2026-09-24T09-10-1508c8b8.jsonl",
		compactSlash:   "session-2026-09-24T09-10-9860df61.jsonl",
	}
	if len(got) != len(want) {
		t.Errorf("listed %v, want %v", got, want)
	}
	for id, file := range want {
		if got[id] != file {
			t.Errorf("session %s listed from %q, want %q", id, got[id], file)
		}
	}
}

// TestResumedRecords: once gemini has synced its history into the file, a
// model turn's content is a list of parts carrying its function calls, and
// a tool's answer is a record gemini synthesized under <id>_response.
func TestResumedRecords(t *testing.T) {
	st, err := Codec.Open(compactHome)
	if err != nil {
		t.Fatal(err)
	}
	s, err := st.Read(t.Context(), compactResumed)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]transcript.Entry{}
	for _, e := range s.Messages() {
		byID[e.ID] = e
	}
	call := byID["6ac0f753-2c72-4d22-9f51-dc658f76f84d"]
	var calls []string
	for _, b := range call.Content {
		if b.Kind == transcript.BlockToolUse && b.Name == "write_file" {
			calls = append(calls, b.ToolID)
		}
	}
	if strings.Join(calls, ",") != "write_file__write_file_1790241046337_0,write_file__write_file_1790241046374_1" {
		t.Errorf("model turn with function call parts: calls %v", calls)
	}
	if e := byID["6ac0f753-2c72-4d22-9f51-dc658f76f84d_response"]; e.Role != transcript.RoleTool {
		t.Errorf("synthesized response record: role %q", e.Role)
	}
	ctx := s.Context()
	if len(ctx) == 0 || !strings.HasPrefix(ctx[0].Text(), "<session_context>") {
		t.Errorf("context does not open with the session context gemini gives the model")
	}
	for _, e := range s.Linearize() {
		if strings.HasPrefix(e.Text(), "<session_context>") {
			t.Errorf("linearize shows the session context %s, which gemini's history view hides", e.ID)
		}
	}
}

// TestAudience: what gemini gives the model on resume and what its history
// view shows, per record, on the session that was sent "/compress" as a
// prompt and never resumed.
func TestAudience(t *testing.T) {
	st, err := Codec.Open(compactHome)
	if err != nil {
		t.Fatal(err)
	}
	s, err := st.Read(t.Context(), compactSlash)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]transcript.Audience{
		// Session context: resent to the model, hidden from the person.
		"d04923d38bb0f6017037e74183378ef4": transcript.AudienceModel,
		// A prompt, with the hook's context part behind it.
		"0ee8c0f3-e04b-4d02-b930-7137bf8659c2": transcript.AudienceAll,
		// A model turn with no text, thoughts or calls.
		"531161c5-180e-4cce-832d-a0944b5d24a6": transcript.AudienceNone,
		"1c4a9a1c-61da-4efe-a734-d8f2b24870a8": transcript.AudienceAll,
		// A slash command: shown, and left out of the resumed history.
		"b9811406-b0dd-4ef5-8268-a00970c2b574": transcript.AudienceUser,
		"70d863fa-2675-44db-87be-2c38de83f753": transcript.AudienceAll,
	}
	for _, e := range s.Messages() {
		if a, ok := want[e.ID]; ok && e.Audience != a {
			t.Errorf("%s: audience %d, want %d", e.ID, e.Audience, a)
		}
	}
	for _, e := range s.Context() {
		if strings.HasPrefix(e.Text(), "/compress") {
			t.Errorf("context carries the slash command %s", e.ID)
		}
	}
	var shown bool
	for _, e := range s.Linearize() {
		shown = shown || strings.HasPrefix(e.Text(), "/compress")
	}
	if !shown {
		t.Error("linearize dropped the slash command the history view shows")
	}
}

// TestHookContext: a hook's additional context rides in the prompt's
// record as a part of its own. The model is given it; another agent gets
// the prompt the person typed.
func TestHookContext(t *testing.T) {
	st, err := Codec.Open(compactHome)
	if err != nil {
		t.Fatal(err)
	}
	s, err := st.Read(t.Context(), compactResumed)
	if err != nil {
		t.Fatal(err)
	}
	var sent bool
	for _, e := range s.Context() {
		sent = sent || strings.Contains(e.Text(), "<hook_context>")
	}
	if !sent {
		t.Error("the model is not given the hook's context")
	}
	for _, e := range s.Portable().Lower(transcript.Capabilities{}).Entries {
		if strings.Contains(e.Text(), "<hook_context>") {
			t.Errorf("gemini's hook context moved with the session: %q", e.Text())
		}
	}
}

// TestThoughtsNotResent: gemini records a turn's thoughts beside it and
// shows them, but strips every thought part from the history it sends
// (core/geminiChat.ts getHistoryTurns, stripThoughts), and a turn left with
// nothing is dropped. The reasoning stays in what the person sees.
func TestThoughtsNotResent(t *testing.T) {
	home := t.TempDir()
	st, err := Codec.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	text := func(s string) transcript.Block { return transcript.Block{Kind: transcript.BlockText, Text: s} }
	think := func(s string) transcript.Block { return transcript.Block{Kind: transcript.BlockReasoning, Text: s} }
	id, err := st.Write(t.Context(), &transcript.Session{CWD: "/work/p", Entries: []transcript.Entry{
		{Role: transcript.RoleUser, Content: []transcript.Block{text("hi")}},
		{Role: transcript.RoleAssistant, Content: []transcript.Block{think("PRIVATE-1"), text("hello")}},
		{Role: transcript.RoleUser, Content: []transcript.Block{text("and?")}},
		{Role: transcript.RoleAssistant, Content: []transcript.Block{think("PRIVATE-2")}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	s, err := st.Read(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range s.Context() {
		for _, b := range e.Content {
			if b.Kind == transcript.BlockReasoning {
				t.Errorf("context carries thought %q", b.Text)
			}
		}
		got = append(got, string(e.Role)+":"+e.Text())
	}
	if want := "user:hi,assistant:hello,user:and?"; strings.Join(got, ",") != want {
		t.Errorf("context = %s, want %s", strings.Join(got, ","), want)
	}
	var shown int
	for _, e := range s.Linearize() {
		for _, b := range e.Content {
			if b.Kind == transcript.BlockReasoning {
				shown++
			}
		}
	}
	if shown != 2 {
		t.Errorf("linearize shows %d thoughts, want 2", shown)
	}
}
