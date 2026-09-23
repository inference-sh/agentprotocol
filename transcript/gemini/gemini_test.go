package gemini

import (
	"testing"

	"github.com/inference-sh/agentprotocol/transcript"
	"github.com/inference-sh/agentprotocol/transcript/transcripttest"
)

// The sample is one ACP run of gemini-cli 0.60 in the harness-test
// container: session context, a prompt, a mock write_file call, an answer.
const (
	sampleCWD = "/tmp/harness-test-gemini-332863879/test-repo"
	sampleID  = "170f5754-4e70-4ac2-8bd8-22e062f69964"
)

func TestRoundTrip(t *testing.T) {
	s := transcripttest.RoundTrip(t, Codec, transcripttest.Sample{Home: "testdata/home", CWD: sampleCWD, ID: sampleID})
	var calls, results int
	for _, e := range s.Messages() {
		for _, b := range e.Content {
			switch b.Kind {
			case transcript.BlockToolUse:
				calls++
			case transcript.BlockToolResult:
				results++
			}
		}
	}
	if calls == 0 || results == 0 {
		t.Errorf("tool calls %d, results %d", calls, results)
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
