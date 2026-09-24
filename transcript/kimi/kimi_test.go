package kimi

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inference-sh/agentprotocol/transcript"
	"github.com/inference-sh/agentprotocol/transcript/transcripttest"
)

// The sample is one ACP run of Kimi Code in the harness-test container: a
// prompt, a mock Read call, its result, an answer.
const (
	sampleCWD = "/tmp/harness-test-kimi-2532946395/test-repo"
	sampleID  = "session_9ef8feda-5261-4097-892f-94cf87e40a76"
)

func TestRoundTrip(t *testing.T) {
	s := transcripttest.RoundTrip(t, Codec, transcripttest.Sample{Home: "testdata/home", CWD: sampleCWD, ID: sampleID})
	if s.CWD != sampleCWD {
		t.Errorf("cwd = %q", s.CWD)
	}
	// The transcript rows hold the conversation in order, the tool result as
	// its own message after the call.
	var roles []transcript.Role
	for _, e := range s.Messages() {
		roles = append(roles, e.Role)
	}
	want := []transcript.Role{transcript.RoleUser, transcript.RoleAssistant, transcript.RoleTool, transcript.RoleAssistant}
	if strings.Join(asStrings(roles), ",") != strings.Join(asStrings(want), ",") {
		t.Errorf("roles = %v, want %v", roles, want)
	}
}

func asStrings(rs []transcript.Role) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = string(r)
	}
	return out
}

func TestWorkspaceDir(t *testing.T) {
	if got := workspaceDir(sampleCWD); got != "wd_test-repo_46412412ac9a" {
		t.Errorf("workspaceDir = %q", got)
	}
}

func TestForeign(t *testing.T) { transcripttest.Foreign(t, Codec, "/tmp/some/project") }

func TestAppend(t *testing.T) {
	transcripttest.Append(t, Codec, transcripttest.Sample{Home: "testdata/home", CWD: sampleCWD, ID: sampleID})
}

// TestContextRows writes a hand-built session and checks the rows kimi
// rebuilds the model's context from, which the harness seed probe found
// missing: kimi resumed the session and the model never saw the planted fact.
func TestContextRows(t *testing.T) {
	home := t.TempDir()
	st, err := Codec.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.Write(t.Context(), &transcript.Session{CWD: "/tmp/p", Entries: []transcript.Entry{
		{Role: transcript.RoleUser, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "The codename is HERON."}}},
		{Role: transcript.RoleAssistant, Content: []transcript.Block{{Kind: transcript.BlockToolUse, ToolID: "call_1", Name: "Read", Input: []byte(`{"path":"README.md"}`)}}},
		{Role: transcript.RoleTool, Content: []transcript.Block{{Kind: transcript.BlockToolResult, ToolID: "call_1", Text: "test", Status: transcript.StatusOK}}},
		{Role: transcript.RoleAssistant, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "Noted: HERON."}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	wire := filepath.Join(home, root, workspaceDir("/tmp/p"), id, "agents", "main", "wire.jsonl")
	f, err := os.Open(wire)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var userInContext, answerInContext bool
	callUUID, resultParent := "", ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r struct {
			Type    string          `json:"type"`
			Message json.RawMessage `json:"message"`
			Event   struct {
				Type       string `json:"type"`
				UUID       string `json:"uuid"`
				ParentUUID string `json:"parentUuid"`
				Part       struct {
					Text string `json:"text"`
				} `json:"part"`
			} `json:"event"`
		}
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		switch {
		case r.Type == "context.append_message" && strings.Contains(string(r.Message), "The codename is HERON."):
			userInContext = true
		case r.Type == "context.append_loop_event" && r.Event.Type == "content.part" && r.Event.Part.Text == "Noted: HERON.":
			answerInContext = true
		case r.Type == "context.append_loop_event" && r.Event.Type == "tool.call":
			callUUID = r.Event.UUID
		case r.Type == "context.append_loop_event" && r.Event.Type == "tool.result":
			resultParent = r.Event.ParentUUID
		}
	}
	if !userInContext {
		t.Error("no context.append_message carries the user's text")
	}
	if !answerInContext {
		t.Error("no content.part carries the assistant's text")
	}
	if callUUID == "" || resultParent != callUUID {
		t.Errorf("tool.result parentUuid %q does not name the tool.call uuid %q", resultParent, callUUID)
	}
}

func TestForeignIDs(t *testing.T) {
	transcripttest.ForeignIDs(t, Codec, "/tmp/some/project", validMessageID)
}

func TestListsCWD(t *testing.T) {
	transcripttest.ListsCWD(t, Codec, transcripttest.Sample{Home: "testdata/home", CWD: sampleCWD, ID: sampleID})
}

func TestImported(t *testing.T) {
	transcripttest.Imported(t, Codec, transcripttest.Sample{Home: "testdata/home", CWD: sampleCWD, ID: sampleID})
}
