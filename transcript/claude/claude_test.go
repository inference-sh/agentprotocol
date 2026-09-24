package claude

import (
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
