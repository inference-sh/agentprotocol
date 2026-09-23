package kimi

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inference-sh/agentprotocol/transcript"
	"github.com/inference-sh/agentprotocol/transcript/transcripttest"
)

// The sample is one ACP run of Kimi Code 2.0.2 in the harness-test
// container: a prompt, a mock Read call, an answer.
const (
	sampleCWD = "/tmp/harness-test-kimi-22650217/test-repo"
	sampleID  = "session_a202bcbf-5f53-4504-bc9b-3f1f8e6ef37d"
)

func TestRoundTrip(t *testing.T) {
	s := transcripttest.RoundTrip(t, Codec, transcripttest.Sample{Home: "testdata/home", CWD: sampleCWD, ID: sampleID})
	if s.CWD != sampleCWD {
		t.Errorf("cwd = %q", s.CWD)
	}
	// Kimi's wire log is written in event order, not conversation order:
	// a tool result is flushed before the assistant message that requested
	// it is appended. The codec preserves that order rather than inventing
	// links the file does not have, so the check is on what is present.
	counts := map[transcript.Role]int{}
	for _, e := range s.Messages() {
		counts[e.Role]++
	}
	if counts[transcript.RoleUser] != 1 || counts[transcript.RoleAssistant] != 2 || counts[transcript.RoleTool] != 1 {
		t.Errorf("role counts = %v", counts)
	}
}

func TestWorkspaceDir(t *testing.T) {
	if got := workspaceDir(sampleCWD); got != "wd_test-repo_04efa33f6083" {
		t.Errorf("workspaceDir = %q", got)
	}
}

func TestForeign(t *testing.T) {
	transcripttest.Foreign(t, Codec, "/tmp/some/project")
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
	if !strings.HasPrefix(id, "session_") {
		t.Errorf("id = %q", id)
	}
	idx, err := os.ReadFile(filepath.Join(home, ".kimi-code", "session_index.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(idx), id) {
		t.Errorf("index:\n%s", idx)
	}
}
