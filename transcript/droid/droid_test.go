package droid

import (
	"testing"

	"github.com/inference-sh/agentprotocol/transcript"
	"github.com/inference-sh/agentprotocol/transcript/transcripttest"
)

// The sample is one ACP run of droid in the harness-test container: a
// prompt, a mock Read call, an answer, then /compress.
const (
	sampleCWD = "/tmp/harness-test-droid-4202561597/test-repo"
	sampleID  = "ecabd2d0-b0bc-4f4a-b6c8-23992aa928c4"
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
		t.Errorf("tool entries = %d, want 1", tools)
	}
	if lin := s.Linearize(); len(lin) == 0 {
		t.Error("linearize returned nothing")
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
