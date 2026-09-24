package grok

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/inference-sh/agentprotocol/transcript"
	"github.com/inference-sh/agentprotocol/transcript/transcripttest"
)

// The samples are ACP runs of Grok CLI in the harness-test container. The
// first is the --probe compact run's session, compacted once more over ACP
// with a mock summary long enough for grok to accept: five prompts, a mock
// read_file call, a failed and a successful /compact, then one more prompt.
// The second ran three prompts, rewound to the second with
// x.ai/rewind/execute, and ran one more. The third is one prompt with a
// PNG attached.
const (
	sampleCWD = "/tmp/harness-test-grok-2119806780"
	sampleID  = "01a0d2bc-f56f-78f1-a37e-8fd0135a718e"
	rewoundID = "01a0d2ca-0758-7b20-a275-8f5e8de76245"
	imageID   = "01a0d2ca-e564-7a82-9e56-029d9a2a7dff"
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

func TestImported(t *testing.T) {
	transcripttest.Imported(t, Codec, transcripttest.Sample{Home: "testdata/home", CWD: sampleCWD, ID: sampleID})
}
