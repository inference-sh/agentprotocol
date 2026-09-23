package kiro

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

// TestHandBuiltSidecarIsComplete writes a hand-built session into a copy of
// the sample store and requires its sidecar to have every field kiro's own
// sidecar has: kiro rejected the smaller one with "failed to parse session
// metadata". The sidecar must also list the ids the transcript rows carry.
func TestHandBuiltSidecarIsComplete(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".kiro", "sessions", "cli")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	real, err := os.ReadFile(filepath.Join("testdata/home/.kiro/sessions/cli", sampleID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, sampleID+".json"), real, 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := Codec.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.Write(t.Context(), &transcript.Session{CWD: "/tmp/p", Entries: []transcript.Entry{
		{Role: transcript.RoleUser, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "The codename is HERON."}}},
		{Role: transcript.RoleAssistant, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "Noted."}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	written, err := os.ReadFile(filepath.Join(dir, id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var want, got any
	if err := json.Unmarshal(real, &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(written, &got); err != nil {
		t.Fatal(err)
	}
	for _, path := range missingPaths(want, got, "") {
		t.Errorf("hand-built sidecar lacks %s, which kiro's own has", path)
	}

	var doc struct {
		State struct {
			Meta struct {
				Turns []struct {
					IDs []string `json:"message_ids"`
				} `json:"user_turn_metadatas"`
			} `json:"conversation_metadata"`
			AgentName string `json:"agent_name"`
		} `json:"session_state"`
		Reason string `json:"session_created_reason"`
	}
	if err := json.Unmarshal(written, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Reason != "subagent" || doc.State.AgentName != "belt" {
		t.Errorf("reason %q, agent %q: not taken from kiro's own sidecar", doc.Reason, doc.State.AgentName)
	}
	s, err := st.Read(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, e := range s.Messages() {
		ids = append(ids, e.ID)
	}
	if len(doc.State.Meta.Turns) != 1 || strings.Join(doc.State.Meta.Turns[0].IDs, ",") != strings.Join(ids, ",") {
		t.Errorf("sidecar lists %+v, transcript has %v", doc.State.Meta.Turns, ids)
	}
}

// missingPaths lists the object keys present in want and absent from got,
// following objects and the first element of arrays.
func missingPaths(want, got any, at string) []string {
	var out []string
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return []string{at}
		}
		for k, v := range w {
			gv, ok := g[k]
			if !ok {
				out = append(out, at+"."+k)
				continue
			}
			if gv != nil && v != nil {
				out = append(out, missingPaths(v, gv, at+"."+k)...)
			}
		}
	case []any:
		g, ok := got.([]any)
		if ok && len(w) > 0 && len(g) > 0 {
			out = append(out, missingPaths(w[0], g[0], at+"[0]")...)
		}
	}
	return out
}
