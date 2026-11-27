package runner

import (
	"strings"
	"testing"

	"github.com/inference-sh/agentprotocol/conformance/server"
	"github.com/inference-sh/agentprotocol/harness"
)

func TestToolNames(t *testing.T) {
	long := strings.Repeat("x", 65)
	cases := []struct {
		name, body, want string
	}{
		{"none declared", `{"messages":[]}`, ""},
		{"portable", `{"tools":[{"type":"function","function":{"name":"Read"}},{"name":"mcp__srv__tool"}]}`, "pass"},
		{"too long", `{"tools":[{"name":"` + long + `"}]}`, "finding"},
		{"a dot", `{"tools":[{"functionDeclarations":[{"name":"srv.tool"}]}]}`, "finding"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := &TestRunner{harness: harness.All["claude"]}
			r.checkToolNames("acp", []server.LogEntry{{Body: []byte(c.body)}})
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
