package sqlite

import (
	"errors"
	"testing"

	"github.com/inference-sh/agentprotocol/transcript"
)

// The kiro v1 sample is one headless run (kiro-cli chat --no-interactive
// --agent-engine v1) in the harness-test container: a prompt, an fs_read
// call, its result, an answer.
const (
	kiroV1CWD = "/tmp/harness-test-kiro-3129754923/test-repo"
	kiroV1ID  = "eb5c1822-c2a9-4d10-8739-54e1f8e860d8"
)

func TestKiroV1Read(t *testing.T) {
	st, err := Kiro.Open("testdata/kiro")
	if err != nil {
		t.Fatal(err)
	}
	infos, err := st.List(t.Context(), kiroV1CWD)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].ID != kiroV1ID {
		t.Fatalf("list = %+v", infos)
	}
	s, err := st.Read(t.Context(), kiroV1ID)
	if err != nil {
		t.Fatal(err)
	}
	var roles []string
	for _, e := range s.Messages() {
		roles = append(roles, string(e.Role))
	}
	if got := join(roles); got != "user,assistant,tool,assistant" {
		t.Fatalf("roles = %s", got)
	}
	msgs := s.Messages()
	if msgs[0].Text() != "What is the project codename? Reply ONLY the codename." {
		t.Errorf("prompt = %q", msgs[0].Text())
	}
	if b := msgs[1].Content[0]; b.Kind != transcript.BlockToolUse || b.Name != "fs_read" || b.ToolID != "mock-tool-1" {
		t.Errorf("tool call = %+v", b)
	}
	if b := msgs[2].Content[0]; b.ToolID != "mock-tool-1" || b.Text != "test" || b.Status != transcript.StatusOK {
		t.Errorf("tool result = %+v", b)
	}
	if msgs[3].Text() != "Hello from mock server." || s.Model != "kiro-default" {
		t.Errorf("answer %q, model %q", msgs[3].Text(), s.Model)
	}
	if countTurns(s) == 0 {
		t.Error("no turn in events")
	}
	if _, err := st.Write(t.Context(), s); !errors.Is(err, transcript.ErrReadOnly) {
		t.Errorf("writing a v1 conversation: err = %v, want ErrReadOnly", err)
	}
}

func TestKiroRegistered(t *testing.T) {
	if _, ok := transcript.Registered("kiro"); !ok {
		t.Error("kiro not registered")
	}
}

func join(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += ","
		}
		out += s
	}
	return out
}
