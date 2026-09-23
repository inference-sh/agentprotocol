package sqlite

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/inference-sh/agentprotocol/transcript"
)

// gooseSample and hermesSample are one ACP run each in the harness-test
// container.
const (
	gooseCWD = "/tmp/harness-test-goose-2430368088/test-repo"
	gooseID  = "20260922_1"
	hermesID = "c00adfd4-727b-4792-96ad-6946dea9b94d"
)

func TestGooseRead(t *testing.T) {
	st, err := Goose.Open("testdata/goose")
	if err != nil {
		t.Fatal(err)
	}
	infos, err := st.List(t.Context(), gooseCWD)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].ID != gooseID {
		t.Fatalf("list = %+v", infos)
	}
	s, err := st.Read(t.Context(), gooseID)
	if err != nil {
		t.Fatal(err)
	}
	var calls, results int
	for _, e := range s.Messages() {
		for _, b := range e.Content {
			switch b.Kind {
			case transcript.BlockToolUse:
				calls++
				if b.Name != "shell" {
					t.Errorf("tool name = %q", b.Name)
				}
			case transcript.BlockToolResult:
				results++
			}
		}
	}
	if calls != 1 || results != 1 {
		t.Errorf("calls %d results %d", calls, results)
	}
	if turns := countTurns(s); turns == 0 {
		t.Error("no turn started in events")
	}
}

func TestHermesRead(t *testing.T) {
	st, err := Hermes.Open("testdata/hermes")
	if err != nil {
		t.Fatal(err)
	}
	s, err := st.Read(t.Context(), hermesID)
	if err != nil {
		t.Fatal(err)
	}
	roles := map[transcript.Role]int{}
	for _, e := range s.Messages() {
		roles[e.Role]++
	}
	if roles[transcript.RoleUser] == 0 || roles[transcript.RoleAssistant] == 0 || roles[transcript.RoleTool] == 0 {
		t.Errorf("roles = %v", roles)
	}
}

// TestRoundTrip writes each store into a fresh home and reads it back, the
// same shape the JSONL conformance test uses, since the SQLite stores share
// no code with it.
func TestGooseRoundTrip(t *testing.T)  { roundTrip(t, Goose, "goose") }
func TestHermesRoundTrip(t *testing.T) { roundTrip(t, Hermes, "hermes") }

func roundTrip(t *testing.T, codec transcript.Codec, agent string) {
	t.Helper()
	home := t.TempDir()
	st, err := codec.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	s := &transcript.Session{Agent: agent, CWD: "/tmp/p", Entries: []transcript.Entry{
		{Role: transcript.RoleUser, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "What is the codename?"}}},
		{Role: transcript.RoleAssistant, Content: []transcript.Block{{Kind: transcript.BlockToolUse, ToolID: "call_1", Name: "read", Input: []byte(`{"path":"README.md"}`)}}},
		{Role: transcript.RoleTool, Content: []transcript.Block{{Kind: transcript.BlockToolResult, ToolID: "call_1", Name: "read", Text: "HERON", Status: transcript.StatusOK}}},
		{Role: transcript.RoleAssistant, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "HERON"}}},
	}}
	id, err := st.Write(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	back, err := st.Read(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	// The check is on content, not message count: goose and hermes keep a
	// tool result in its own message, while opencode folds it into the
	// assistant message, and both are faithful.
	msgs := back.Messages()
	if len(msgs) == 0 {
		t.Fatal("no messages read back")
	}
	first := msgs[0]
	if first.Role != transcript.RoleUser || first.Text() != "What is the codename?" {
		t.Errorf("first message = %+v", first)
	}
	var call, result, finalText bool
	for _, e := range msgs {
		for _, b := range e.Content {
			switch {
			case b.Kind == transcript.BlockToolUse && b.ToolID == "call_1":
				call = true
			case b.Kind == transcript.BlockToolResult && b.Text == "HERON":
				result = true
			case b.Kind == transcript.BlockText && b.Text == "HERON" && e.Role == transcript.RoleAssistant:
				finalText = true
			}
		}
	}
	if !call || !result || !finalText {
		t.Errorf("call %v, result %v, finalText %v", call, result, finalText)
	}
	if got, _ := st.List(t.Context(), "/tmp/p"); len(got) != 1 || got[0].ID != id {
		t.Errorf("list = %+v", got)
	}
}

func TestRegistered(t *testing.T) {
	for _, agent := range []string{"goose", "hermes"} {
		if _, ok := transcript.Registered(agent); !ok {
			t.Errorf("%s not registered", agent)
		}
	}
}

func countTurns(s *transcript.Session) int {
	var n int
	for _, ev := range s.Events() {
		if ev.Type == "turn.started" {
			n++
		}
	}
	return n
}

// TestGooseWriteKeepsExisting reproduces the harness seed probe: goose has
// run a turn today, so its store holds today's first session, and a
// hand-built session is then written with no id. The write must take a new id
// and leave goose's session intact.
func TestGooseWriteKeepsExisting(t *testing.T) {
	st, err := Goose.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	today := time.Now().UTC().Format("20060102") + "_1"
	if _, err := st.Write(t.Context(), &transcript.Session{ID: today, CWD: gooseCWD, Entries: []transcript.Entry{
		{Role: transcript.RoleUser, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "goose's own turn"}}},
	}}); err != nil {
		t.Fatal(err)
	}
	id, err := st.Write(t.Context(), &transcript.Session{CWD: gooseCWD, Entries: []transcript.Entry{
		{Role: transcript.RoleUser, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "planted"}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if id == today {
		t.Fatalf("the hand-built session took the id %s of goose's own session", id)
	}
	own, err := st.Read(t.Context(), today)
	if err != nil {
		t.Fatalf("goose's own session is gone: %v", err)
	}
	if msgs := own.Messages(); len(msgs) != 1 || msgs[0].Text() != "goose's own turn" {
		t.Errorf("goose's own session was replaced: %+v", msgs)
	}
}

// TestHermesHandBuiltRestorable checks the columns hermes's ACP adapter reads
// to restore a session: it restores only source "acp", and takes the
// directory from model_config. With source "cli" the seed probe's session
// loaded as an empty one and the planted fact never reached the model.
func TestHermesHandBuiltRestorable(t *testing.T) {
	home := t.TempDir()
	st, err := Hermes.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.Write(t.Context(), &transcript.Session{CWD: "/tmp/p", Entries: []transcript.Entry{
		{Role: transcript.RoleUser, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "The codename is HERON."}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	db, done, err := openRO(filepath.Join(home, hermesPath))
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	defer db.Close()
	var source, cwd string
	if err := db.QueryRow(`SELECT source, json_extract(model_config, '$.cwd') FROM sessions WHERE id = ?`, id).Scan(&source, &cwd); err != nil {
		t.Fatal(err)
	}
	if source != "acp" {
		t.Errorf("source = %q; hermes's ACP adapter restores only \"acp\"", source)
	}
	if cwd != "/tmp/p" {
		t.Errorf("model_config cwd = %q", cwd)
	}
}
