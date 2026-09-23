package grok

import (
	"testing"

	"github.com/inference-sh/agentprotocol/transcript"
	"github.com/inference-sh/agentprotocol/transcript/transcripttest"
)

// The sample is one ACP run of Grok CLI in the harness-test container: a
// system prompt, context, a prompt, a mock read_file call, an answer.
const (
	sampleCWD = "/tmp/harness-test-grok-3224857346"
	sampleID  = "01a0cadd-6736-7120-a28a-90c4bbe16a34"
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
		t.Errorf("tool entries = %d", tools)
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
