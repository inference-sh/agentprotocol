package codex

import (
	"testing"

	"github.com/inference-sh/agentprotocol/transcript"
	"github.com/inference-sh/agentprotocol/transcript/transcripttest"
)

// The sample is one headless run of codex 0.137 in the harness-test
// container: instructions, a prompt, a mock exec_command call, an answer.
const (
	sampleCWD = "/tmp/harness-test-codex-643346103/test-repo"
	sampleID  = "01a0cad9-6894-7800-b1c5-44db37a059d9"
)

func TestRoundTrip(t *testing.T) {
	s := transcripttest.RoundTrip(t, Codec, transcripttest.Sample{Home: "testdata/home", CWD: sampleCWD, ID: sampleID})
	if s.CWD != sampleCWD {
		t.Errorf("cwd = %q", s.CWD)
	}
	var calls, results int
	for _, e := range s.Messages() {
		for _, b := range e.Content {
			switch b.Kind {
			case transcript.BlockToolUse:
				calls++
				if b.Name != "exec_command" || string(b.Input) != `{"cmd":"cat README.md"}` {
					t.Errorf("tool_use = %+v", b)
				}
			case transcript.BlockToolResult:
				results++
			}
		}
	}
	if calls != 1 || results != 1 {
		t.Errorf("tool calls %d, results %d", calls, results)
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
