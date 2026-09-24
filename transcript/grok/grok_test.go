package grok

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/inference-sh/agentprotocol/transcript"
	"github.com/inference-sh/agentprotocol/transcript/transcripttest"
)

// The sample is one ACP run of Grok CLI in the harness-test container: a
// system prompt, context, a prompt, a mock read_file call, an answer.
const (
	sampleCWD = "/tmp/harness-test-grok-3224857346"
	sampleID  = "01a0cadd-6736-7120-a28a-90c4bbe16a34"
)

func TestRoundTrip(t *testing.T) {
	s := transcripttest.RoundTrip(t, Codec, transcripttest.Sample{Home: "testdata/home", CWD: sampleCWD, ID: sampleID})
	if s.CWD != sampleCWD {
		t.Errorf("cwd = %q", s.CWD)
	}
	var tools int
	for _, e := range s.Messages() {
		if e.Role == transcript.RoleTool {
			tools++
		}
	}
	if tools != 1 {
		t.Errorf("tool entries = %d", tools)
	}
	if _, ok := s.Vendor.(*Vendor); !ok {
		t.Errorf("vendor = %T", s.Vendor)
	}
}

func TestForeign(t *testing.T) {
	transcripttest.Foreign(t, Codec, "/tmp/some/project")
}

func TestAppend(t *testing.T) {
	transcripttest.Append(t, Codec, transcripttest.Sample{Home: "testdata/home", CWD: sampleCWD, ID: sampleID})
}

// TestHandBuiltSummary checks that a session built by hand writes every
// field grok's Summary type requires, since grok rejects summary.json with
// "missing field" otherwise.
func TestHandBuiltSummary(t *testing.T) {
	home := t.TempDir()
	st, err := Codec.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.Write(t.Context(), &transcript.Session{CWD: "/tmp/p", Entries: []transcript.Entry{
		{Role: transcript.RoleUser, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "hi"}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(home, root, transcript.EscapedCwd.Name("/tmp/p"), id, "summary.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"info", "session_summary", "created_at", "updated_at", "num_messages", "current_model_id"} {
		if _, ok := fields[required]; !ok {
			t.Errorf("summary.json lacks %s, which grok requires", required)
		}
	}
}

func TestForeignIDs(t *testing.T) {
	transcripttest.ForeignIDs(t, Codec, "/tmp/some/project", func(string) bool { return true })
}

func TestListsCWD(t *testing.T) {
	transcripttest.ListsCWD(t, Codec, transcripttest.Sample{Home: "testdata/home", CWD: sampleCWD, ID: sampleID})
}
