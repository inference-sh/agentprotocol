package cursor

import (
	"testing"

	"github.com/inference-sh/agentprotocol/transcript"
	"github.com/inference-sh/agentprotocol/transcript/transcripttest"
)

// The sample is one headless run of cursor-agent in the harness-test
// container, whose mock checkpoints the conversation the way Cursor's
// backend does (harness-test 4b2d12c): a prompt, a read_file tool call and an
// answer. Cursor's transcript writer emits no row for the tool result, so the
// transcript holds three messages; the sqlite module's Cursor codec reads the
// result from the blob store.
const (
	sampleCWD = "/tmp/harness-test-cursor-305147681/test-repo"
	sampleID  = "58cbecf4-11d9-4b9a-a22a-963cc72d6d16"
)

func TestRead(t *testing.T) {
	s := transcripttest.RoundTrip(t, Codec, transcripttest.Sample{Home: "testdata/home", CWD: sampleCWD, ID: sampleID})
	msgs := s.Messages()
	if len(msgs) != 3 || msgs[0].Role != transcript.RoleUser || msgs[1].Role != transcript.RoleAssistant || msgs[2].Role != transcript.RoleAssistant {
		t.Fatalf("messages = %+v", msgs)
	}
	if call := msgs[1].Content[0]; call.Kind != transcript.BlockToolUse || call.Name != "read_file" || string(call.Input) != `{"path":"README.md"}` {
		t.Errorf("tool call = %+v", call)
	}
	if msgs[2].Text() != "Hello from mock server." {
		t.Errorf("answer = %q", msgs[2].Text())
	}
}

func TestProjectDir(t *testing.T) {
	if got := ProjectDir("/home/ok/inference"); got != "home-ok-inference" {
		t.Errorf("ProjectDir = %q", got)
	}
}

func TestReadOnly(t *testing.T) {
	st, err := Codec.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.Write(t.Context(), &transcript.Session{CWD: "/tmp/p", Entries: []transcript.Entry{
		{Role: transcript.RoleUser, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "hi"}}},
	}})
	if err != transcript.ErrReadOnly {
		t.Errorf("Write err = %v, want ErrReadOnly", err)
	}
}
