package claude

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/inference-sh/agentprotocol/transcript"
	"github.com/inference-sh/agentprotocol/transcript/transcripttest"
)

// The sample is one headless run of claude 2.1.x in the harness-test
// container: a prompt, a mock tool call, an answer, then /compact.
const (
	sampleCWD = "/tmp/harness-test-claude-819756836/test-repo"
	sampleID  = "0109cea5-d889-480e-9672-3cd36a7a4734"
)

func TestRoundTrip(t *testing.T) {
	s := transcripttest.RoundTrip(t, Codec, transcripttest.Sample{Home: "testdata/home", CWD: sampleCWD, ID: sampleID})
	if s.CWD != sampleCWD {
		t.Errorf("cwd = %q", s.CWD)
	}
	msgs := s.Messages()
	if msgs[0].Role != transcript.RoleUser || msgs[0].Text() != "What is the project codename? Reply ONLY the codename." {
		t.Errorf("first message = %+v", msgs[0])
	}
	// The active branch after /compact starts at the compact boundary, so
	// the linearized view is shorter than the file.
	lin := s.Linearize()
	if len(lin) == 0 || len(lin) >= len(msgs) {
		t.Errorf("linearize: %d of %d messages", len(lin), len(msgs))
	}
}

func TestForeign(t *testing.T) {
	transcripttest.Foreign(t, Codec, "/tmp/some/project")
}

func TestAppend(t *testing.T) {
	transcripttest.Append(t, Codec, transcripttest.Sample{Home: "testdata/home", CWD: sampleCWD, ID: sampleID})
}

func TestForeignIDs(t *testing.T) {
	transcripttest.ForeignIDs(t, Codec, "/tmp/some/project", transcript.IsUUID)
}

func TestListsCWD(t *testing.T) {
	transcripttest.ListsCWD(t, Codec, transcripttest.Sample{Home: "testdata/home", CWD: sampleCWD, ID: sampleID})
}

// TestHolders: Claude Code's ~/.claude/sessions/<pid>.json names the session
// each running process holds, and the listing carries those pids for that
// session only.
func TestHolders(t *testing.T) {
	home := t.TempDir()
	src := filepath.Join("testdata/home/.claude/projects", transcript.MangledCwd.Name(sampleCWD))
	dst := filepath.Join(home, ".claude", "projects", transcript.MangledCwd.Name(sampleCWD))
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(src, sampleID+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, sampleID+".jsonl"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	reg := filepath.Join(home, ".claude", "sessions")
	if err := os.MkdirAll(reg, 0o755); err != nil {
		t.Fatal(err)
	}
	for pid, sid := range map[int]string{4242: sampleID, 4343: "some-other-session"} {
		body := fmt.Sprintf(`{"pid":%d,"sessionId":%q,"cwd":%q,"kind":"interactive"}`, pid, sid, sampleCWD)
		if err := os.WriteFile(filepath.Join(reg, fmt.Sprintf("%d.json", pid)), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	st, err := Codec.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	infos, err := st.List(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || len(infos[0].Holders) != 1 || infos[0].Holders[0] != 4242 {
		t.Errorf("holders = %+v, want [4242]", infos)
	}
}

func TestImported(t *testing.T) {
	transcripttest.Imported(t, Codec, transcripttest.Sample{Home: "testdata/home", CWD: sampleCWD, ID: sampleID})
}
