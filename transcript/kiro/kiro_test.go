package kiro

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/inference-sh/agentprotocol/transcript"
	"github.com/inference-sh/agentprotocol/transcript/transcripttest"
)

// The sample is one ACP run of kiro-cli 2.23 in the harness-test
// container: a prompt, a mock read tool call, an answer.
const (
	sampleCWD = "/tmp/harness-test-kiro-2549443901/test-repo"
	sampleID  = "e53ccf60-b53a-4fdd-a00f-05547225a99d"
)

func TestRoundTrip(t *testing.T) {
	s := transcripttest.RoundTrip(t, Codec, transcripttest.Sample{Home: "testdata/home", CWD: sampleCWD, ID: sampleID})
	if s.CWD != sampleCWD {
		t.Errorf("cwd = %q", s.CWD)
	}
	msgs := s.Messages()
	roles := []transcript.Role{}
	for _, e := range msgs {
		roles = append(roles, e.Role)
	}
	want := []transcript.Role{transcript.RoleUser, transcript.RoleAssistant, transcript.RoleTool, transcript.RoleAssistant}
	if len(roles) != len(want) {
		t.Fatalf("roles = %v, want %v", roles, want)
	}
	for i := range want {
		if roles[i] != want[i] {
			t.Errorf("roles = %v, want %v", roles, want)
			break
		}
	}
	if _, ok := s.Vendor.(*Vendor); !ok {
		t.Errorf("vendor = %T, want *Vendor with the sidecar", s.Vendor)
	}
}

func TestSidecarWritten(t *testing.T) {
	transcripttest.Foreign(t, Codec, "/tmp/some/project")
	home := t.TempDir()
	st, err := Codec.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	s := &transcript.Session{CWD: "/tmp/p", Entries: []transcript.Entry{
		{Role: transcript.RoleUser, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "hi"}}},
		{Role: transcript.RoleAssistant, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "hello"}}},
	}}
	id, err := st.Write(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(home, ".kiro", "sessions", "cli", id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		SessionID string `json:"session_id"`
		CWD       string `json:"cwd"`
		State     struct {
			Meta struct {
				Turns []struct {
					IDs []string `json:"message_ids"`
				} `json:"user_turn_metadatas"`
			} `json:"conversation_metadata"`
		} `json:"session_state"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.SessionID != id || doc.CWD != "/tmp/p" {
		t.Errorf("sidecar identity = %+v", doc)
	}
	if len(doc.State.Meta.Turns) != 1 || len(doc.State.Meta.Turns[0].IDs) != 2 {
		t.Errorf("sidecar turns = %+v", doc.State.Meta.Turns)
	}
}

func TestAppend(t *testing.T) {
	transcripttest.Append(t, Codec, transcripttest.Sample{Home: "testdata/home", CWD: sampleCWD, ID: sampleID})
}
