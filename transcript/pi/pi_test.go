package pi

import (
	"testing"

	"github.com/inference-sh/agentprotocol/transcript"
	"github.com/inference-sh/agentprotocol/transcript/transcripttest"
)

// The sample is one ACP run of Oh My Pi in the harness-test container: a
// prompt and an answer. pi's own headless run wrote no session file, so its
// rows are covered through the fork, which shares them.
const (
	sampleCWD = "/tmp/harness-test-omp-511720626/test-repo"
	sampleID  = "01a0cadc-b646-7592-bbc4-c8c8d999cbed"
)

func TestRoundTripOMP(t *testing.T) {
	s := transcripttest.RoundTrip(t, OMP, transcripttest.Sample{Home: "testdata/home", CWD: sampleCWD, ID: sampleID})
	if s.CWD != sampleCWD {
		t.Errorf("cwd = %q", s.CWD)
	}
	msgs := s.Messages()
	if len(msgs) != 2 || msgs[0].Role != transcript.RoleUser || msgs[1].Role != transcript.RoleAssistant {
		t.Errorf("messages = %+v", msgs)
	}
}

func TestForeign(t *testing.T) {
	transcripttest.Foreign(t, Codec, "/tmp/some/project")
	transcripttest.Foreign(t, OMP, "/tmp/some/project")
}

func TestDashWrappedCwd(t *testing.T) {
	if got := transcript.DashWrappedCwd.Name("/home/ok/inference/go/cli"); got != "--home-ok-inference-go-cli--" {
		t.Errorf("got %q", got)
	}
}
