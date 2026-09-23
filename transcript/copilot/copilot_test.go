package copilot

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inference-sh/agentprotocol/transcript"
	"github.com/inference-sh/agentprotocol/transcript/transcripttest"
)

// The sample is one ACP run of Copilot CLI 1.0.88 in the harness-test
// container: a prompt and an answer, with no tool call, so tool rows are
// covered by the foreign test only.
const (
	sampleCWD = "/tmp/harness-test-copilot-2755684716"
	sampleID  = "e8e5d65b-4e60-49c4-9f46-20681c238c39"
)

func TestRoundTrip(t *testing.T) {
	s := transcripttest.RoundTrip(t, Codec, transcripttest.Sample{Home: "testdata/home", CWD: sampleCWD, ID: sampleID})
	if s.CWD != sampleCWD {
		t.Errorf("cwd = %q", s.CWD)
	}
	if lin := s.Linearize(); len(lin) != 2 || lin[0].Role != transcript.RoleUser || lin[1].Role != transcript.RoleAssistant {
		t.Errorf("linearize = %+v", lin)
	}
}

// TestReadOnly: without the index, a written session is one Copilot will
// not load, so the pure-Go codec refuses to write.
func TestReadOnly(t *testing.T) {
	st, err := Codec.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Write(t.Context(), &transcript.Session{CWD: "/tmp/p", Entries: []transcript.Entry{
		{Role: transcript.RoleUser, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "hi"}}},
	}}); err != transcript.ErrReadOnly {
		t.Errorf("Write err = %v, want ErrReadOnly", err)
	}
}

// The Writer tests check the file half that the sqlite module builds on.
func TestForeign(t *testing.T) {
	transcripttest.Foreign(t, Writer, "/tmp/some/project")
	home := t.TempDir()
	st, err := Writer.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.Write(t.Context(), &transcript.Session{CWD: "/tmp/p", Entries: []transcript.Entry{
		{Role: transcript.RoleUser, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "hi: there"}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	ws, err := os.ReadFile(filepath.Join(home, ".copilot", "session-state", id, "workspace.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(ws), "id: "+id+"\n") || !strings.Contains(string(ws), `name: "hi: there"`) {
		t.Errorf("workspace.yaml:\n%s", ws)
	}
}

func TestAppend(t *testing.T) {
	transcripttest.Append(t, Writer, transcripttest.Sample{Home: "testdata/home", CWD: sampleCWD, ID: sampleID})
}

// TestHandBuiltStart writes a hand-built session into a store holding one of
// Copilot's own and checks the first event: Copilot rejects a session whose
// envelope lacks parentId ("invalid session event envelope") or whose
// session.start lacks copilotVersion, and the version must be the one
// Copilot stamped in that store.
func TestHandBuiltStart(t *testing.T) {
	home := t.TempDir()
	own := filepath.Join(home, ".copilot", "session-state", sampleID)
	if err := os.MkdirAll(own, 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join("testdata/home/.copilot/session-state", sampleID, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(own, "events.jsonl"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := Writer.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.Write(t.Context(), &transcript.Session{CWD: "/tmp/p", Entries: []transcript.Entry{
		{Role: transcript.RoleUser, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "hi"}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	var first map[string]json.RawMessage
	if _, err := transcript.PeekFirstLine(filepath.Join(home, ".copilot", "session-state", id, "events.jsonl"), func(row json.RawMessage) (transcript.Info, error) {
		return transcript.Info{}, json.Unmarshal(row, &first)
	}); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"type", "data", "id", "timestamp", "parentId"} {
		if _, ok := first[k]; !ok {
			t.Errorf("session.start envelope lacks %s", k)
		}
	}
	if string(first["parentId"]) != "null" {
		t.Errorf("root parentId = %s, want null", first["parentId"])
	}
	var data struct {
		SessionID      string  `json:"sessionId"`
		CopilotVersion *string `json:"copilotVersion"`
		Producer       string  `json:"producer"`
	}
	if err := json.Unmarshal(first["data"], &data); err != nil {
		t.Fatal(err)
	}
	if data.CopilotVersion == nil || *data.CopilotVersion != "1.0.88" || data.Producer != "copilot-agent" || data.SessionID != id {
		t.Errorf("session.start data = %+v", data)
	}
}

func TestForeignIDs(t *testing.T) {
	transcripttest.ForeignIDs(t, Writer, "/tmp/some/project", transcript.IsUUID)
}
