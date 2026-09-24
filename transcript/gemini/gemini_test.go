package gemini

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
