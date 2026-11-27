package runner

import (
	"testing"

	"github.com/inference-sh/agentprotocol/conformance/server"
	"github.com/inference-sh/agentprotocol/harness"
)

func TestIsolation(t *testing.T) {
	cases := []struct {
		name, home, body, want string
	}{
		{"clean", "/tmp/harness-test-x", `{"messages":[{"content":"cwd /tmp/harness-test-x/test-repo"}]}`, "pass"},
		{"real home named", "/tmp/harness-test-x", `{"messages":[{"content":"from /home/dev/.claude/CLAUDE.md"}]}`, "finding:real-home"},
		{"a longer name sharing the prefix is not the home", "/tmp/harness-test-x", `{"content":"/home/developer/x"}`, "pass"},
		{"the agent's own install dir", "/tmp/harness-test-x", `{"content":"<location>/home/dev/.npm-global/lib/node_modules/@google/gemini-cli/bundle/SKILL.md</location>"}`, "pass"},
		{"install dir, then a real path", "/tmp/harness-test-x", `{"content":"/home/dev/.nvm/x and /home/dev/.gemini/GEMINI.md"}`, "finding:real-home"},
		{"home kept (PreserveHome)", "/home/dev", `{"content":"/home/dev/x"}`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := &TestRunner{harness: harness.Harness{Name: "x"}, home: c.home, testRealHome: "/home/dev"}
			r.checkIsolation("acp", []server.LogEntry{{Path: "/v1/messages", Body: []byte(c.body)}})
			got := ""
			if len(r.result.Checks) > 0 {
				got = r.result.Checks[0].Key()
			}
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}
