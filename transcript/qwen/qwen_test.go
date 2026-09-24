package qwen

import (
	"testing"

	"github.com/inference-sh/agentprotocol/transcript"
	"github.com/inference-sh/agentprotocol/transcript/transcripttest"
)

// The sample is one ACP run of qwen-code 0.24.4 in the harness-test
// container: a prompt, a read_file call and its result, an answer, then
// /compress.
const (
	sampleCWD = "/tmp/harness-test-qwen-1472790582/test-repo"
	sampleID  = "0a3ff5d7-1017-48e2-bb72-a94e155bbd77"
)

func TestRoundTrip(t *testing.T) {
	s := transcripttest.RoundTrip(t, Codec, transcripttest.Sample{Home: "testdata/home", CWD: sampleCWD, ID: sampleID})
	if s.CWD != sampleCWD {
		t.Errorf("cwd = %q", s.CWD)
	}
	var call, result bool
	for _, e := range s.Messages() {
		for _, b := range e.Content {
			if b.Kind == transcript.BlockToolUse && b.Name == "read_file" && b.ToolID == "call_mock_1" {
				call = true
			}
			if b.Kind == transcript.BlockToolResult && b.ToolID == "call_mock_1" && b.Text == "test" && e.Role == transcript.RoleTool {
				result = true
			}
		}
	}
	if !call || !result {
		t.Errorf("tool call %v, result %v", call, result)
	}
	if s.Model != "gpt-4o-mini" {
		t.Errorf("model = %q", s.Model)
	}
}

func TestForeign(t *testing.T) { transcripttest.Foreign(t, Codec, "/tmp/some/project") }

func TestAppend(t *testing.T) {
	transcripttest.Append(t, Codec, transcripttest.Sample{Home: "testdata/home", CWD: sampleCWD, ID: sampleID})
}

func TestForeignIDs(t *testing.T) {
	transcripttest.ForeignIDs(t, Codec, "/tmp/some/project", transcript.IsUUID)
}

func TestListsCWD(t *testing.T) {
	transcripttest.ListsCWD(t, Codec, transcripttest.Sample{Home: "testdata/home", CWD: sampleCWD, ID: sampleID})
}

func TestImported(t *testing.T) {
	transcripttest.Imported(t, Codec, transcripttest.Sample{Home: "testdata/home", CWD: sampleCWD, ID: sampleID})
}
