package cursor

import (
	"testing"

	"github.com/inference-sh/agentprotocol/transcript"
	"github.com/inference-sh/agentprotocol/transcript/transcripttest"
)

// The sample is one headless run of cursor-agent in the harness-test
// container, whose mock checkpoints the conversation the way Cursor's
// backend does (harness-test d7e0451): a prompt and an answer.
const (
	sampleCWD = "/tmp/harness-test-cursor-2307417833/test-repo"
	sampleID  = "1a92290c-00cf-4f0f-9ef8-66fc0572212d"
)

func TestRead(t *testing.T) {
	s := transcripttest.RoundTrip(t, Codec, transcripttest.Sample{Home: "testdata/home", CWD: sampleCWD, ID: sampleID})
	msgs := s.Messages()
	if len(msgs) != 2 || msgs[0].Role != transcript.RoleUser || msgs[1].Role != transcript.RoleAssistant {
		t.Fatalf("messages = %+v", msgs)
	}
	if msgs[1].Text() != "Hello from mock server." {
		t.Errorf("answer = %q", msgs[1].Text())
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
